package e2e

import (
	"strconv"
	"testing"
	"time"
)

// AC-4: close the laptop mid-build, pick up the phone, and the conversation is
// there — same transcript, same state, no work lost. The two "devices" here
// are two independent HTTP clients with their own cookie jars, which is
// exactly what a second device is to the server.
func TestSecondDeviceResumesMidBuild(t *testing.T) {
	e := newAuthEnv(t, "fake:slow-asking-site")
	e.admin("account", "new", "Anna", "--tenant", "t_default")
	e.start()

	laptop := newBrowser(t, e.env)
	laptop.signIn("Anna")
	var proj struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
	}
	laptop.do("POST", "/v1/projects", map[string]string{"name": "bakery"}, &proj)
	laptop.say(proj.SessionID, "build me a bakery site", "k1")

	// The laptop watches work begin, then "closes" — we simply stop using it.
	laptop.waitFor(proj.SessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "tool.result") >= 1
	})

	// The phone signs in fresh and asks the platform what is going on. It must
	// learn the state from the platform, not guess from the transcript.
	phone := newBrowser(t, e.env)
	phone.signIn("Anna")
	var status map[string]any
	if code := phone.do("GET", "/v1/sessions/"+proj.SessionID, nil, &status); code != 200 {
		t.Fatalf("phone could not read session state: HTTP %d", code)
	}
	if s := status["state"]; s != "working" && s != "waiting-for-input" && s != "queued" {
		t.Fatalf("phone sees state %v, want the build in progress", s)
	}

	// And the whole transcript so far, from sequence zero.
	evs := phone.events(proj.SessionID)
	if count(evs, "user.message") != 1 {
		t.Fatalf("phone does not see the original request: %d user messages", count(evs, "user.message"))
	}

	// The build reaches its question; the phone answers it.
	phone.waitFor(proj.SessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "input.requested") >= 1
	})
	phone.do("GET", "/v1/sessions/"+proj.SessionID, nil, &status)
	runID, _ := status["runId"].(string)
	if code := phone.do("POST", "/v1/runs/"+runID+"/input", map[string]string{"text": "Weekdays 9-5"}, nil); code != 202 {
		t.Fatalf("phone could not answer: HTTP %d", code)
	}

	// The laptop — reconnecting from its own cursor — sees the answer it never
	// sent, and the completed run.
	final := laptop.waitFor(proj.SessionID, 20*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})
	answered := false
	for _, ev := range final {
		if ev.Type == "input.provided" && ev.UserText == "Weekdays 9-5" {
			answered = true
		}
	}
	if !answered {
		t.Fatal("the laptop never learned what the phone answered")
	}
	if count(final, "run.started") != 1 {
		t.Fatalf("the handover started extra runs: %d", count(final, "run.started"))
	}
}

// AC-4, the plainer half: a device that disconnects entirely and comes back
// resumes from its cursor with no gap and no duplicates.
func TestReconnectFromCursorLosesNothing(t *testing.T) {
	e := newEnv(t, "fake:site")
	e.start()
	_, sessionID := e.newProject("bakery")
	e.say(sessionID, "build me a bakery site", "k1")
	all := e.waitFor(sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})

	// Reconnect from the middle, as a client with a stored cursor would.
	mid := len(all) / 2
	cursor := all[mid].Seq
	want := all[mid+1:] // exactly what the client has not seen
	var tail []event
	e.call("GET", "/v1/sessions/"+sessionID+"/events?stream=false&after="+strconv.FormatInt(cursor, 10), nil, nil, &tail)

	if len(tail) != len(want) {
		t.Fatalf("backfill from cursor %d returned %d events, want %d", cursor, len(tail), len(want))
	}
	for i := range want {
		if tail[i].Seq != want[i].Seq || tail[i].Type != want[i].Type {
			t.Fatalf("backfill diverges at %d: got %s#%d, want %s#%d",
				i, tail[i].Type, tail[i].Seq, want[i].Type, want[i].Seq)
		}
	}
	seen := map[int64]bool{}
	for _, ev := range tail {
		if seen[ev.Seq] {
			t.Fatalf("duplicate event at seq %d", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}

// R-NFR-2 (time to first token, p95 < 3s) is a property of log-first resume:
// nothing needs warming before a returning client is served. v-min has no
// parked harness to tune, so this measures rather than tunes — and fails only
// if the design has quietly stopped delivering on it.
func TestReturningClientIsServedPromptly(t *testing.T) {
	e := newEnv(t, "fake:site")
	e.start()
	_, sessionID := e.newProject("bakery")
	e.say(sessionID, "build me a bakery site", "k1")
	e.waitFor(sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})

	// A device returning later: state, then transcript. This is the whole cold
	// path a phone hits when it wakes up.
	var worst time.Duration
	for range 5 {
		start := time.Now()
		var status map[string]any
		e.call("GET", "/v1/sessions/"+sessionID, nil, nil, &status)
		var evs []event
		e.call("GET", "/v1/sessions/"+sessionID+"/events?stream=false", nil, nil, &evs)
		if elapsed := time.Since(start); elapsed > worst {
			worst = elapsed
		}
		if len(evs) == 0 {
			t.Fatal("returning client got an empty transcript")
		}
	}
	t.Logf("returning-client latency (worst of 5): %s — R-NFR-2 target is p95 < 3s", worst)
	if worst > 3*time.Second {
		t.Fatalf("a returning client waited %s; log-first resume should make this immediate", worst)
	}
}
