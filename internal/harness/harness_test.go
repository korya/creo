package harness

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/model"
	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/run"
	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/workspace"
)

type fixture struct {
	db    *store.DB
	log   *eventlog.Log
	coord *run.Coordinator
	h     *Harness
}

func setup(t *testing.T, gw model.Gateway) *fixture {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	err = db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO projects (id, name, created_at) VALUES ('p1','test',?)`, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sessions (id, project_id, created_at) VALUES ('s1','p1',?)`, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	log := eventlog.New(db)
	ps, err := project.New(db, filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	wp, err := workspace.NewProvider(filepath.Join(dir, "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		db:    db,
		log:   log,
		coord: run.New(db, time.Minute),
		h: &Harness{
			Log:        log,
			Projects:   ps,
			Workspaces: wp,
			Gateway:    &model.Metered{Inner: gw, DB: db},
			Profile:    DefaultProfile(),
		},
	}
}

func (f *fixture) userMessage(t *testing.T, text string) string {
	t.Helper()
	evs, err := f.log.Append(context.Background(), "s1", []eventlog.NewEvent{{Type: EvUserMessage, UserText: text}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return evs[0].ID
}

func (f *fixture) claimRun(t *testing.T, worker string) *run.Run {
	t.Helper()
	ctx := context.Background()
	evID := f.userMessage(t, "build me a site")
	if _, err := f.coord.RequestRun(ctx, "s1", "p1", evID, "k-"+worker); err != nil {
		t.Fatal(err)
	}
	r, err := f.coord.Claim(ctx, worker)
	if err != nil || r == nil {
		t.Fatalf("claim: %+v %v", r, err)
	}
	return r
}

// A full scripted run: events emitted, files written, version committed.
func TestFullRun(t *testing.T) {
	fake, _ := model.FakeScript("site")
	f := setup(t, fake)
	ctx := context.Background()
	r := f.claimRun(t, "w1")

	text, err := f.h.Execute(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Fatal("no final text")
	}
	ws, _ := f.h.Workspaces.Open("p1")
	for _, p := range []string{"index.html", "style.css"} {
		if _, err := ws.ReadFile(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
	versions, _ := f.h.Projects.ListVersions(ctx, "p1")
	if len(versions) != 1 {
		t.Fatalf("want 1 version, got %d", len(versions))
	}
	evs, _ := f.log.Read(ctx, "s1", 0, []string{EvRunCompleted})
	if len(evs) != 1 || evs[0].UserText != text {
		t.Fatalf("run.completed missing or wrong: %+v", evs)
	}
}

// Progress: successful tool results carry a plain-language phrase for the UI,
// while the run's semantics (reconstruct/model context) are unchanged.
func TestToolResultsCarryProgress(t *testing.T) {
	fake, _ := model.FakeScript("site")
	f := setup(t, fake)
	ctx := context.Background()
	r := f.claimRun(t, "w1")
	if _, err := f.h.Execute(ctx, r); err != nil {
		t.Fatal(err)
	}
	evs, _ := f.log.Read(ctx, "s1", 0, []string{EvToolResult})
	if len(evs) == 0 {
		t.Fatal("no tool.result events")
	}
	var phrased int
	for _, e := range evs {
		if e.UserText != "" {
			phrased++
			if strings.Contains(e.UserText, "Home") { // capital canary must never appear
				t.Fatalf("progress phrase leaked capital 'Home': %q", e.UserText)
			}
		}
	}
	// fake:site writes index.html and style.css — both should be phrased.
	if phrased < 2 {
		t.Fatalf("expected >=2 phrased tool results, got %d of %d", phrased, len(evs))
	}
}

// Log-first resume: a takeover run re-executes unresolved tool calls from the
// log and continues the script exactly where the previous holder stopped.
func TestResumeWithPendingTools(t *testing.T) {
	fake, _ := model.FakeScript("site")
	f := setup(t, fake)
	ctx := context.Background()
	r := f.claimRun(t, "w1")

	// Simulate a worker that died right after emitting an assistant message
	// with a tool call, before any tool result was logged.
	_, err := f.log.Append(ctx, "s1", []eventlog.NewEvent{
		{Type: EvRunStarted, RunID: r.ID},
		{Type: EvAssistant, RunID: r.ID, UserText: "Creating your home page.", Detail: assistantDetail{Blocks: []model.Block{
			{Type: model.BlockText, Text: "Creating your home page."},
			{Type: model.BlockToolUse, ToolID: "tool_site_0_0", ToolName: "write_file",
				ToolInput: []byte(`{"path":"index.html","content":"<h1>Home</h1>"}`)},
		}}},
	}, &r.Lease)
	if err != nil {
		t.Fatal(err)
	}

	// Takeover: expire + recover + reclaim under a new worker/generation.
	f.expireLeases(t)
	if _, err := f.coord.RecoverOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	r2, err := f.coord.Claim(ctx, "w2")
	if err != nil || r2 == nil || r2.ID != r.ID {
		t.Fatalf("takeover claim: %+v %v", r2, err)
	}

	text, err := f.h.Execute(ctx, r2)
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Fatal("no final text after resume")
	}
	// The interrupted tool call was executed exactly once, and the script
	// continued from step 1 (style.css), not from the beginning.
	evs, _ := f.log.Read(ctx, "s1", 0, nil)
	var resumed, results, assistants int
	for _, e := range evs {
		switch e.Type {
		case EvRunResumed:
			resumed++
		case EvToolResult:
			results++
		case EvAssistant:
			assistants++
		}
	}
	if resumed != 1 {
		t.Fatalf("want 1 run.resumed, got %d", resumed)
	}
	if results != 2 { // index.html (replayed) + style.css
		t.Fatalf("want 2 tool results, got %d", results)
	}
	if assistants != 3 { // 1 pre-crash + step 1 + final step
		t.Fatalf("want 3 assistant messages, got %d", assistants)
	}
	ws, _ := f.h.Workspaces.Open("p1")
	if _, err := ws.ReadFile("style.css"); err != nil {
		t.Fatalf("script did not continue past crash point: %v", err)
	}
}

// A harness whose lease was taken over cannot contribute anything authoritative.
func TestStaleHarnessIsFenced(t *testing.T) {
	fake, _ := model.FakeScript("site")
	f := setup(t, fake)
	ctx := context.Background()
	r := f.claimRun(t, "w1")

	f.expireLeases(t)
	f.coord.RecoverOrphans(ctx)
	r2, _ := f.coord.Claim(ctx, "w2")
	if r2 == nil {
		t.Fatal("takeover claim failed")
	}

	// The zombie holder tries to run with its superseded lease.
	_, err := f.h.Execute(ctx, r)
	if !errors.Is(err, eventlog.ErrStaleLease) {
		t.Fatalf("want ErrStaleLease, got %v", err)
	}
	evs, _ := f.log.Read(ctx, "s1", 0, []string{EvRunStarted, EvRunResumed})
	if len(evs) != 0 {
		t.Fatalf("zombie emitted %d lifecycle events", len(evs))
	}
}

// expireLeases force-expires all leases (test-only, bypasses the clock).
func (f *fixture) expireLeases(t *testing.T) {
	t.Helper()
	err := f.db.Write(context.Background(), func(tx *sql.Tx) error {
		past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
		_, err := tx.Exec(`UPDATE runs SET lease_expires_at = ? WHERE status = 'running'`, past)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
