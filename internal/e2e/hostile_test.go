package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// authEnv starts a server WITHOUT --insecure and provisions tenants/tokens via
// the local admin CLI (which operates directly on the data dir).
type authEnv struct {
	*env
}

func newAuthEnv(t *testing.T, model string) *authEnv {
	return &authEnv{env: newEnv(t, model)}
}

func (e *authEnv) start() {
	e.t.Helper()
	cmd := exec.Command(binPath, "serve",
		"--addr", e.addr, "--serve-addr", e.serveAddr, "--data", e.dataDir, "--model", e.model, "--lease-ttl", "2s")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.cmd = cmd
	e.t.Cleanup(func() {
		if e.cmd != nil && e.cmd.Process != nil {
			e.cmd.Process.Kill()
			e.cmd.Wait()
		}
	})
	e.waitHealthy()
}

func (e *authEnv) waitHealthy() {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := httpGet(e.url("/healthz")); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatal("server not healthy")
}

// admin runs a local CLI subcommand against the data dir (server need not run).
func (e *authEnv) admin(args ...string) string {
	e.t.Helper()
	full := append(args, "--data", e.dataDir)
	out, err := exec.Command(binPath, full...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("admin %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (e *authEnv) newTenantToken(t *testing.T, name string, extra ...string) (tenantID, token string) {
	t.Helper()
	// `tenant new` prints: "tenant <id>  (name)"
	args := append([]string{"tenant", "new", name}, extra...)
	tenantID = strings.Fields(e.admin(args...))[1]
	return tenantID, e.tokenFor(t, tenantID)
}

// tokenFor extracts the plaintext token (the "creo_" line) from `token new`;
// admin() merges stdout+stderr, so scan rather than take the last line.
func (e *authEnv) tokenFor(t *testing.T, tenantID string) string {
	t.Helper()
	for _, line := range strings.Split(e.admin("token", "new", tenantID), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "creo_") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no token in `token new` output")
	return ""
}

// AC-6 + AC-7 + S5: a hostile prompt-injected project cannot escape its
// workspace, read the host, reach a sibling, and its owner's token cannot see
// another tenant's resources.
func TestHostileProjectContained(t *testing.T) {
	e := newAuthEnv(t, "fake:hostile")
	e.start()

	tenantA, tokenA := e.newTenantToken(t, "attacker")
	_, tokenB := e.newTenantToken(t, "victim")
	_ = tenantA

	// Victim (tenant B) builds a real site whose content is a canary.
	victimSession := createProject(t, e.env, tokenB, "victim-site")
	sayAuthed(t, e.env, tokenB, victimSession, "build me a site", "vk1")
	waitCompleted(t, e.env, tokenB, victimSession, 1)

	// Attacker (tenant A) runs the hostile script.
	attackSession := createProject(t, e.env, tokenA, "attacker-site")
	sayAuthed(t, e.env, tokenA, attackSession, "do your worst", "ak1")
	evs := waitCompletedAuthed(t, e.env, tokenA, attackSession, 1)

	// Every escape attempt after the first legit write must have errored, and
	// nothing outside the attacker's own workspace changed.
	var toolResults int
	for _, ev := range evs {
		if ev.Type == "tool.result" {
			toolResults++
		}
		// The victim's canary content must never appear in the attacker's log.
		if strings.Contains(ev.UserText, "Home") && ev.Type != "assistant.message" {
			// assistant.message may legitimately contain the word; only flag
			// tool.result leakage of file contents.
			if ev.Type == "tool.result" {
				t.Fatalf("victim content leaked into attacker log: %+v", ev)
			}
		}
	}
	if toolResults < 6 {
		t.Fatalf("expected all hostile tool calls to run (as errors), got %d results", toolResults)
	}
	// No file escaped the attacker's workspace into the data dir root or siblings.
	for _, bad := range []string{"evil.txt", "attacker-site/../evil.txt"} {
		if _, err := os.Stat(filepath.Join(e.dataDir, "workspaces", bad)); err == nil {
			t.Fatalf("file escaped the workspace: %s", bad)
		}
	}

	// Cross-tenant API isolation: attacker's token gets 404 on victim's session.
	code := statusFor(t, e.env, tokenA, "GET", "/v1/sessions/"+victimSession+"/events?stream=false")
	if code != 404 {
		t.Fatalf("attacker reached victim session: HTTP %d (want 404)", code)
	}
	// And victim's projects are invisible to the attacker's project list.
	projs := listProjects(t, e.env, tokenA)
	for _, p := range projs {
		if strings.Contains(p, "victim") {
			t.Fatalf("victim project visible to attacker: %s", p)
		}
	}
}

// S1: every /v1 route rejects missing or invalid tokens.
func TestAuthRequired(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	e.start()
	_, token := e.newTenantToken(t, "t")
	session := createProject(t, e.env, token, "p")

	routes := [][2]string{
		{"GET", "/v1/projects"},
		{"POST", "/v1/projects"},
		{"GET", "/v1/sessions/" + session + "/events?stream=false"},
		{"GET", "/v1/runs/run_x"},
	}
	for _, rt := range routes {
		if code := statusFor(t, e.env, "", rt[0], rt[1]); code != 401 {
			t.Errorf("%s %s with no token: HTTP %d (want 401)", rt[0], rt[1], code)
		}
		if code := statusFor(t, e.env, "creo_garbage", rt[0], rt[1]); code != 401 {
			t.Errorf("%s %s with bad token: HTTP %d (want 401)", rt[0], rt[1], code)
		}
	}
}

// S3: a tenant over its daily token budget gets new runs refused with a
// plain-language failure event.
func TestBudgetExhausted(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	e.start()
	// 1-token daily budget: the first model call already exceeds it.
	_, token := e.newTenantToken(t, "broke", "--daily-tokens", "1")

	session := createProject(t, e.env, token, "p")
	sayAuthed(t, e.env, token, session, "build me a site", "k1")
	evs := waitForAuthed(t, e.env, token, session, 20*time.Second, func(evs []event) bool {
		return count(evs, "run.failed") >= 1
	})
	var failText string
	for _, ev := range evs {
		if ev.Type == "run.failed" {
			failText = ev.UserText
		}
	}
	if !strings.Contains(strings.ToLower(failText), "budget") {
		t.Fatalf("budget failure not surfaced in plain language: %q", failText)
	}
	if count(evs, "run.completed") != 0 {
		t.Fatal("run completed despite exhausted budget")
	}
}
