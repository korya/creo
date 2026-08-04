// Package project is the ProjectStore component: immutable, content-addressed
// versions of what the project IS, each traceable to the event that produced
// it. Restore, sandbox rebuild, and publishing all draw from here.
package project

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/workspace"
)

var ErrNotFound = errors.New("version not found")

type VersionMeta struct {
	ID              string    `json:"id"`
	ProjectID       string    `json:"projectId"`
	Seq             int64     `json:"seq"`
	ParentID        string    `json:"parentId,omitempty"`
	ProducedByEvent string    `json:"producedByEvent"`
	CreatedAt       time.Time `json:"createdAt"`
}

type Store struct {
	db     *store.DB
	casDir string
}

func New(db *store.DB, casDir string) (*Store, error) {
	if err := os.MkdirAll(casDir, 0o755); err != nil {
		return nil, err
	}
	return &Store{db: db, casDir: casDir}, nil
}

// Commit snapshots the workspace into a content-addressed version. Identical
// content yields the identical version id (and is not duplicated).
func (s *Store) Commit(ctx context.Context, projectID string, ws *workspace.Workspace, producedByEvent string) (string, error) {
	files, err := ws.ListFiles()
	if err != nil {
		return "", err
	}
	type entry struct {
		path, sha string
		size      int
	}
	var manifest []entry
	var manifestLines []string
	for _, path := range files {
		content, err := ws.ReadFile(path)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(content)
		sha := hex.EncodeToString(sum[:])
		blob := filepath.Join(s.casDir, sha)
		if _, err := os.Stat(blob); errors.Is(err, os.ErrNotExist) {
			tmp := blob + ".tmp"
			if err := os.WriteFile(tmp, content, 0o644); err != nil {
				return "", err
			}
			if err := os.Rename(tmp, blob); err != nil {
				return "", err
			}
		}
		manifest = append(manifest, entry{path, sha, len(content)})
		manifestLines = append(manifestLines, fmt.Sprintf("%s\x00%s\x00%d", path, sha, len(content)))
	}
	sort.Strings(manifestLines)
	manifestSum := sha256.Sum256([]byte(strings.Join(manifestLines, "\n")))
	versionID := "v_" + hex.EncodeToString(manifestSum[:16])

	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM versions WHERE project_id = ? AND id = ?`, projectID, versionID).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return nil // identical content already committed
		}
		var parent string
		var maxSeq int64
		err := tx.QueryRow(`SELECT id, seq FROM versions WHERE project_id = ? ORDER BY seq DESC LIMIT 1`, projectID).Scan(&parent, &maxSeq)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(
			`INSERT INTO versions (id, project_id, seq, parent_id, produced_by_event, created_at) VALUES (?,?,?,?,?,?)`,
			versionID, projectID, maxSeq+1, parent, producedByEvent, now,
		); err != nil {
			return err
		}
		for _, e := range manifest {
			if _, err := tx.Exec(
				`INSERT INTO version_files (project_id, version_id, path, blob_sha, size) VALUES (?,?,?,?,?)`,
				projectID, versionID, e.path, e.sha, e.size,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return versionID, nil
}

// Materialize replaces the workspace content with the given version, byte-exact.
func (s *Store) Materialize(ctx context.Context, projectID, versionID string, ws *workspace.Workspace) error {
	rows, err := s.db.R.QueryContext(ctx,
		`SELECT path, blob_sha FROM version_files WHERE project_id = ? AND version_id = ? ORDER BY path`, projectID, versionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type entry struct{ path, sha string }
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.path, &e.sha); err != nil {
			return err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(entries) == 0 {
		var n int
		if err := s.db.R.QueryRow(`SELECT COUNT(*) FROM versions WHERE project_id = ? AND id = ?`, projectID, versionID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s/%s", ErrNotFound, projectID, versionID)
		}
	}
	if err := ws.Clear(); err != nil {
		return err
	}
	for _, e := range entries {
		content, err := os.ReadFile(filepath.Join(s.casDir, e.sha))
		if err != nil {
			return fmt.Errorf("cas blob %s: %w", e.sha, err)
		}
		if err := ws.WriteFile(e.path, content); err != nil {
			return err
		}
	}
	return nil
}

// Latest returns the newest version id for a project ("" if none).
func (s *Store) Latest(ctx context.Context, projectID string) (string, error) {
	var id string
	err := s.db.R.QueryRowContext(ctx,
		`SELECT id FROM versions WHERE project_id = ? ORDER BY seq DESC LIMIT 1`, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Store) ListVersions(ctx context.Context, projectID string) ([]VersionMeta, error) {
	rows, err := s.db.R.QueryContext(ctx,
		`SELECT id, seq, parent_id, produced_by_event, created_at FROM versions WHERE project_id = ? ORDER BY seq`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionMeta
	for rows.Next() {
		var v VersionMeta
		var created string
		if err := rows.Scan(&v.ID, &v.Seq, &v.ParentID, &v.ProducedByEvent, &created); err != nil {
			return nil, err
		}
		v.ProjectID = projectID
		v.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, v)
	}
	return out, rows.Err()
}
