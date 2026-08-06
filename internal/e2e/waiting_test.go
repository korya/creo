package e2e

import (
	"encoding/json"
	"testing"
	"time"
)

func lastOfType(evs []event, typ string) (event, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == typ {
			return evs[i], true
		}
	}
	return event{}, false
}

// states extracts the session states the client was told to render, in order.
func states(t *testing.T, evs []event) []string {
	t.Helper()
	var out []string
	for _, e := range evs {
		if e.Type != "session.state.changed" {
			continue
		}
		var d struct {
			State string `json:"state"`
		}
		json.Unmarshal(e.Detail, &d)
		out = append(out, d.State)
	}
	return out
}

func sessionState(t *testing.T, e *env, sessionID string) map[string]any {
	t.Helper()
	var out map[string]any
	e.call("GET", "/v1/sessions/"+sessionID, nil, nil, &out)
	return out
}

// AC-5: the agent asks a question, the run parks, a SECOND device answers it,
// and the run resumes to completion. The question survives on the log, so it
// is answerable from anywhere — not just from the device that saw it asked.
func TestQuestionAnsweredFromSecondDevice(t *testing.T) {
	e := newEnv(t, "fake:asking-site")
	e.start()
	_, sessionID := e.newProject("bakery")
	e.say(sessionID, "build me a bakery site", "k1")

	evs := e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "input.requested") >= 1
	})
	q, _ := lastOfType(evs, "input.requested")
	if q.UserText != "What are your opening hours?" {
		t.Fatalf("question text not carried verbatim: %q", q.UserText)
	}
	var qd struct {
		ToolID  string   `json:"toolId"`
		Choices []string `json:"choices"`
	}
	json.Unmarshal(q.Detail, &qd)
	if len(qd.Choices) != 2 {
		t.Fatalf("choices not surfaced to clients: %+v", qd)
	}

	// The session reports its state explicitly, with the pending question — a
	// device arriving now renders it without replaying the log (R-SES-5).
	st := sessionState(t, e, sessionID)
	if st["state"] != "waiting-for-input" {
		t.Fatalf("session state = %v, want waiting-for-input", st["state"])
	}
	if st["question"] == nil {
		t.Fatal("pending question missing from session state")
	}

	// The work done before the question is already saved: pausing never costs
	// the user what the agent already built.
	if count(evs, "artifact.version.created") < 1 {
		t.Fatal("no version committed before parking on the question")
	}

	// A second device answers, addressing the run it never started.
	runID, _ := st["runId"].(string)
	if runID == "" {
		t.Fatal("session state carried no runId")
	}
	var ans map[string]any
	e.call("POST", "/v1/runs/"+runID+"/input", map[string]string{"text": "Weekdays 9–5"}, nil, &ans)

	final := e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})
	if got := count(final, "run.started"); got != 1 {
		t.Fatalf("answering restarted the conversation: %d run.started", got)
	}
	if got := count(final, "input.provided"); got != 1 {
		t.Fatalf("want 1 input.provided, got %d", got)
	}
	// The whole exchange is one run: paused, not abandoned.
	if got := count(final, "run.completed"); got != 1 {
		t.Fatalf("want 1 run.completed, got %d", got)
	}

	got := states(t, final)
	want := []string{"queued", "working", "waiting-for-input", "queued", "working", "idle"}
	if len(got) != len(want) {
		t.Fatalf("state sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state sequence = %v, want %v", got, want)
		}
	}
}

// A plain message typed while a question is pending IS the answer. Without
// this the reply would queue a run that RC-2 could never claim, and the user
// would watch their words disappear.
func TestTypedReplyAnswersThePendingQuestion(t *testing.T) {
	e := newEnv(t, "fake:asking-site")
	e.start()
	_, sessionID := e.newProject("bakery")
	e.say(sessionID, "build me a bakery site", "k1")
	e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "input.requested") >= 1
	})

	// The user just types, as they would in any chat.
	e.say(sessionID, "we open at seven", "k2")

	final := e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})
	if got := count(final, "input.provided"); got != 1 {
		t.Fatalf("typed reply did not answer the question: %d input.provided", got)
	}
	if got := count(final, "run.started"); got != 1 {
		t.Fatalf("typed reply started a second run: %d run.started", got)
	}
	answer, _ := lastOfType(final, "input.provided")
	if answer.UserText != "we open at seven" {
		t.Fatalf("answer text = %q", answer.UserText)
	}
}

// A parked question outlives the process (R-SES-1 applied to waiting): kill
// the server mid-question, restart, and the answer still lands.
func TestQuestionSurvivesRestart(t *testing.T) {
	e := newEnv(t, "fake:asking-site")
	e.start()
	_, sessionID := e.newProject("bakery")
	e.say(sessionID, "build me a bakery site", "k1")
	e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "input.requested") >= 1
	})

	e.sigkill()
	e.start()

	st := sessionState(t, e, sessionID)
	if st["state"] != "waiting-for-input" {
		t.Fatalf("after restart, state = %v, want waiting-for-input", st["state"])
	}
	runID, _ := st["runId"].(string)
	e.call("POST", "/v1/runs/"+runID+"/input", map[string]string{"text": "Weekdays 9–5"}, nil, nil)

	final := e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})
	if got := count(final, "run.completed"); got != 1 {
		t.Fatalf("want 1 run.completed after restart, got %d", got)
	}
}

// R-RUN-4: the user stops a build in progress. The run ends cancelled, the
// last committed version stands, and the project accepts new work.
func TestCancelMidBuild(t *testing.T) {
	e := newEnv(t, "fake:slow-asking-site")
	e.start()
	_, sessionID := e.newProject("bakery")
	e.say(sessionID, "build me a bakery site", "k1")

	// Wait until the agent is genuinely working.
	e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "tool.result") >= 1
	})
	st := sessionState(t, e, sessionID)
	runID, _ := st["runId"].(string)
	e.call("POST", "/v1/runs/"+runID+"/cancel", nil, nil, nil)

	final := e.waitFor(sessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "run.cancelled") >= 1
	})
	// Stopping is not failing: no failure ever reaches the user.
	if got := count(final, "run.failed"); got != 0 {
		t.Fatalf("cancel surfaced as a failure %d time(s)", got)
	}
	if got := count(final, "run.completed"); got != 0 {
		t.Fatalf("cancelled run also completed (%d)", got)
	}
	cancelled, _ := lastOfType(final, "run.cancelled")
	if cancelled.UserText == "" {
		t.Fatal("cancel event carries no plain-language text")
	}
	if s := sessionState(t, e, sessionID)["state"]; s != "idle" {
		t.Fatalf("after cancel, state = %v, want idle", s)
	}

	// Nothing the cancelled worker did after the stop reached the log: the
	// event immediately after run.cancelled is the state change, not more
	// narration from a worker that hadn't noticed yet.
	for i, ev := range final {
		if ev.Type != "run.cancelled" {
			continue
		}
		for _, later := range final[i+1:] {
			if later.Type == "assistant.message" || later.Type == "tool.result" || later.Type == "input.requested" {
				t.Fatalf("cancelled worker kept writing: %s after run.cancelled", later.Type)
			}
		}
	}

	// The project is free: a new request starts a fresh run.
	e.say(sessionID, "try again", "k3")
	after := e.waitFor(sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.started") >= 2
	})
	if got := count(after, "run.cancelled"); got != 1 {
		t.Fatalf("want exactly 1 cancellation, got %d", got)
	}
}
