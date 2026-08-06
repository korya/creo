package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/korya/creo/internal/eventlog"
)

// claimOne claims and fails the test if nothing was claimable.
func claimOne(t *testing.T, c *Coordinator, worker string) *Run {
	t.Helper()
	r, err := c.Claim(context.Background(), worker)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("expected a claimable run, got none")
	}
	return r
}

func statusOf(t *testing.T, c *Coordinator, runID string) string {
	t.Helper()
	r, err := c.Get(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}

// A parked question holds no lease and no worker: the round trip
// running → waiting → queued → running works, and the second claim gets a
// fresh generation (RC-3 still applies across the pause).
func TestAwaitResumeRoundTrip(t *testing.T) {
	c, _, _ := testCoord(t, time.Minute)
	ctx := context.Background()
	ref, err := c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
	if err != nil {
		t.Fatal(err)
	}
	first := claimOne(t, c, "w1")

	if err := c.Await(ctx, first.Lease); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, c, ref.RunID); got != StatusWaiting {
		t.Fatalf("status = %q, want %q", got, StatusWaiting)
	}

	// While waiting, nothing is claimable — the run holds no lease, but it is
	// also not up for grabs.
	if r, _ := c.Claim(ctx, "w2"); r != nil {
		t.Fatalf("waiting run was claimed by another worker: %+v", r)
	}

	if err := c.Resume(ctx, ref.RunID); err != nil {
		t.Fatal(err)
	}
	second := claimOne(t, c, "w2")
	if second.ID != ref.RunID {
		t.Fatalf("resumed a different run: %s", second.ID)
	}
	if second.Lease.Gen <= first.Lease.Gen {
		t.Fatalf("generation did not advance across the pause: %d -> %d", first.Lease.Gen, second.Lease.Gen)
	}
}

// RC-2 extends to waiting: a parked conversation still owns its project, so a
// second run must not start underneath it and mutate the same site.
func TestWaitingRunBlocksSiblingsButNotOtherProjects(t *testing.T) {
	c, _, _ := testCoord(t, time.Minute)
	ctx := context.Background()
	first, _ := c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
	held := claimOne(t, c, "w1")
	if err := c.Await(ctx, held.Lease); err != nil {
		t.Fatal(err)
	}

	// A second run on the SAME project is queued but unclaimable.
	if _, err := c.RequestRun(ctx, "s_p1", "p1", "e2", "k2"); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Claim(ctx, "w2"); r != nil {
		t.Fatalf("claimed a sibling run while the project waits for input: %+v", r)
	}

	// A different project is unaffected.
	other, _ := c.RequestRun(ctx, "s_p2", "p2", "e3", "k3")
	got := claimOne(t, c, "w3")
	if got.ID != other.RunID {
		t.Fatalf("claimed %s, want the other project's run %s", got.ID, other.RunID)
	}

	// Answering the first frees the project for its own queue.
	if err := c.Resume(ctx, first.RunID); err != nil {
		t.Fatal(err)
	}
	if r := claimOne(t, c, "w4"); r.ID != first.RunID {
		t.Fatalf("claimed %s, want the resumed run %s", r.ID, first.RunID)
	}
}

// A question outlives the process: recovery leaves waiting runs alone, because
// there is no lease to expire and no work in flight to rescue.
func TestRecoveryIgnoresWaitingRuns(t *testing.T) {
	c, _, _ := testCoord(t, time.Millisecond)
	ctx := context.Background()
	ref, _ := c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
	held := claimOne(t, c, "w1")
	if err := c.Await(ctx, held.Lease); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // any lease would be long expired

	n, err := c.RecoverOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recovery touched %d waiting run(s), want 0", n)
	}
	if got := statusOf(t, c, ref.RunID); got != StatusWaiting {
		t.Fatalf("status = %q, want %q — the question must survive", got, StatusWaiting)
	}
}

// Only a waiting run can be resumed; a stale answer from a second device is
// refused rather than restarting finished work.
func TestResumeRejectsNonWaiting(t *testing.T) {
	c, _, _ := testCoord(t, time.Minute)
	ctx := context.Background()
	ref, _ := c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")

	if err := c.Resume(ctx, ref.RunID); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("resume of a queued run: %v, want ErrNotWaiting", err)
	}
	held := claimOne(t, c, "w1")
	if err := c.Resume(ctx, ref.RunID); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("resume of a running run: %v, want ErrNotWaiting", err)
	}
	if err := c.Complete(ctx, held.Lease, StatusCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if err := c.Resume(ctx, ref.RunID); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("resume of a completed run: %v, want ErrNotWaiting", err)
	}
}

// R-RUN-4: cancel works from every non-terminal state and is refused (kindly)
// once the run has finished.
func TestCancelFromEveryState(t *testing.T) {
	ctx := context.Background()

	t.Run("queued", func(t *testing.T) {
		c, _, _ := testCoord(t, time.Minute)
		ref, _ := c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
		if err := c.Cancel(ctx, ref.RunID); err != nil {
			t.Fatal(err)
		}
		if got := statusOf(t, c, ref.RunID); got != StatusCancelled {
			t.Fatalf("status = %q, want %q", got, StatusCancelled)
		}
		// A cancelled run is not claimable.
		if r, _ := c.Claim(ctx, "w1"); r != nil {
			t.Fatalf("claimed a cancelled run: %+v", r)
		}
	})

	t.Run("running: the holder's lease dies with it", func(t *testing.T) {
		c, log, _ := testCoord(t, time.Minute)
		ref, _ := c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
		held := claimOne(t, c, "w1")
		if err := c.Cancel(ctx, ref.RunID); err != nil {
			t.Fatal(err)
		}
		// This is what stops the worker: the same fencing that makes crash
		// takeover safe.
		if err := c.Renew(ctx, held.Lease); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("renew after cancel: %v, want ErrLeaseLost", err)
		}
		// And the holder is fenced at the STORE, not merely at its next
		// renew: a worker mid-tool-call cannot keep narrating a stopped run.
		if _, err := log.Append(ctx, "s_p1",
			[]eventlog.NewEvent{{Type: "assistant.message", RunID: ref.RunID, UserText: "still going"}},
			&held.Lease); !errors.Is(err, eventlog.ErrStaleLease) {
			t.Fatalf("cancelled worker could still append: %v, want ErrStaleLease", err)
		}
		if err := c.Complete(ctx, held.Lease, StatusCompleted, "too late"); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("a cancelled run must not be completable: %v", err)
		}
		if got := statusOf(t, c, ref.RunID); got != StatusCancelled {
			t.Fatalf("status = %q, want %q", got, StatusCancelled)
		}
	})

	t.Run("waiting", func(t *testing.T) {
		c, _, _ := testCoord(t, time.Minute)
		ref, _ := c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
		held := claimOne(t, c, "w1")
		if err := c.Await(ctx, held.Lease); err != nil {
			t.Fatal(err)
		}
		if err := c.Cancel(ctx, ref.RunID); err != nil {
			t.Fatal(err)
		}
		if got := statusOf(t, c, ref.RunID); got != StatusCancelled {
			t.Fatalf("status = %q, want %q", got, StatusCancelled)
		}
		// The project is free again: a new run can start.
		next, _ := c.RequestRun(ctx, "s_p1", "p1", "e2", "k2")
		if r := claimOne(t, c, "w2"); r.ID != next.RunID {
			t.Fatalf("claimed %s, want %s", r.ID, next.RunID)
		}
	})

	t.Run("already finished", func(t *testing.T) {
		c, _, _ := testCoord(t, time.Minute)
		ref, _ := c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
		held := claimOne(t, c, "w1")
		if err := c.Complete(ctx, held.Lease, StatusCompleted, "done"); err != nil {
			t.Fatal(err)
		}
		if err := c.Cancel(ctx, ref.RunID); !errors.Is(err, ErrTerminal) {
			t.Fatalf("cancel of a completed run: %v, want ErrTerminal", err)
		}
		if got := statusOf(t, c, ref.RunID); got != StatusCompleted {
			t.Fatalf("cancel rewrote a terminal state to %q", got)
		}
	})

	t.Run("unknown run", func(t *testing.T) {
		c, _, _ := testCoord(t, time.Minute)
		if err := c.Cancel(ctx, "run_nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cancel of an unknown run: %v, want ErrNotFound", err)
		}
	})
}

// Await is lease-fenced like every other authoritative transition: a worker
// whose lease was superseded cannot park the run it no longer owns.
func TestAwaitIsFenced(t *testing.T) {
	c, _, _ := testCoord(t, time.Millisecond)
	ctx := context.Background()
	c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
	stale := claimOne(t, c, "w1")
	time.Sleep(10 * time.Millisecond)
	if _, err := c.RecoverOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	claimOne(t, c, "w2") // takeover: generation advances

	if err := c.Await(ctx, stale.Lease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker parked the run: %v, want ErrLeaseLost", err)
	}
}
