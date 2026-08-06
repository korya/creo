package tenant

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

type file struct {
	sha  string
	size int64
}

// seedVersion records a saved version's files for a project, the way
// ProjectStore.Commit does.
func seedVersion(t *testing.T, s *Store, projectID, versionID string, files map[string]file) {
	t.Helper()
	err := s.db.Write(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(
			`INSERT INTO versions (id, project_id, seq, produced_by_event, created_at) VALUES (?,?,?,?,?)`,
			versionID, projectID, len(versionID), "e1", now); err != nil {
			return err
		}
		for path, f := range files {
			if _, err := tx.Exec(
				`INSERT INTO version_files (project_id, version_id, path, blob_sha, size) VALUES (?,?,?,?,?)`,
				projectID, versionID, path, f.sha, f.size); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedProject(t *testing.T, s *Store, projectID, tenantID string) {
	t.Helper()
	err := s.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (id, tenant_id, name, created_at) VALUES (?,?,?,?)`,
			projectID, tenantID, projectID, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Content is addressed by hash, so an unchanged file across many versions
// occupies the store once. Counting it per version would bill a user ten times
// for one logo they never touched.
func TestStorageUsedCountsEachBlobOnce(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	ten, _ := s.Create(ctx, "acme", nil, 2, nil)
	seedProject(t, s, "p1", ten.ID)

	logo := file{"sha-logo", 1000}
	seedVersion(t, s, "p1", "v1", map[string]file{"logo.svg": logo, "index.html": {"sha-a", 500}})
	seedVersion(t, s, "p1", "v2", map[string]file{"logo.svg": logo, "index.html": {"sha-b", 700}})

	used, err := s.StorageUsed(ctx, ten.ID)
	if err != nil {
		t.Fatal(err)
	}
	// logo once (1000) + two distinct index.html revisions (500 + 700).
	if used != 2200 {
		t.Fatalf("used = %d, want 2200 (the logo counted once across versions)", used)
	}
}

func TestStorageUsedIsPerTenant(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	mine, _ := s.Create(ctx, "mine", nil, 2, nil)
	theirs, _ := s.Create(ctx, "theirs", nil, 2, nil)
	seedProject(t, s, "p1", mine.ID)
	seedProject(t, s, "p2", theirs.ID)
	seedVersion(t, s, "p1", "v1", map[string]file{"a.html": {"sha-a", 100}})
	seedVersion(t, s, "p2", "v1", map[string]file{"b.html": {"sha-b", 9999}})

	used, _ := s.StorageUsed(ctx, mine.ID)
	if used != 100 {
		t.Fatalf("used = %d, want 100 — a neighbour's data must not count", used)
	}
}

func TestCheckStorage(t *testing.T) {
	ctx := context.Background()

	t.Run("unlimited by default", func(t *testing.T) {
		s, _ := testStore(t)
		ten, _ := s.Create(ctx, "acme", nil, 2, nil)
		seedProject(t, s, "p1", ten.ID)
		if err := s.CheckStorage(ctx, ten.ID, map[string]int64{"sha-huge": 1 << 40}); err != nil {
			t.Fatalf("no limit set, yet refused: %v", err)
		}
	})

	t.Run("refuses a commit that would exceed the limit", func(t *testing.T) {
		s, _ := testStore(t)
		limit := int64(1000)
		ten, _ := s.Create(ctx, "acme", nil, 2, &limit)
		seedProject(t, s, "p1", ten.ID)
		seedVersion(t, s, "p1", "v1", map[string]file{"a.html": {"sha-a", 800}})

		if err := s.CheckStorage(ctx, ten.ID, map[string]int64{"sha-new": 100}); err != nil {
			t.Fatalf("800 + 100 is under 1000, yet refused: %v", err)
		}
		err := s.CheckStorage(ctx, ten.ID, map[string]int64{"sha-new": 300})
		if !errors.Is(err, ErrStorageExceeded) {
			t.Fatalf("800 + 300 exceeds 1000: got %v, want ErrStorageExceeded", err)
		}
	})

	t.Run("content the tenant already holds is free", func(t *testing.T) {
		s, _ := testStore(t)
		limit := int64(1000)
		ten, _ := s.Create(ctx, "acme", nil, 2, &limit)
		seedProject(t, s, "p1", ten.ID)
		seedVersion(t, s, "p1", "v1", map[string]file{"big.png": {"sha-big", 900}})

		// Re-committing the same image must not be charged twice — otherwise a
		// tenant near the limit could never save any change at all.
		if err := s.CheckStorage(ctx, ten.ID, map[string]int64{"sha-big": 900, "sha-tiny": 50}); err != nil {
			t.Fatalf("unchanged content was charged again: %v", err)
		}
	})

	t.Run("a neighbour's usage does not consume my limit", func(t *testing.T) {
		s, _ := testStore(t)
		limit := int64(1000)
		mine, _ := s.Create(ctx, "mine", nil, 2, &limit)
		theirs, _ := s.Create(ctx, "theirs", nil, 2, nil)
		seedProject(t, s, "p1", mine.ID)
		seedProject(t, s, "p2", theirs.ID)
		seedVersion(t, s, "p2", "v1", map[string]file{"huge.bin": {"sha-huge", 100000}})

		if err := s.CheckStorage(ctx, mine.ID, map[string]int64{"sha-a": 500}); err != nil {
			t.Fatalf("a neighbour's usage blocked my commit: %v", err)
		}
	})
}
