// Package eventlog is the SessionLog component: the append-only, authoritative
// record of everything that happened in a session. Contracts SL-1..SL-5
// (docs/components.md §1) are enforced here and tested in this package.
package eventlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/store"
)

// ErrStaleLease is returned when an append carries a lease generation that has
// been superseded. Enforced in storage (SL-3): the append writes nothing.
var ErrStaleLease = errors.New("stale lease")

type Event struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	Seq       int64           `json:"seq"`
	RunID     string          `json:"runId,omitempty"`
	Type      string          `json:"type"`
	UserText  string          `json:"userText,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

type NewEvent struct {
	Type     string
	RunID    string
	UserText string
	Detail   any // marshalled to JSON; nil -> {}
}

// Lease fences appends: RunID+Gen must match the run's current lease.
type Lease struct {
	RunID    string
	WorkerID string
	Gen      int64
}

type Log struct {
	db *store.DB

	mu   sync.Mutex
	subs map[string]map[int]chan Event
	next int
}

func New(db *store.DB) *Log {
	return &Log{db: db, subs: map[string]map[int]chan Event{}}
}

// Append writes events atomically (SL-1) with gapless per-session sequence
// numbers (SL-2), rejecting stale leases before any write (SL-3). On success
// the created events (with their ids and seqs) are returned and published to
// live subscribers (SL-4).
func (l *Log) Append(ctx context.Context, sessionID string, evs []NewEvent, lease *Lease) ([]Event, error) {
	var appended []Event
	err := l.db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		appended, err = l.AppendTx(tx, sessionID, evs, lease)
		return err
	})
	if err != nil {
		return nil, err
	}
	l.Publish(sessionID, appended)
	return appended, nil
}

// AppendTx is Append inside a caller-owned transaction — used when an append
// must be atomic with other writes (e.g. idempotency-keyed submit). The caller
// must invoke Publish with the returned events after a successful commit.
func (l *Log) AppendTx(tx *sql.Tx, sessionID string, evs []NewEvent, lease *Lease) ([]Event, error) {
	if len(evs) == 0 {
		return nil, errors.New("empty append")
	}
	var appended []Event
	err := func() error {
		if lease != nil {
			var gen int64
			var worker string
			err := tx.QueryRow(`SELECT lease_gen, lease_worker FROM runs WHERE id = ?`, lease.RunID).Scan(&gen, &worker)
			if err != nil {
				return fmt.Errorf("lease lookup %s: %w", lease.RunID, err)
			}
			if gen != lease.Gen || worker != lease.WorkerID {
				return fmt.Errorf("%w: run %s holds gen=%d worker=%q, append carried gen=%d worker=%q",
					ErrStaleLease, lease.RunID, gen, worker, lease.Gen, lease.WorkerID)
			}
		}
		var base int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = ?`, sessionID).Scan(&base); err != nil {
			return err
		}
		now := time.Now().UTC()
		for i, ev := range evs {
			detail := []byte("{}")
			if ev.Detail != nil {
				var err error
				if detail, err = json.Marshal(ev.Detail); err != nil {
					return err
				}
			}
			e := Event{
				ID:        ulid.Make().String(),
				SessionID: sessionID,
				Seq:       base + int64(i) + 1,
				RunID:     ev.RunID,
				Type:      ev.Type,
				UserText:  ev.UserText,
				Detail:    detail,
				CreatedAt: now,
			}
			_, err := tx.Exec(
				`INSERT INTO events (session_id, seq, id, run_id, type, user_text, detail, created_at) VALUES (?,?,?,?,?,?,?,?)`,
				e.SessionID, e.Seq, e.ID, e.RunID, e.Type, e.UserText, string(e.Detail), e.CreatedAt.Format(time.RFC3339Nano),
			)
			if err != nil {
				return err
			}
			appended = append(appended, e)
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	return appended, nil
}

// Publish fans events out to live subscribers. Callers of AppendTx invoke it
// after their transaction commits; Append does it automatically.
func (l *Log) Publish(sessionID string, evs []Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ch := range l.subs[sessionID] {
		for _, e := range evs {
			select {
			case ch <- e:
			default: // slow subscriber: drop; it will re-read from its cursor on reconnect
			}
		}
	}
}

// Read returns events with seq > after, optionally filtered by type.
func (l *Log) Read(ctx context.Context, sessionID string, after int64, types []string) ([]Event, error) {
	q := `SELECT id, seq, COALESCE(run_id,''), type, user_text, detail, created_at FROM events WHERE session_id = ? AND seq > ?`
	args := []any{sessionID, after}
	if len(types) > 0 {
		q += ` AND type IN (?` + repeat(",?", len(types)-1) + `)`
		for _, t := range types {
			args = append(args, t)
		}
	}
	q += ` ORDER BY seq`
	rows, err := l.db.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var detail, created string
		if err := rows.Scan(&e.ID, &e.Seq, &e.RunID, &e.Type, &e.UserText, &detail, &created); err != nil {
			return nil, err
		}
		e.SessionID = sessionID
		e.Detail = json.RawMessage(detail)
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Subscribe delivers backfill from `after` followed by the live tail, deduped
// by seq (SL-4: at-least-once from the broker, exactly-once to the consumer).
func (l *Log) Subscribe(ctx context.Context, sessionID string, after int64) (<-chan Event, func()) {
	live := make(chan Event, 1024)
	l.mu.Lock()
	id := l.next
	l.next++
	if l.subs[sessionID] == nil {
		l.subs[sessionID] = map[int]chan Event{}
	}
	l.subs[sessionID][id] = live
	l.mu.Unlock()

	out := make(chan Event, 64)
	done := make(chan struct{})
	cancel := func() {
		l.mu.Lock()
		if m := l.subs[sessionID]; m != nil {
			delete(m, id)
		}
		l.mu.Unlock()
		close(done)
	}

	go func() {
		defer close(out)
		last := after
		backfill, err := l.Read(ctx, sessionID, after, nil)
		if err != nil {
			return
		}
		for _, e := range backfill {
			select {
			case out <- e:
				last = e.Seq
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
		for {
			select {
			case e := <-live:
				if e.Seq <= last {
					continue
				}
				select {
				case out <- e:
					last = e.Seq
				case <-done:
					return
				case <-ctx.Done():
					return
				}
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, cancel
}

func repeat(s string, n int) (out string) {
	for i := 0; i < n; i++ {
		out += s
	}
	return
}
