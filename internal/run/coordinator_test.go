package run

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/store"
)

func testCoord(t *testing.T, ttl time.Duration) (*Coordinator, *eventlog.Log, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	err = db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, p := range []string{"p1", "p2"} {
			if _, err := tx.Exec(`INSERT INTO projects (id, name, created_at) VALUES (?,?,?)`, p, p, now); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO sessions (id, project_id, created_at) VALUES (?,?,?)`, "s_"+p, p, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(db, ttl), eventlog.New(db), db
}

// RC-1: duplicate idempotency key returns the original run, never a second one.
func TestIdempotentRequest(t *testing.T) {
	c, _, _ := testCoord(t, time.Minute)
	ctx := context.Background()
	a, err := c.RequestRun(ctx, "s_p1", "p1", "e1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.RequestRun(ctx, "s_p1", "p1", "e1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if !b.Deduped || b.RunID != a.RunID {
		t.Fatalf("want dedup of %s, got %+v", a.RunID, b)
	}
	c2, _ := c.RequestRun(ctx, "s_p1", "p1", "e2", "key-2")
	if c2.RunID == a.RunID || c2.Deduped {
		t.Fatalf("distinct key must create a new run: %+v", c2)
	}
}

// RC-2: while a project's run holds an unexpired lease, its other runs are unclaimable.
func TestSingleWriterPerProject(t *testing.T) {
	c, _, _ := testCoord(t, time.Minute)
	ctx := context.Background()
	c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
	c.RequestRun(ctx, "s_p1", "p1", "e2", "k2")
	c.RequestRun(ctx, "s_p2", "p2", "e3", "k3")

	r1, err := c.Claim(ctx, "w1")
	if err != nil || r1 == nil {
		t.Fatalf("first claim: %v %v", r1, err)
	}
	r2, err := c.Claim(ctx, "w2")
	if err != nil {
		t.Fatal(err)
	}
	if r2 == nil || r2.ProjectID != "p2" {
		t.Fatalf("second claim must skip p1 (locked) and take p2, got %+v", r2)
	}
	r3, _ := c.Claim(ctx, "w3")
	if r3 != nil {
		t.Fatalf("no claimable run should remain, got %+v", r3)
	}

	// Completing p1's run frees the project for its queued run.
	if err := c.Complete(ctx, r1.Lease, StatusCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	r4, _ := c.Claim(ctx, "w3")
	if r4 == nil || r4.ProjectID != "p1" {
		t.Fatalf("queued p1 run should now be claimable, got %+v", r4)
	}
}

// RC-3: generations strictly increase; a superseded holder cannot renew,
// complete, or (composing with SL-3) append.
func TestFencingAfterTakeover(t *testing.T) {
	c, l, _ := testCoord(t, 50*time.Millisecond)
	ctx := context.Background()
	c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")

	r1, _ := c.Claim(ctx, "w1")
	if r1 == nil {
		t.Fatal("claim failed")
	}
	time.Sleep(80 * time.Millisecond) // let the lease expire
	if n, err := c.RecoverOrphans(ctx); err != nil || n != 1 {
		t.Fatalf("recover: n=%d err=%v", n, err)
	}
	r2, _ := c.Claim(ctx, "w2")
	if r2 == nil || r2.ID != r1.ID {
		t.Fatalf("takeover claim should return same run, got %+v", r2)
	}
	if r2.Lease.Gen <= r1.Lease.Gen {
		t.Fatalf("generation must increase: %d -> %d", r1.Lease.Gen, r2.Lease.Gen)
	}

	// The old holder is fenced everywhere.
	if err := c.Renew(ctx, r1.Lease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale renew: want ErrLeaseLost, got %v", err)
	}
	if err := c.Complete(ctx, r1.Lease, StatusCompleted, "x"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale complete: want ErrLeaseLost, got %v", err)
	}
	if _, err := l.Append(ctx, "s_p1", []eventlog.NewEvent{{Type: "late"}}, &r1.Lease); !errors.Is(err, eventlog.ErrStaleLease) {
		t.Fatalf("stale append: want ErrStaleLease, got %v", err)
	}
	// The new holder works.
	if err := c.Renew(ctx, r2.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ctx, "s_p1", []eventlog.NewEvent{{Type: "ok"}}, &r2.Lease); err != nil {
		t.Fatal(err)
	}
}

// Relinquish (fix-04): a held run yields to `recovering` and is immediately
// claimable; a superseded holder's relinquish is a fenced no-op.
func TestRelinquish(t *testing.T) {
	c, _, _ := testCoord(t, time.Minute)
	ctx := context.Background()
	c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
	r1, _ := c.Claim(ctx, "w1")
	if r1 == nil {
		t.Fatal("claim failed")
	}
	// Graceful hand-off: relinquish leaves it recovering, no failure.
	if err := c.Relinquish(ctx, r1.Lease); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Get(ctx, r1.ID)
	if got.Status != StatusRecovering {
		t.Fatalf("want recovering after relinquish, got %s", got.Status)
	}
	// Immediately claimable (no lease-expiry wait), with a higher generation.
	r2, _ := c.Claim(ctx, "w2")
	if r2 == nil || r2.ID != r1.ID {
		t.Fatalf("relinquished run not immediately claimable: %+v", r2)
	}
	if r2.Lease.Gen <= r1.Lease.Gen {
		t.Fatalf("generation must increase on re-claim: %d -> %d", r1.Lease.Gen, r2.Lease.Gen)
	}
	// The old holder can no longer relinquish (fenced).
	if err := c.Relinquish(ctx, r1.Lease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale relinquish: want ErrLeaseLost, got %v", err)
	}
	got, _ = c.Get(ctx, r1.ID)
	if got.Status != StatusRunning {
		t.Fatalf("stale relinquish must not disturb the new holder: %s", got.Status)
	}
}

// RC-4 + RC-5: an expired-lease run becomes claimable; nothing is stuck in limbo.
func TestRecoveryScan(t *testing.T) {
	c, _, _ := testCoord(t, 30*time.Millisecond)
	ctx := context.Background()
	c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
	r, _ := c.Claim(ctx, "w1")
	if r == nil {
		t.Fatal("claim failed")
	}
	// Kill the worker (do nothing) and let the lease lapse.
	time.Sleep(60 * time.Millisecond)

	n, err := c.RecoverOrphans(ctx)
	if err != nil || n != 1 {
		t.Fatalf("recover found %d (%v)", n, err)
	}
	got, _ := c.Get(ctx, r.ID)
	if got.Status != StatusRecovering {
		t.Fatalf("want recovering, got %s", got.Status)
	}
	r2, _ := c.Claim(ctx, "w2")
	if r2 == nil || r2.ID != r.ID {
		t.Fatal("recovering run must be claimable")
	}
	if err := c.Complete(ctx, r2.Lease, StatusCompleted, "recovered"); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Get(ctx, r.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("run stuck: %s", got.Status)
	}
}

// A renewed lease keeps the project locked past the original TTL.
func TestRenewExtendsLease(t *testing.T) {
	c, _, _ := testCoord(t, 60*time.Millisecond)
	ctx := context.Background()
	c.RequestRun(ctx, "s_p1", "p1", "e1", "k1")
	c.RequestRun(ctx, "s_p1", "p1", "e2", "k2")
	r, _ := c.Claim(ctx, "w1")
	for i := 0; i < 3; i++ {
		time.Sleep(40 * time.Millisecond)
		if err := c.Renew(ctx, r.Lease); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := c.RecoverOrphans(ctx); n != 0 {
		t.Fatalf("renewed lease treated as orphan")
	}
	if other, _ := c.Claim(ctx, "w2"); other != nil {
		t.Fatalf("project lock violated during renewed lease: %+v", other)
	}
}
