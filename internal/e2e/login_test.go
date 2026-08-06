package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// A browser: cookie jar, no bearer token. This is the human path — everything
// the reference client does, a third-party client can do identically.
type browser struct {
	t    *testing.T
	e    *env
	http *http.Client
}

func newBrowser(t *testing.T, e *env) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{t: t, e: e, http: &http.Client{Jar: jar}}
}

func (b *browser) do(method, path string, body any, out any) int {
	b.t.Helper()
	var rdr *strings.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = strings.NewReader(string(buf))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, b.e.url(path), rdr)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 400 {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

type loginFlow struct {
	FlowID  string `json:"flowId"`
	Kind    string `json:"kind"`
	Choices []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"choices"`
}

type principal struct {
	UserID    string `json:"userId"`
	TenantID  string `json:"tenantId"`
	Name      string `json:"name"`
	Method    string `json:"method"`
	Assurance string `json:"assurance"`
}

// signIn performs the whole human login: pick a name, get a cookie.
func (b *browser) signIn(name string) principal {
	b.t.Helper()
	var flow loginFlow
	if code := b.do("POST", "/v1/auth/login/begin", map[string]any{}, &flow); code != 200 {
		b.t.Fatalf("login/begin: HTTP %d", code)
	}
	for _, c := range flow.Choices {
		if c.Name != name {
			continue
		}
		var p principal
		code := b.do("POST", "/v1/auth/login/complete",
			map[string]any{"flowId": flow.FlowID, "params": map[string]string{"account": c.ID}}, &p)
		if code != 200 {
			b.t.Fatalf("login/complete: HTTP %d", code)
		}
		return p
	}
	b.t.Fatalf("account %q not offered; picker had %+v", name, flow.Choices)
	return principal{}
}

// AC-4 groundwork + D1: a human signs in by tapping a name, works entirely on
// the cookie, and their actions are attributed to their user id in the log.
func TestHumanLoginAndAttribution(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	e.admin("account", "new", "Anna", "--tenant", "t_default")
	e.start()

	b := newBrowser(t, e.env)
	p := b.signIn("Anna")
	if p.UserID == "" || p.Name != "Anna" {
		t.Fatalf("unexpected principal: %+v", p)
	}
	// The static driver is honest about what it proves.
	if p.Assurance != "attributed" || p.Method != "static" {
		t.Fatalf("static login must report attributed identity, got %+v", p)
	}

	// The cookie alone drives the product: create, converse, and the run
	// completes — no bearer token anywhere.
	var proj struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
	}
	if code := b.do("POST", "/v1/projects", map[string]string{"name": "annas site"}, &proj); code != 201 {
		t.Fatalf("create project as human: HTTP %d", code)
	}
	req, _ := http.NewRequest("POST", e.url("/v1/sessions/"+proj.SessionID+"/messages"),
		strings.NewReader(`{"text":"build me a site"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "k1")
	resp, err := b.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("say as human: HTTP %d", resp.StatusCode)
	}

	// R-SEC-2: the user's message carries their user id as actor; events the
	// platform emits on its own behalf do not.
	var evs []struct {
		Type  string `json:"type"`
		Actor string `json:"actor"`
	}
	deadline := 0
	for deadline < 300 {
		var got []struct {
			Type  string `json:"type"`
			Actor string `json:"actor"`
		}
		b.do("GET", "/v1/sessions/"+proj.SessionID+"/events?stream=false", nil, &got)
		evs = got
		done := false
		for _, ev := range got {
			if ev.Type == "run.completed" {
				done = true
			}
		}
		if done {
			break
		}
		deadline++
	}
	var sawUserMessage bool
	for _, ev := range evs {
		if ev.Type == "user.message" {
			sawUserMessage = true
			if ev.Actor != p.UserID {
				t.Fatalf("user.message actor = %q, want %q", ev.Actor, p.UserID)
			}
		}
		if ev.Type == "run.started" && ev.Actor != "" {
			t.Fatalf("platform event carries an actor: %+v", ev)
		}
	}
	if !sawUserMessage {
		t.Fatal("no user.message event found")
	}

	// Signing out kills the cookie.
	b.do("POST", "/v1/auth/logout", nil, nil)
	if code := b.do("GET", "/v1/projects", nil, nil); code != 401 {
		t.Fatalf("after logout: HTTP %d (want 401)", code)
	}
}

// AC-5 groundwork: a second device signing in as the same account sees the
// same projects — resume is a property of the log, not the device.
func TestSecondDeviceSeesSameProjects(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	e.admin("account", "new", "Anna", "--tenant", "t_default")
	e.start()

	laptop := newBrowser(t, e.env)
	laptop.signIn("Anna")
	var proj struct {
		ID string `json:"id"`
	}
	laptop.do("POST", "/v1/projects", map[string]string{"name": "shared site"}, &proj)

	phone := newBrowser(t, e.env) // separate cookie jar = a different device
	phone.signIn("Anna")
	var projects []struct {
		ID string `json:"id"`
	}
	if code := phone.do("GET", "/v1/projects", nil, &projects); code != 200 {
		t.Fatalf("phone list: HTTP %d", code)
	}
	found := false
	for _, p := range projects {
		if p.ID == proj.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("second device cannot see the project created on the first: %+v", projects)
	}
}

// Hostile: a session cookie is scoped exactly like a token — a foreign
// resource is indistinguishable from a missing one, and revocation is
// immediate.
func TestSessionScopingAndRevocation(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	// Accounts live in the default tenant; the attacker is a separate tenant
	// with an API token (the T2-shaped case static login refuses to serve).
	out := e.admin("account", "new", "Anna", "--tenant", "t_default")
	annaID := strings.Fields(out)[1]
	_, attackerToken := e.newTenantToken(t, "attacker")
	e.start()

	attackerSession := createProject(t, e.env, attackerToken, "attacker project")

	b := newBrowser(t, e.env)
	b.signIn("Anna")
	// Anna cannot reach the attacker tenant's session: 404, never 403.
	if code := b.do("GET", "/v1/sessions/"+attackerSession+"/events?stream=false", nil, nil); code != 404 {
		t.Fatalf("cross-tenant session via cookie: HTTP %d (want 404)", code)
	}

	// Disabling the account revokes live sessions immediately.
	e.admin("account", "disable", annaID)
	if code := b.do("GET", "/v1/projects", nil, nil); code != 401 {
		t.Fatalf("disabled account still authenticated: HTTP %d (want 401)", code)
	}
	// And the picker no longer offers the name.
	var flow loginFlow
	b.do("POST", "/v1/auth/login/begin", map[string]any{}, &flow)
	for _, c := range flow.Choices {
		if c.Name == "Anna" {
			t.Fatal("disabled account still offered by the picker")
		}
	}
}
