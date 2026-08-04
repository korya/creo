package tenant

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korya/creo/internal/store"
)

func testStore(t *testing.T) (*Store, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db), db
}

func TestTokenLifecycle(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	ten, err := s.Create(ctx, "acme", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, tokenID, err := s.CreateToken(ctx, ten.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, "creo_") || len(plaintext) != 45 {
		t.Fatalf("token shape: %q", plaintext)
	}

	got, err := s.Authenticate(ctx, plaintext)
	if err != nil || got != ten.ID {
		t.Fatalf("authenticate: %q %v", got, err)
	}
	if _, err := s.Authenticate(ctx, "creo_garbage"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("garbage token: %v", err)
	}
	if err := s.RevokeToken(ctx, tokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked token still works: %v", err)
	}
	if err := s.RevokeToken(ctx, tokenID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
}

func TestTokenForUnknownTenant(t *testing.T) {
	s, _ := testStore(t)
	if _, _, err := s.CreateToken(context.Background(), "t_nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func seedUsage(t *testing.T, db *store.DB, tenantID string, tokens int64, at time.Time) {
	t.Helper()
	ctx := context.Background()
	err := db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		pid := "p_" + tenantID + now
		rid := "run_" + tenantID + now
		if _, err := tx.Exec(`INSERT INTO projects (id, tenant_id, name, created_at) VALUES (?,?,?,?)`, pid, tenantID, "x", now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO sessions (id, tenant_id, project_id, created_at) VALUES (?,?,?,?)`, "s_"+pid, tenantID, pid, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO runs (id, session_id, project_id, trigger_event_id, status, created_at, updated_at) VALUES (?,?,?,?,'completed',?,?)`,
			rid, "s_"+pid, pid, "e", now, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO usage (run_id, provider, model, input_tokens, output_tokens, created_at) VALUES (?,?,?,?,0,?)`,
			rid, "fake", "fake", tokens, at.UTC().Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBudget(t *testing.T) {
	s, db := testStore(t)
	ctx := context.Background()
	limit := int64(1000)
	ten, _ := s.Create(ctx, "capped", &limit, 2)
	other, _ := s.Create(ctx, "other", nil, 2)

	// Yesterday's spend does not count against today's window.
	seedUsage(t, db, ten.ID, 5000, time.Now().UTC().Add(-30*time.Hour))
	if err := s.CheckBudget(ctx, ten.ID); err != nil {
		t.Fatalf("yesterday's usage counted: %v", err)
	}

	// Under the limit today: allowed.
	seedUsage(t, db, ten.ID, 900, time.Now().UTC())
	if err := s.CheckBudget(ctx, ten.ID); err != nil {
		t.Fatalf("under limit: %v", err)
	}

	// At/over the limit: hard stop.
	seedUsage(t, db, ten.ID, 200, time.Now().UTC())
	if err := s.CheckBudget(ctx, ten.ID); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("want ErrBudgetExceeded, got %v", err)
	}

	// Another tenant's spend never bleeds over; unlimited tenant never blocked.
	if err := s.CheckBudget(ctx, other.ID); err != nil {
		t.Fatalf("cross-tenant bleed or unlimited blocked: %v", err)
	}
}
