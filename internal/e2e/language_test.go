package e2e

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// What must never reach a non-coder's screen (R-AGT-3: "No error codes,
// ever, on the primary surface").
//
// jargonWords are matched on word boundaries — "lease" must not fire on
// "Please", which is how the first version of this test lied to us.
var jargonWords = []string{
	"sql", "sqlite", "goroutine", "panic", "nil", "exception", "stack",
	"traceback", "unmarshal", "eof", "blob", "sha256", "lease", "idempotency",
	"header", "tenant", "unknown", "invalid", "unsupported", "malformed",
	"forbidden", "unauthorized", "null", "runid", "sessionid",
}

// jargonPhrases are matched as substrings: symbols and API-contract voice that
// cannot appear innocently inside an ordinary sentence.
var jargonPhrases = []string{
	"err=", "error:", "0x", "json:", "/users/", "/var/", "http 5", "500 ",
	"context deadline", "connection refused", "tenant_id", "run_id",
	"must be", "is required", "no version", "not found", "bad request",
}

var jargonRe = func() *regexp.Regexp {
	return regexp.MustCompile(`\b(` + strings.Join(jargonWords, "|") + `)\b`)
}()

func assertPlainLanguage(t *testing.T, where, text string) {
	t.Helper()
	if strings.TrimSpace(text) == "" {
		// An empty string is not a pass. A silent failure is its own R-AGT-3
		// violation, and it is how this very test once passed vacuously.
		t.Errorf("%s produced no message at all", where)
		return
	}
	lower := strings.ToLower(text)
	if m := jargonRe.FindString(lower); m != "" {
		t.Errorf("%s leaks technical detail (%q): %q", where, m, text)
	}
	for _, bad := range jargonPhrases {
		if strings.Contains(lower, bad) {
			t.Errorf("%s leaks technical detail (%q): %q", where, bad, text)
		}
	}
	// A sentence, not an identifier.
	if !strings.Contains(text, " ") {
		t.Errorf("%s is not a sentence: %q", where, text)
	}
}

// AC-12: every failure path a non-coder can reach ends in plain language. This
// walks the paths a real person hits — publishing nothing, restoring past the
// beginning, an expired sign-in, a stopped build — and reads what they'd see.
func TestUserFacingErrorsAreSentences(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	e.admin("account", "new", "Anna", "--tenant", "t_default")
	e.start()

	b := newBrowser(t, e.env)
	b.signIn("Anna")
	var proj struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
	}
	b.do("POST", "/v1/projects", map[string]string{"name": "bakery"}, &proj)

	errorText := func(method, path string, body any) string {
		var out struct {
			Error string `json:"error"`
		}
		b.do(method, path, body, &out)
		return out.Error
	}

	// Publishing, previewing, and downloading before anything exists.
	assertPlainLanguage(t, "publish with nothing built",
		errorText("POST", "/v1/projects/"+proj.ID+"/publish", map[string]string{}))
	assertPlainLanguage(t, "preview with nothing built",
		errorText("GET", "/v1/projects/"+proj.ID+"/preview", nil))
	assertPlainLanguage(t, "export with nothing built",
		errorText("GET", "/v1/projects/"+proj.ID+"/export", nil))

	// Restoring past the first version.
	b.say(proj.SessionID, "build me a bakery site", "k1")
	b.waitFor(proj.SessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})
	b.do("POST", "/v1/projects/"+proj.ID+"/publish", map[string]string{}, nil)
	assertPlainLanguage(t, "rollback past the beginning",
		errorText("POST", "/v1/projects/"+proj.ID+"/rollback", map[string]string{}))

	// A link to something that is gone.
	assertPlainLanguage(t, "unknown project", errorText("GET", "/v1/projects/p_gone/preview", nil))
	assertPlainLanguage(t, "unknown session", errorText("GET", "/v1/sessions/s_gone", nil))

	// An expired sign-in, which every long-lived device eventually hits.
	stale := newBrowser(t, e.env)
	assertPlainLanguage(t, "not signed in", errorText2(t, stale, "GET", "/v1/projects"))
}

func errorText2(t *testing.T, b *browser, method, path string) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	b.do(method, path, nil, &out)
	return out.Error
}

// Everything the platform emits into a transcript is read aloud by the
// product. This walks a whole ordinary session — build, question, answer,
// publish, stop — and holds every userText to the same bar.
func TestEveryEmittedUserTextIsPlainLanguage(t *testing.T) {
	e := newEnv(t, "fake:asking-site")
	e.start()
	_, sessionID := e.newProject("bakery")
	e.say(sessionID, "build me a bakery site", "k1")
	e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "input.requested") >= 1
	})
	var st map[string]any
	e.call("GET", "/v1/sessions/"+sessionID, nil, nil, &st)
	runID, _ := st["runId"].(string)
	e.call("POST", "/v1/runs/"+runID+"/input", map[string]string{"text": "Weekdays 9-5"}, nil, nil)
	evs := e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})

	seen := 0
	for _, ev := range evs {
		if ev.UserText == "" {
			continue
		}
		seen++
		assertPlainLanguage(t, "event "+ev.Type, ev.UserText)
	}
	if seen < 4 {
		t.Fatalf("only %d user-facing messages in a full session — the walk is too shallow to prove anything", seen)
	}
}

// The escalation ladder (R-AGT-3): a failure the platform cannot fix ends in
// ONE message, and that message says what the user can do — never a code, and
// never a second competing error for the same failure.
func TestFailureEndsInOneActionableMessage(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	e.start()
	_, token := e.newTenantToken(t, "broke", "--daily-tokens", "1")
	session := createProject(t, e.env, token, "p")
	sayAuthed(t, e.env, token, session, "build me a site", "k1")

	evs := waitForAuthed(t, e.env, token, session, 20*time.Second, func(evs []event) bool {
		return count(evs, "run.failed") >= 1
	})
	var messages []string
	for _, ev := range evs {
		if ev.Type == "run.failed" && ev.UserText != "" {
			messages = append(messages, ev.UserText)
		}
	}
	if len(messages) != 1 {
		t.Fatalf("want exactly one failure message, got %d: %v", len(messages), messages)
	}
	assertPlainLanguage(t, "budget failure", messages[0])
	// Actionable: it tells them what to do next, not merely that it broke.
	lower := strings.ToLower(messages[0])
	if !strings.Contains(lower, "try again") && !strings.Contains(lower, "ask ") {
		t.Fatalf("failure message offers no way forward: %q", messages[0])
	}

	// The technical cause is retained for the operator on the same event —
	// two audiences, one log (architecture.md §3.1).
	for _, ev := range evs {
		if ev.Type != "run.failed" {
			continue
		}
		var detail map[string]any
		json.Unmarshal(ev.Detail, &detail)
		if detail["error"] == nil || detail["error"] == "" {
			t.Fatal("failure event kept no technical detail for diagnostics")
		}
	}
}
