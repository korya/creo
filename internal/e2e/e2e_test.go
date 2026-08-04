// Package e2e runs the M0 acceptance tests against the real `creo` binary:
// AC-1 kill -9 mid-run and resume, AC-2 workspace loss, AC-3 duplicate submit.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "creo-e2e-bin")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	binPath = filepath.Join(dir, "creo")
	build := exec.Command("go", "build", "-o", binPath, "github.com/korya/creo/cmd/creo")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type env struct {
	t       *testing.T
	dataDir string
	addr    string
	model   string
	cmd     *exec.Cmd
}

func newEnv(t *testing.T, model string) *env {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return &env{t: t, dataDir: t.TempDir(), addr: addr, model: model}
}

func (e *env) start() {
	e.t.Helper()
	// Existing M0 acceptance tests run in --insecure mode (unauthenticated
	// requests map to t_default). Auth and cross-tenant isolation get their
	// own coverage in hostile_test.go.
	cmd := exec.Command(binPath, "serve",
		"--addr", e.addr, "--data", e.dataDir, "--model", e.model, "--lease-ttl", "2s", "--insecure")
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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(e.url("/healthz"))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatal("server did not become healthy")
}

func (e *env) sigkill() {
	e.t.Helper()
	if err := e.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		e.t.Fatal(err)
	}
	e.cmd.Wait()
	e.cmd = nil
}

func (e *env) url(path string) string { return "http://" + e.addr + path }

func (e *env) call(method, path string, body any, headers map[string]string, out any) {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(method, e.url(path), &buf)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		e.t.Fatalf("%s %s -> %s", method, path, resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatal(err)
		}
	}
}

type event struct {
	Seq      int64  `json:"seq"`
	Type     string `json:"type"`
	UserText string `json:"userText"`
}

func (e *env) events(sessionID string) []event {
	e.t.Helper()
	var evs []event
	e.call("GET", "/v1/sessions/"+sessionID+"/events?stream=false", nil, nil, &evs)
	return evs
}

func (e *env) waitFor(sessionID string, timeout time.Duration, pred func([]event) bool) []event {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs := e.events(sessionID)
		if pred(evs) {
			return evs
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("condition not reached within %s; last events: %+v", timeout, e.events(sessionID))
	return nil
}

func count(evs []event, typ string) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

func (e *env) newProject(name string) (projectID, sessionID string) {
	var p struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
	}
	e.call("POST", "/v1/projects", map[string]string{"name": name}, nil, &p)
	return p.ID, p.SessionID
}

func (e *env) say(sessionID, text, key string) (runID string, deduped bool) {
	var out struct {
		RunID   string `json:"runId"`
		Deduped bool   `json:"deduped"`
	}
	e.call("POST", "/v1/sessions/"+sessionID+"/messages",
		map[string]string{"text": text}, map[string]string{"Idempotency-Key": key}, &out)
	return out.RunID, out.Deduped
}

// AC-1: kill -9 the server mid-run; after restart the run resumes from the
// log and completes, with a gapless sequence and exactly one resume marker.
func TestKillDashNineAndResume(t *testing.T) {
	e := newEnv(t, "fake:slow-site")
	e.start()
	projectID, sessionID := e.newProject("kill-test")
	runID, _ := e.say(sessionID, "build me a big site", "k1")

	// Wait until the run has demonstrably made partial progress, then kill.
	e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "tool.result") >= 3
	})
	e.sigkill()

	e.start() // same data dir: boot recovery must find the orphaned run
	evs := e.waitFor(sessionID, 60*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") == 1
	})

	if got := count(evs, "run.resumed"); got != 1 {
		t.Fatalf("want exactly 1 run.resumed, got %d", got)
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("sequence gap at index %d: seq %d", i, ev.Seq)
		}
	}
	var r struct{ Status string }
	e.call("GET", "/v1/runs/"+runID, nil, nil, &r)
	if r.Status != "completed" {
		t.Fatalf("run status after resume: %s", r.Status)
	}
	var versions []struct{ ID string }
	e.call("GET", "/v1/projects/"+projectID+"/versions", nil, nil, &versions)
	if len(versions) != 1 {
		t.Fatalf("want 1 committed version, got %d", len(versions))
	}
	// All eight pages of the script exist: the resumed run continued, not restarted.
	for i := 1; i <= 8; i++ {
		p := filepath.Join(e.dataDir, "workspaces", projectID, fmt.Sprintf("page%d.html", i))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s after resume: %v", p, err)
		}
	}
}

// AC-3: submitting the same message twice creates exactly one run.
func TestDuplicateSubmit(t *testing.T) {
	e := newEnv(t, "fake:site")
	e.start()
	_, sessionID := e.newProject("dup-test")

	run1, dedup1 := e.say(sessionID, "build me a site", "same-key")
	run2, dedup2 := e.say(sessionID, "build me a site", "same-key")
	if dedup1 || !dedup2 || run1 != run2 {
		t.Fatalf("dedup broken: run1=%s(%v) run2=%s(%v)", run1, dedup1, run2, dedup2)
	}
	evs := e.waitFor(sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})
	if got := count(evs, "user.message"); got != 1 {
		t.Fatalf("duplicate submit appended %d user messages", got)
	}
	if got := count(evs, "run.started"); got != 1 {
		t.Fatalf("duplicate submit started %d runs", got)
	}
}

// AC-2: destroy the workspace directory; the next run rebuilds it from the
// last committed version before doing anything else.
func TestWorkspaceLoss(t *testing.T) {
	e := newEnv(t, "fake:site")
	e.start()
	projectID, sessionID := e.newProject("loss-test")
	e.say(sessionID, "build me a site", "k1")
	e.waitFor(sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") == 1
	})

	wsDir := filepath.Join(e.dataDir, "workspaces", projectID)
	if err := os.RemoveAll(wsDir); err != nil {
		t.Fatal(err)
	}
	e.say(sessionID, "anything else?", "k2")
	e.waitFor(sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") == 2
	})
	// The first run's files are back — materialized from the committed version.
	if _, err := os.Stat(filepath.Join(wsDir, "index.html")); err != nil {
		t.Fatalf("workspace not rebuilt from version: %v", err)
	}
}
