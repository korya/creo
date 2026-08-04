package e2e

import (
	"sync"
	"testing"
	"time"
)

// S6: many concurrent submits of the same idempotency key produce exactly one
// run AND one user.message event (closes the M0 race).
func TestConcurrentDuplicateSubmit(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	e.start()
	_, token := e.newTenantToken(t, "t")
	session := createProject(t, e.env, token, "p")

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := doAuthed(t, e.env, token, "POST", "/v1/sessions/"+session+"/messages",
				map[string]string{"text": "build me a site"},
				map[string]string{"Idempotency-Key": "race-key"})
			resp.Body.Close()
		}()
	}
	wg.Wait()

	evs := waitForAuthed(t, e.env, token, session, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})
	if got := count(evs, "user.message"); got != 1 {
		t.Fatalf("concurrent duplicate submit appended %d user messages (want 1)", got)
	}
	if got := count(evs, "run.started"); got != 1 {
		t.Fatalf("started %d runs (want 1)", got)
	}
}
