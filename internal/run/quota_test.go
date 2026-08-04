package run

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/korya/creo/internal/store"
)

// M1 per-tenant concurrency quota: with max_concurrent_runs=1, a tenant's
// second run (in a different project) is unclaimable until the first finishes.
func TestTenantConcurrencyQuota(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	err = db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO tenants (id, name, max_concurrent_runs, created_at) VALUES ('t_one','one',1,?)`, now); err != nil {
			return err
		}
		for _, p := range []string{"pa", "pb"} {
			if _, err := tx.Exec(`INSERT INTO projects (id, tenant_id, name, created_at) VALUES (?,'t_one',?,?)`, p, p, now); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO sessions (id, tenant_id, project_id, created_at) VALUES (?,'t_one',?,?)`, "s_"+p, p, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := New(db, time.Minute)
	c.RequestRun(ctx, "s_pa", "pa", "e1", "k1")
	c.RequestRun(ctx, "s_pb", "pb", "e2", "k2")

	r1, err := c.Claim(ctx, "w1")
	if err != nil || r1 == nil {
		t.Fatalf("first claim: %+v %v", r1, err)
	}
	if r2, _ := c.Claim(ctx, "w2"); r2 != nil {
		t.Fatalf("quota violated: second concurrent run claimed: %+v", r2)
	}
	if err := c.Complete(ctx, r1.Lease, StatusCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	r3, _ := c.Claim(ctx, "w2")
	if r3 == nil || r3.ProjectID != "pb" {
		t.Fatalf("queued run not claimable after quota freed: %+v", r3)
	}
}
