// Package tenant is the minimal IdentityService plus per-tenant budget and
// quota queries: who is calling, and whether they may spend. Tokens are the
// tenant principal in M1 (user objects arrive with the human-login surface).
package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/store"
)

var (
	ErrUnauthorized    = errors.New("unauthorized")
	ErrBudgetExceeded  = errors.New("daily token budget exceeded")
	ErrStorageExceeded = errors.New("storage limit reached")
	ErrNotFound        = errors.New("tenant not found")
)

type Tenant struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	DailyTokenLimit   *int64 `json:"dailyTokenLimit,omitempty"` // nil = unlimited
	MaxStorageBytes   *int64 `json:"maxStorageBytes,omitempty"` // nil = unlimited
	MaxConcurrentRuns int64  `json:"maxConcurrentRuns"`
}

type Store struct {
	db *store.DB
}

func New(db *store.DB) *Store { return &Store{db: db} }

func (s *Store) Create(ctx context.Context, name string, dailyTokenLimit *int64, maxConcurrentRuns int64, maxStorageBytes *int64) (Tenant, error) {
	if maxConcurrentRuns <= 0 {
		maxConcurrentRuns = 2
	}
	t := Tenant{ID: "t_" + ulid.Make().String(), Name: name, DailyTokenLimit: dailyTokenLimit, MaxConcurrentRuns: maxConcurrentRuns, MaxStorageBytes: maxStorageBytes}
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO tenants (id, name, daily_token_limit, max_concurrent_runs, max_storage_bytes, created_at) VALUES (?,?,?,?,?,?)`,
			t.ID, t.Name, t.DailyTokenLimit, t.MaxConcurrentRuns, t.MaxStorageBytes, now(),
		)
		return err
	})
	return t, err
}

func (s *Store) Get(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := s.db.R.QueryRowContext(ctx,
		`SELECT id, name, daily_token_limit, max_concurrent_runs, max_storage_bytes FROM tenants WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.DailyTokenLimit, &t.MaxConcurrentRuns, &t.MaxStorageBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Store) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.R.QueryContext(ctx,
		`SELECT id, name, daily_token_limit, max_concurrent_runs, max_storage_bytes FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.DailyTokenLimit, &t.MaxConcurrentRuns, &t.MaxStorageBytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateToken mints a bearer token. The plaintext is returned exactly once;
// only its SHA-256 is stored.
func (s *Store) CreateToken(ctx context.Context, tenantID, name string) (plaintext, tokenID string, err error) {
	if _, err := s.Get(ctx, tenantID); err != nil {
		return "", "", err
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	plaintext = "creo_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	tokenID = "tok_" + ulid.Make().String()
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO api_tokens (id, tenant_id, name, token_sha256, created_at) VALUES (?,?,?,?,?)`,
			tokenID, tenantID, name, hex.EncodeToString(sum[:]), now(),
		)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return plaintext, tokenID, nil
}

func (s *Store) RevokeToken(ctx context.Context, tokenID string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now(), tokenID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("token %s: %w", tokenID, ErrNotFound)
		}
		return nil
	})
}

// Authenticate resolves a plaintext bearer token to its tenant. The lookup is
// by SHA-256 digest, so a timing side-channel only leaks hash comparisons.
func (s *Store) Authenticate(ctx context.Context, plaintext string) (string, error) {
	sum := sha256.Sum256([]byte(plaintext))
	var tenantID string
	err := s.db.R.QueryRowContext(ctx,
		`SELECT tenant_id FROM api_tokens WHERE token_sha256 = ? AND revoked_at IS NULL`,
		hex.EncodeToString(sum[:])).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnauthorized
	}
	return tenantID, err
}

// SpentToday sums the tenant's recorded usage (input+output tokens) since
// midnight UTC.
func (s *Store) SpentToday(ctx context.Context, tenantID string) (int64, error) {
	dayStart := time.Now().UTC().Truncate(24 * time.Hour).Format(time.RFC3339Nano)
	var spent int64
	err := s.db.R.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(u.input_tokens + u.output_tokens), 0)
		FROM usage u
		JOIN runs r ON r.id = u.run_id
		JOIN projects p ON p.id = r.project_id
		WHERE p.tenant_id = ? AND u.created_at >= ?`, tenantID, dayStart).Scan(&spent)
	return spent, err
}

// CheckBudget is the hard stop (R-LLM-5): a tenant at or over its daily limit
// gets no new model calls until the window resets.
func (s *Store) CheckBudget(ctx context.Context, tenantID string) error {
	t, err := s.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	if t.DailyTokenLimit == nil {
		return nil
	}
	spent, err := s.SpentToday(ctx, tenantID)
	if err != nil {
		return err
	}
	if spent >= *t.DailyTokenLimit {
		return fmt.Errorf("%w: spent %d of %d today", ErrBudgetExceeded, spent, *t.DailyTokenLimit)
	}
	return nil
}

// StorageUsed is the bytes a tenant's saved versions actually occupy. Content
// is addressed by hash, so the same file across ten versions is counted once —
// billing a user ten times for one unchanged logo would be a lie.
func (s *Store) StorageUsed(ctx context.Context, tenantID string) (int64, error) {
	var used int64
	err := s.db.R.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(size), 0) FROM (
			SELECT DISTINCT vf.blob_sha, vf.size
			FROM version_files vf
			JOIN projects p ON p.id = vf.project_id
			WHERE p.tenant_id = ?
		)`, tenantID).Scan(&used)
	return used, err
}

// CheckStorage vets a pending commit against the tenant's limit (R-TEN-3).
// Only content the tenant does not already hold counts toward the increase,
// matching how the store actually grows.
func (s *Store) CheckStorage(ctx context.Context, tenantID string, blobs map[string]int64) error {
	t, err := s.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	if t.MaxStorageBytes == nil {
		return nil
	}
	used, err := s.StorageUsed(ctx, tenantID)
	if err != nil {
		return err
	}
	var added int64
	for sha, size := range blobs {
		var held int
		err := s.db.R.QueryRowContext(ctx, `
			SELECT 1 FROM version_files vf JOIN projects p ON p.id = vf.project_id
			WHERE p.tenant_id = ? AND vf.blob_sha = ? LIMIT 1`, tenantID, sha).Scan(&held)
		if errors.Is(err, sql.ErrNoRows) {
			added += size
		} else if err != nil {
			return err
		}
	}
	if used+added > *t.MaxStorageBytes {
		return fmt.Errorf("%w: %d bytes stored, %d more needed, limit %d",
			ErrStorageExceeded, used, added, *t.MaxStorageBytes)
	}
	return nil
}

// TenantOfProject resolves a project to its owning tenant (the storage hook).
func (s *Store) TenantOfProject(ctx context.Context, projectID string) (string, error) {
	var tenantID string
	err := s.db.R.QueryRowContext(ctx,
		`SELECT tenant_id FROM projects WHERE id = ?`, projectID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tenantID, err
}

// TenantOfRun resolves a run to its owning tenant (used by the budget hook).
func (s *Store) TenantOfRun(ctx context.Context, runID string) (string, error) {
	var tenantID string
	err := s.db.R.QueryRowContext(ctx, `
		SELECT p.tenant_id FROM runs r JOIN projects p ON p.id = r.project_id WHERE r.id = ?`, runID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tenantID, err
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
