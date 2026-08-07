package e2e

import (
	"strings"
	"testing"
	"time"
)

// Issue #4: a model that forgets the home page gets told, fixes it, and the
// user sees a finished site — no failure, and one quiet line about the time.
func TestUnservableBuildIsRepairedSilently(t *testing.T) {
	e := newAuthEnv(t, "fake:repairs-site")
	e.start()
	_, token := e.newTenantToken(t, "acme")
	projectID, sessionID := e.projectIDFor(t, token, "bakery")

	sayAuthed(t, e.env, token, sessionID, "build me a bakery site", "k1")
	evs := waitCompletedAuthed(t, e.env, token, sessionID, 1)

	if n := count(evs, "run.failed"); n != 0 {
		t.Fatalf("a repaired run surfaced %d failure(s)", n)
	}
	if n := count(evs, "repair.started"); n != 1 {
		t.Fatalf("want 1 repair.started, got %d", n)
	}
	if n := count(evs, "repair.completed"); n != 1 {
		t.Fatalf("want 1 repair.completed, got %d", n)
	}
	for _, ev := range evs {
		switch ev.Type {
		case "repair.started":
			if ev.UserText != "" {
				t.Fatalf("repair.started must render nothing: %q", ev.UserText)
			}
		case "repair.completed":
			assertPlainLanguage(t, "repair acknowledgment", ev.UserText)
			if strings.Contains(strings.ToLower(ev.UserText), "home page") {
				t.Fatalf("acknowledgment names the artifact: %q", ev.UserText)
			}
		}
	}

	// The whole point: what the run produced is something a visitor can open.
	assertServable(t, e.env, token, projectID)
}

// When repair cannot help, the platform says so once, in plain language, and
// leaves nothing behind for anyone to publish.
func TestUnservableBuildFailsHonestly(t *testing.T) {
	e := newAuthEnv(t, "fake:no-page")
	e.start()
	_, token := e.newTenantToken(t, "acme")
	projectID, sessionID := e.projectIDFor(t, token, "bakery")

	sayAuthed(t, e.env, token, sessionID, "build me a bakery site", "k1")
	evs := waitForAuthed(t, e.env, token, sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.failed") >= 1
	})

	if n := count(evs, "run.completed"); n != 0 {
		t.Fatalf("an unservable run reported completion %d time(s)", n)
	}
	if n := count(evs, "artifact.version.created"); n != 0 {
		t.Fatalf("an unservable run minted %d version(s)", n)
	}
	var failures []string
	for _, ev := range evs {
		if ev.Type == "run.failed" && ev.UserText != "" {
			failures = append(failures, ev.UserText)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("want exactly one failure message, got %d: %v", len(failures), failures)
	}
	assertPlainLanguage(t, "unservable failure", failures[0])

	// With no version, the user is told there is nothing to show rather than
	// being handed a 404 — and Publish is refused for the same reason.
	resp := doAuthed(t, e.env, token, "GET", "/v1/projects/"+projectID+"/preview", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("preview with nothing built: HTTP %d (want 400)", resp.StatusCode)
	}
	pub := doAuthed(t, e.env, token, "POST", "/v1/projects/"+projectID+"/publish", map[string]string{}, nil)
	defer pub.Body.Close()
	if pub.StatusCode != 400 {
		t.Fatalf("publish with nothing built: HTTP %d (want 400)", pub.StatusCode)
	}
}

// The repair budget lives in the log, so killing the server mid-repair does
// not hand the model a fresh set of attempts on resume.
func TestRepairBudgetSurvivesRestart(t *testing.T) {
	e := newAuthEnv(t, "fake:no-page")
	e.start()
	_, token := e.newTenantToken(t, "acme")
	_, sessionID := e.projectIDFor(t, token, "bakery")

	sayAuthed(t, e.env, token, sessionID, "build me a bakery site", "k1")
	waitForAuthed(t, e.env, token, sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "repair.started") >= 1
	})

	e.sigkill()
	e.start()

	evs := waitForAuthed(t, e.env, token, sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.failed") >= 1
	})
	// Two is the budget. A resume that reset it would show more, and the run
	// would keep burning turns on a model that cannot produce a page.
	if n := count(evs, "repair.started"); n > 2 {
		t.Fatalf("resume granted extra repair attempts: %d total", n)
	}
	if n := count(evs, "artifact.version.created"); n != 0 {
		t.Fatalf("an unservable run minted %d version(s) across a restart", n)
	}
}

// Parking on a question before any page exists is legitimate — the run must
// still wait for input rather than failing, and must mint nothing.
func TestQuestionParksBeforeAnythingIsServable(t *testing.T) {
	e := newAuthEnv(t, "fake:asks-before-page")
	e.start()
	_, token := e.newTenantToken(t, "acme")
	projectID, sessionID := e.projectIDFor(t, token, "bakery")

	sayAuthed(t, e.env, token, sessionID, "build me a bakery site", "k1")
	evs := waitForAuthed(t, e.env, token, sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "input.requested") >= 1
	})
	if n := count(evs, "run.failed"); n != 0 {
		t.Fatalf("parking before a page exists failed the run (%d)", n)
	}
	if n := count(evs, "artifact.version.created"); n != 0 {
		t.Fatalf("parking minted %d unservable version(s)", n)
	}

	// Answering lets it finish, and what it finishes with is servable.
	var st map[string]any
	e.call("GET", "/v1/sessions/"+sessionID, nil, map[string]string{"Authorization": "Bearer " + token}, &st)
	runID, _ := st["runId"].(string)
	if runID == "" {
		t.Fatal("no runId in session state")
	}
	resp := doAuthed(t, e.env, token, "POST", "/v1/runs/"+runID+"/input", map[string]string{"text": "Weekdays 9-5"}, nil)
	resp.Body.Close()
	waitCompletedAuthed(t, e.env, token, sessionID, 1)
	assertServable(t, e.env, token, projectID)
}
