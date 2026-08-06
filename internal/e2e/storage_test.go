package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func countCASBlobs(t *testing.T, e *env) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.dataDir, "cas"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// D2 / R-TEN-3: the storage cap has to bite where the store actually grows —
// at the commit — or "it cannot use more than X" is a dashboard rather than a
// guarantee. A tenant that hits it hears a sentence, not an error code, and a
// neighbour is untouched.
func TestStorageLimitRefusesInPlainLanguage(t *testing.T) {
	e := newAuthEnv(t, "fake:big-site") // writes ~2 MB in one page
	e.start()
	_, token := e.newTenantToken(t, "cramped", "--max-storage-mb", "1")
	session := createProject(t, e.env, token, "p")

	sayAuthed(t, e.env, token, session, "build me a site", "k1")
	evs := waitForAuthed(t, e.env, token, session, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.failed") >= 1
	})

	var failText string
	for _, ev := range evs {
		if ev.Type == "run.failed" {
			failText = ev.UserText
		}
	}
	lower := strings.ToLower(failText)
	if !strings.Contains(lower, "room") && !strings.Contains(lower, "space") {
		t.Fatalf("storage refusal not in plain language: %q", failText)
	}
	for _, jargon := range []string{"bytes", "quota", "sql", "error:", "max_storage", "blob"} {
		if strings.Contains(lower, jargon) {
			t.Fatalf("technical detail leaked to the user: %q", failText)
		}
	}
	if count(evs, "run.completed") != 0 {
		t.Fatal("run completed despite an exhausted storage limit")
	}
	if count(evs, "artifact.version.created") != 0 {
		t.Fatal("a version was saved despite the refusal")
	}
}

// A generous limit must not fire spuriously: the same build succeeds when the
// tenant has room. Without this, "the cap works" could just mean "commits are
// broken".
func TestStorageLimitLeavesRoomAlone(t *testing.T) {
	e := newAuthEnv(t, "fake:big-site")
	e.start()
	_, token := e.newTenantToken(t, "roomy", "--max-storage-mb", "50")
	session := createProject(t, e.env, token, "p")

	sayAuthed(t, e.env, token, session, "build me a site", "k1")
	evs := waitForAuthed(t, e.env, token, session, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1 || count(evs, "run.failed") >= 1
	})
	if count(evs, "run.failed") != 0 {
		t.Fatal("a 2 MB site was refused under a 50 MB limit")
	}
	if count(evs, "artifact.version.created") < 1 {
		t.Fatal("no version saved")
	}
}

// A refused commit must leave nothing behind. If the store grew on every
// failed attempt, a tenant at their limit would dig themselves deeper by
// simply retrying — and retrying is exactly what people do.
func TestRefusedCommitWritesNothing(t *testing.T) {
	e := newAuthEnv(t, "fake:big-site")
	e.start()
	_, token := e.newTenantToken(t, "cramped", "--max-storage-mb", "1")
	session := createProject(t, e.env, token, "p")

	before := countCASBlobs(t, e.env)
	sayAuthed(t, e.env, token, session, "build me a site", "k1")
	waitForAuthed(t, e.env, token, session, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.failed") >= 1
	})
	time.Sleep(300 * time.Millisecond) // let any stray write land

	if after := countCASBlobs(t, e.env); after != before {
		t.Fatalf("a refused commit still wrote %d blob(s) to the store", after-before)
	}
}

// Limits are per tenant: one tenant hitting the wall must not affect another.
func TestStorageLimitIsPerTenant(t *testing.T) {
	e := newAuthEnv(t, "fake:big-site")
	e.start()
	_, cramped := e.newTenantToken(t, "cramped", "--max-storage-mb", "1")
	_, roomy := e.newTenantToken(t, "roomy", "--max-storage-mb", "50")

	crampedSession := createProject(t, e.env, cramped, "p1")
	sayAuthed(t, e.env, cramped, crampedSession, "build me a site", "k1")
	waitForAuthed(t, e.env, cramped, crampedSession, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.failed") >= 1
	})

	roomySession := createProject(t, e.env, roomy, "p2")
	sayAuthed(t, e.env, roomy, roomySession, "build me a site", "k2")
	evs := waitForAuthed(t, e.env, roomy, roomySession, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1 || count(evs, "run.failed") >= 1
	})
	if count(evs, "run.failed") != 0 {
		t.Fatal("a neighbour's exhausted limit blocked this tenant")
	}
}
