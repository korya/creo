package publish

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/korya/creo/internal/store"
)

func setup(t *testing.T) (*Store, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Two chained versions: v1 (root) <- v2.
	err = db.Write(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO projects (id, name, created_at) VALUES ('p1','p',?)`, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO versions (id, project_id, seq, parent_id, produced_by_event, created_at) VALUES ('v1','p1',1,'','e1',?)`, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO versions (id, project_id, seq, parent_id, produced_by_event, created_at) VALUES ('v2','p1',2,'v1','e2',?)`, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(db), db
}

func TestPublishRollbackLineage(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()

	live, err := s.Publish(ctx, "p1", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if live.Slug != "p1" || live.VersionID != "v2" {
		t.Fatalf("publish: %+v", live)
	}
	cur, _ := s.Current(ctx, "p1")
	if cur.VersionID != "v2" {
		t.Fatalf("current after publish: %+v", cur)
	}
	bySlug, err := s.BySlug(ctx, "p1")
	if err != nil || bySlug.VersionID != "v2" {
		t.Fatalf("by slug: %+v %v", bySlug, err)
	}

	// Rollback walks v2 -> its parent v1.
	rb, err := s.Rollback(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if rb.VersionID != "v1" {
		t.Fatalf("rollback target: %+v", rb)
	}
	cur, _ = s.Current(ctx, "p1")
	if cur.VersionID != "v1" {
		t.Fatalf("current after rollback: %+v", cur)
	}

	// v1 is root: another rollback has nowhere to go.
	if _, err := s.Rollback(ctx, "p1"); !errors.Is(err, ErrNoParent) {
		t.Fatalf("rollback past root: want ErrNoParent, got %v", err)
	}
}

func TestRollbackUnpublished(t *testing.T) {
	s, _ := setup(t)
	if _, err := s.Rollback(context.Background(), "p1"); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("want ErrNotPublished, got %v", err)
	}
}

func TestPreviewSecret(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	secret, err := s.EnsurePreviewSecret(ctx, "p1")
	if err != nil || secret == "" {
		t.Fatalf("mint: %q %v", secret, err)
	}
	// Idempotent.
	again, _ := s.EnsurePreviewSecret(ctx, "p1")
	if again != secret {
		t.Fatalf("secret changed: %q != %q", again, secret)
	}
	if !s.CheckPreviewSecret(ctx, "p1", secret) {
		t.Fatal("correct secret rejected")
	}
	if s.CheckPreviewSecret(ctx, "p1", "wrong") {
		t.Fatal("wrong secret accepted")
	}
	if s.CheckPreviewSecret(ctx, "p1", "") {
		t.Fatal("empty secret accepted")
	}
}
