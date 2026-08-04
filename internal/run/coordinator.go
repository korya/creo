// Package run is the RunCoordinator component: it owns "who may act right now".
// Contracts RC-1..RC-5 (docs/components.md §2) are enforced here and tested in
// this package. Fencing composes with eventlog (SL-3): the lease generations
// issued here are what the SessionLog checks on every append.
package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/store"
)

var (
	ErrLeaseLost = errors.New("lease lost")
	ErrNotFound  = errors.New("run not found")
)

const (
	StatusQueued     = "queued"
	StatusRunning    = "running"
	StatusRecovering = "recovering"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

type Run struct {
	ID             string
	SessionID      string
	ProjectID      string
	TriggerEventID string
	Status         string
	Outcome        string
	Lease          eventlog.Lease
}

type RunRef struct {
	RunID   string
	Deduped bool
}

type Coordinator struct {
	db       *store.DB
	leaseTTL time.Duration
	wake     chan struct{}
}

func New(db *store.DB, leaseTTL time.Duration) *Coordinator {
	if leaseTTL <= 0 {
		leaseTTL = 15 * time.Second
	}
	return &Coordinator{db: db, leaseTTL: leaseTTL, wake: make(chan struct{}, 1)}
}

// Wake returns a channel that receives a signal when new work may be available.
func (c *Coordinator) Wake() <-chan struct{} { return c.wake }

// Poke nudges idle workers. Exposed for callers of RequestRunTx, who must
// poke after their transaction commits.
func (c *Coordinator) Poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// RequestRun registers a run for a trigger event. RC-1: a previously seen
// (sessionID, idempotencyKey) returns the original run and creates nothing.
func (c *Coordinator) RequestRun(ctx context.Context, sessionID, projectID, triggerEventID, idempotencyKey string) (RunRef, error) {
	var ref RunRef
	err := c.db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		ref, err = c.RequestRunTx(tx, sessionID, projectID, triggerEventID, idempotencyKey)
		return err
	})
	if err == nil && !ref.Deduped {
		c.Poke()
	}
	return ref, err
}

// RequestRunTx is RequestRun inside a caller-owned transaction, so a submit
// (event append + run registration + idempotency key) commits atomically.
func (c *Coordinator) RequestRunTx(tx *sql.Tx, sessionID, projectID, triggerEventID, idempotencyKey string) (RunRef, error) {
	var existing string
	err := tx.QueryRow(`SELECT run_id FROM idempotency_keys WHERE session_id = ? AND key = ?`, sessionID, idempotencyKey).Scan(&existing)
	if err == nil {
		return RunRef{RunID: existing, Deduped: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RunRef{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := "run_" + ulid.Make().String()
	if _, err := tx.Exec(
		`INSERT INTO runs (id, session_id, project_id, trigger_event_id, status, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		id, sessionID, projectID, triggerEventID, StatusQueued, now, now,
	); err != nil {
		return RunRef{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO idempotency_keys (session_id, key, run_id, created_at) VALUES (?,?,?,?)`,
		sessionID, idempotencyKey, id, now,
	); err != nil {
		return RunRef{}, err
	}
	return RunRef{RunID: id}, nil
}

// Claim hands one claimable run to a worker. RC-2: never while another run of
// the same project holds an unexpired lease. RC-3: each claim increments the
// lease generation, fencing out any previous holder.
func (c *Coordinator) Claim(ctx context.Context, workerID string) (*Run, error) {
	var claimed *Run
	err := c.db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		nowStr := now.Format(time.RFC3339Nano)
		// RC-2 per project, plus the M1 per-tenant concurrency quota: a run is
		// claimable only while its tenant is below max_concurrent_runs.
		row := tx.QueryRow(`
			SELECT r.id, r.session_id, r.project_id, r.trigger_event_id, r.status, r.lease_gen
			FROM runs r
			JOIN projects p ON p.id = r.project_id
			JOIN tenants t ON t.id = p.tenant_id
			WHERE r.status IN (?, ?)
			  AND NOT EXISTS (
			    SELECT 1 FROM runs o
			    WHERE o.project_id = r.project_id AND o.id != r.id
			      AND o.status = ? AND o.lease_expires_at > ?
			  )
			  AND (
			    SELECT COUNT(*) FROM runs o2
			    JOIN projects p2 ON p2.id = o2.project_id
			    WHERE p2.tenant_id = p.tenant_id
			      AND o2.status = ? AND o2.lease_expires_at > ?
			  ) < t.max_concurrent_runs
			ORDER BY r.created_at
			LIMIT 1`, StatusQueued, StatusRecovering, StatusRunning, nowStr, StatusRunning, nowStr)
		var r Run
		var gen int64
		if err := row.Scan(&r.ID, &r.SessionID, &r.ProjectID, &r.TriggerEventID, &r.Status, &gen); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		newGen := gen + 1
		expires := now.Add(c.leaseTTL).Format(time.RFC3339Nano)
		if _, err := tx.Exec(
			`UPDATE runs SET status = ?, lease_worker = ?, lease_gen = ?, lease_expires_at = ?, updated_at = ? WHERE id = ?`,
			StatusRunning, workerID, newGen, expires, nowStr, r.ID,
		); err != nil {
			return err
		}
		r.Status = StatusRunning
		r.Lease = eventlog.Lease{RunID: r.ID, WorkerID: workerID, Gen: newGen}
		claimed = &r
		return nil
	})
	return claimed, err
}

// Renew extends a held lease. Fails (RC-3) if the lease was superseded.
func (c *Coordinator) Renew(ctx context.Context, lease eventlog.Lease) error {
	return c.db.Write(ctx, func(tx *sql.Tx) error {
		expires := time.Now().UTC().Add(c.leaseTTL).Format(time.RFC3339Nano)
		res, err := tx.Exec(
			`UPDATE runs SET lease_expires_at = ? WHERE id = ? AND lease_worker = ? AND lease_gen = ? AND status = ?`,
			expires, lease.RunID, lease.WorkerID, lease.Gen, StatusRunning,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("%w: renew of run %s gen %d", ErrLeaseLost, lease.RunID, lease.Gen)
		}
		return nil
	})
}

// Complete moves a run to a terminal state; only the current lease holder can.
func (c *Coordinator) Complete(ctx context.Context, lease eventlog.Lease, status, outcome string) error {
	if status != StatusCompleted && status != StatusFailed {
		return fmt.Errorf("invalid terminal status %q", status)
	}
	err := c.db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := tx.Exec(
			`UPDATE runs SET status = ?, outcome = ?, lease_expires_at = '', updated_at = ? WHERE id = ? AND lease_worker = ? AND lease_gen = ? AND status = ?`,
			status, outcome, now, lease.RunID, lease.WorkerID, lease.Gen, StatusRunning,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("%w: complete of run %s gen %d", ErrLeaseLost, lease.RunID, lease.Gen)
		}
		return nil
	})
	if err == nil {
		c.Poke() // the project may have queued runs waiting on RC-2
	}
	return err
}

// RecoverOrphans moves expired-lease running runs to recovering (RC-4, RC-5).
func (c *Coordinator) RecoverOrphans(ctx context.Context) (int64, error) {
	var n int64
	err := c.db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := tx.Exec(
			`UPDATE runs SET status = ?, updated_at = ? WHERE status = ? AND lease_expires_at != '' AND lease_expires_at <= ?`,
			StatusRecovering, now, StatusRunning, now,
		)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err == nil && n > 0 {
		c.Poke()
	}
	return n, err
}

// Get returns a run's current state (for API/status use).
func (c *Coordinator) Get(ctx context.Context, runID string) (*Run, error) {
	row := c.db.R.QueryRow(`SELECT id, session_id, project_id, trigger_event_id, status, outcome FROM runs WHERE id = ?`, runID)
	var r Run
	if err := row.Scan(&r.ID, &r.SessionID, &r.ProjectID, &r.TriggerEventID, &r.Status, &r.Outcome); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}
