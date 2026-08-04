package eventlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/korya/creo/internal/store"
)

func testLog(t *testing.T) (*Log, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedSession(t, db, "s1")
	return New(db), db
}

func seedSession(t *testing.T, db *store.DB, sessionID string) {
	t.Helper()
	ctx := context.Background()
	err := db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO projects (id, name, created_at) VALUES ('p1','test',?)`, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sessions (id, project_id, created_at) VALUES (?,'p1',?)`, sessionID, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedRun(t *testing.T, db *store.DB, runID string, gen int64, worker string) {
	t.Helper()
	err := db.Write(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := tx.Exec(`INSERT INTO runs (id, session_id, project_id, trigger_event_id, status, lease_worker, lease_gen, created_at, updated_at)
			VALUES (?,'s1','p1','e0','running',?,?,?,?)`, runID, worker, gen, now, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// SL-1 + SL-3: a stale-lease append writes nothing, even for a multi-event batch.
func TestAppendStaleLeaseAtomic(t *testing.T) {
	l, _ := testLog(t)
	ctx := context.Background()
	seedRun(t, l.db, "r1", 3, "w1")

	batch := []NewEvent{{Type: "a"}, {Type: "b"}, {Type: "c"}}
	_, err := l.Append(ctx, "s1", batch, &Lease{RunID: "r1", WorkerID: "w1", Gen: 2})
	if !errors.Is(err, ErrStaleLease) {
		t.Fatalf("want ErrStaleLease, got %v", err)
	}
	evs, err := l.Read(ctx, "s1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("stale append leaked %d events", len(evs))
	}

	// Correct lease succeeds and writes the whole batch.
	if _, err := l.Append(ctx, "s1", batch, &Lease{RunID: "r1", WorkerID: "w1", Gen: 3}); err != nil {
		t.Fatal(err)
	}
	evs, _ = l.Read(ctx, "s1", 0, nil)
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
}

// SL-2: concurrent appends produce a gapless, strictly monotonic sequence.
func TestConcurrentAppendsGapless(t *testing.T) {
	l, _ := testLog(t)
	ctx := context.Background()
	const writers, each = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := l.Append(ctx, "s1", []NewEvent{{Type: fmt.Sprintf("w%d.%d", w, i)}}, nil); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	evs, err := l.Read(ctx, "s1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != writers*each {
		t.Fatalf("want %d events, got %d", writers*each, len(evs))
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("gap at index %d: seq %d", i, e.Seq)
		}
	}
}

// SL-4: subscriber sees backfill + live tail, in order, without duplicates.
func TestSubscribeBackfillAndLive(t *testing.T) {
	l, _ := testLog(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()

	for i := 0; i < 3; i++ {
		if _, err := l.Append(ctx, "s1", []NewEvent{{Type: fmt.Sprintf("pre%d", i)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ch, cancel := l.Subscribe(ctx, "s1", 0)
	defer cancel()

	go func() {
		for i := 0; i < 3; i++ {
			l.Append(ctx, "s1", []NewEvent{{Type: fmt.Sprintf("live%d", i)}}, nil)
		}
	}()

	var got []Event
	for e := range ch {
		got = append(got, e)
		if len(got) == 6 {
			break
		}
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("out of order or duplicated at %d: seq %d", i, e.Seq)
		}
	}
}

// Read filter by type.
func TestReadTypeFilter(t *testing.T) {
	l, _ := testLog(t)
	ctx := context.Background()
	l.Append(ctx, "s1", []NewEvent{{Type: "keep"}, {Type: "drop"}, {Type: "keep"}}, nil)
	evs, err := l.Read(ctx, "s1", 0, []string{"keep"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2, got %d", len(evs))
	}
}
