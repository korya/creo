// Package publish owns the live-version pointer and per-project preview secret.
// Publish and rollback are single-statement pointer flips — a visitor sees the
// old version or the new one, never a mix.
package publish

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/korya/creo/internal/store"
)

var (
	ErrNoParent     = errors.New("no previous version to roll back to")
	ErrNotPublished = errors.New("project has no published version")
)

type Live struct {
	ProjectID string `json:"projectId"`
	VersionID string `json:"versionId"`
	Slug      string `json:"slug"`
}

type Store struct {
	db *store.DB
}

func New(db *store.DB) *Store { return &Store{db: db} }

// EnsurePreviewSecret returns the project's preview capability secret, minting
// one on first use.
func (s *Store) EnsurePreviewSecret(ctx context.Context, projectID string) (string, error) {
	var secret string
	err := s.db.R.QueryRowContext(ctx, `SELECT preview_secret FROM projects WHERE id = ?`, projectID).Scan(&secret)
	if err != nil {
		return "", err
	}
	if secret != "" {
		return secret, nil
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret = hex.EncodeToString(raw)
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		// Only set if still empty (avoid clobbering a concurrent mint).
		res, err := tx.Exec(`UPDATE projects SET preview_secret = ? WHERE id = ? AND preview_secret = ''`, secret, projectID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return tx.QueryRow(`SELECT preview_secret FROM projects WHERE id = ?`, projectID).Scan(&secret)
		}
		return nil
	})
	return secret, err
}

// CheckPreviewSecret constant-time compares a supplied secret to the project's.
func (s *Store) CheckPreviewSecret(ctx context.Context, projectID, supplied string) bool {
	var secret string
	if err := s.db.R.QueryRowContext(ctx, `SELECT preview_secret FROM projects WHERE id = ?`, projectID).Scan(&secret); err != nil {
		return false
	}
	return secret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(supplied)) == 1
}

// Publish points the project's live URL at a version (atomic upsert). The slug
// defaults to the project id on first publish and is stable thereafter.
func (s *Store) Publish(ctx context.Context, projectID, versionID string) (Live, error) {
	live := Live{ProjectID: projectID, VersionID: versionID}
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		var slug string
		err := tx.QueryRow(`SELECT slug FROM published WHERE project_id = ?`, projectID).Scan(&slug)
		if errors.Is(err, sql.ErrNoRows) {
			slug = projectID
		} else if err != nil {
			return err
		}
		live.Slug = slug
		_, err = tx.Exec(`
			INSERT INTO published (project_id, version_id, slug, published_at) VALUES (?,?,?,?)
			ON CONFLICT(project_id) DO UPDATE SET version_id = excluded.version_id, published_at = excluded.published_at`,
			projectID, versionID, slug, now())
		return err
	})
	return live, err
}

// Rollback repoints live at the parent of the currently-published version.
func (s *Store) Rollback(ctx context.Context, projectID string) (Live, error) {
	var live Live
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		var current, slug string
		err := tx.QueryRow(`SELECT version_id, slug FROM published WHERE project_id = ?`, projectID).Scan(&current, &slug)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotPublished
		} else if err != nil {
			return err
		}
		var parent string
		err = tx.QueryRow(`SELECT parent_id FROM versions WHERE project_id = ? AND id = ?`, projectID, current).Scan(&parent)
		if err != nil {
			return err
		}
		if parent == "" {
			return ErrNoParent
		}
		if _, err := tx.Exec(`UPDATE published SET version_id = ?, published_at = ? WHERE project_id = ?`, parent, now(), projectID); err != nil {
			return err
		}
		live = Live{ProjectID: projectID, VersionID: parent, Slug: slug}
		return nil
	})
	return live, err
}

// Current returns the live pointer for a project.
func (s *Store) Current(ctx context.Context, projectID string) (Live, error) {
	var live Live
	err := s.db.R.QueryRowContext(ctx,
		`SELECT project_id, version_id, slug FROM published WHERE project_id = ?`, projectID).
		Scan(&live.ProjectID, &live.VersionID, &live.Slug)
	if errors.Is(err, sql.ErrNoRows) {
		return live, ErrNotPublished
	}
	return live, err
}

// BySlug resolves a public slug to its live version (the /sites/{slug} path).
func (s *Store) BySlug(ctx context.Context, slug string) (Live, error) {
	var live Live
	err := s.db.R.QueryRowContext(ctx,
		`SELECT project_id, version_id, slug FROM published WHERE slug = ?`, slug).
		Scan(&live.ProjectID, &live.VersionID, &live.Slug)
	if errors.Is(err, sql.ErrNoRows) {
		return live, ErrNotPublished
	}
	return live, err
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
