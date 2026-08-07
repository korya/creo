package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/model"
	"github.com/korya/creo/internal/profile"
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

// Fix-02: the completion text is emitted exactly once (on run.completed), and
// the final assistant.message carries no UI text (only Blocks, for context).
func TestCompletionMessageNotDuplicated(t *testing.T) {
	fake, _ := model.FakeScript("site")
	f := setup(t, fake)
	ctx := context.Background()
	r := f.claimRun(t, "w1")
	text, err := f.h.Execute(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := f.log.Read(ctx, "s1", 0, nil)

	// Exactly one logged event carries the completion text as UserText.
	var carrying, lastAssistantText int
	for _, e := range evs {
		if e.UserText == text {
			carrying++
		}
	}
	// The last assistant.message before completion has empty UserText.
	for _, e := range evs {
		if e.Type == EvAssistant {
			if e.UserText == "" {
				lastAssistantText = 0
			} else {
				lastAssistantText = 1
			}
		}
	}
	if carrying != 1 {
		t.Fatalf("completion text %q appears on %d events, want exactly 1", text, carrying)
	}
	if lastAssistantText != 0 {
		t.Fatal("final assistant.message should carry no UI text (delivered via run.completed)")
	}
	// run.completed still carries it (existing contract).
	rc, _ := f.log.Read(ctx, "s1", 0, []string{EvRunCompleted})
	if len(rc) != 1 || rc[0].UserText != text {
		t.Fatalf("run.completed must carry the completion text: %+v", rc)
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

// gateArtifacts wires the same mint gate the server wires, so these tests
// exercise the real path rather than a harness-local approximation.
func (f *fixture) gateArtifacts() {
	p := f.h.Profile
	f.h.Projects.Validate = func(files []project.File) error {
		sizes := make(map[string]int64, len(files))
		for _, fl := range files {
			sizes[fl.Path] = fl.Size
		}
		return p.ValidateArtifact(sizes)
	}
}

func page(path, body string) model.FakeToolCall {
	return model.FakeToolCall{Name: "write_file", Input: map[string]any{"path": path, "content": body}}
}

func (f *fixture) events(t *testing.T) []eventlog.Event {
	t.Helper()
	evs, err := f.log.Read(context.Background(), "s1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func countType(evs []eventlog.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// The agent forgets the home page, is told, and fixes it. The user sees one
// completion and one acknowledgment — never a failure.
func TestRepairsMissingHomePage(t *testing.T) {
	gw := &model.Fake{ScriptName: "repairs", Steps: []model.FakeStep{
		{Text: "Styling first.", Tools: []model.FakeToolCall{page("style.css", "body{}")}},
		{Text: "All set."}, // declares done with no page — triggers the repair
		{Text: "Added the home page.", Tools: []model.FakeToolCall{page("index.html", "<h1>Hi</h1>")}},
		{Text: "Your site is ready."},
	}}
	f := setup(t, gw)
	f.gateArtifacts()
	r := f.claimRun(t, "w1")

	text, err := f.h.Execute(context.Background(), r)
	if err != nil {
		t.Fatalf("repair should have rescued the run: %v", err)
	}
	if text == "" {
		t.Fatal("no completion text")
	}
	evs := f.events(t)
	if n := countType(evs, EvRunFailed); n != 0 {
		t.Fatalf("a repaired run must not surface a failure (%d)", n)
	}
	if n := countType(evs, EvRepairStarted); n != 1 {
		t.Fatalf("want 1 repair.started, got %d", n)
	}
	if n := countType(evs, EvRepairCompleted); n != 1 {
		t.Fatalf("want 1 repair.completed, got %d", n)
	}
	if n := countType(evs, EvVersionCreated); n != 1 {
		t.Fatalf("want 1 version, got %d", n)
	}
	for _, e := range evs {
		switch e.Type {
		case EvRepairStarted:
			if e.UserText != "" {
				t.Fatalf("repair.started must render nothing, got %q", e.UserText)
			}
		case EvRepairCompleted:
			if e.UserText != repairAcknowledgment {
				t.Fatalf("acknowledgment = %q", e.UserText)
			}
			// Acknowledge the time, not the artifact.
			if strings.Contains(strings.ToLower(e.UserText), "home page") {
				t.Fatalf("acknowledgment should not name the home page: %q", e.UserText)
			}
		}
	}
}

// A model that cannot produce a page exhausts the budget and fails honestly,
// leaving no version behind for anyone to publish.
func TestRepairExhaustedFailsWithoutMintingAVersion(t *testing.T) {
	gw := &model.Fake{ScriptName: "hopeless", Steps: []model.FakeStep{
		{Text: "Styling only.", Tools: []model.FakeToolCall{page("style.css", "body{}")}},
		{Text: "All done."},
	}}
	f := setup(t, gw)
	f.gateArtifacts()
	r := f.claimRun(t, "w1")

	_, err := f.h.Execute(context.Background(), r)
	if !errors.Is(err, profile.ErrArtifactInvalid) {
		t.Fatalf("err = %v, want ErrArtifactInvalid", err)
	}
	evs := f.events(t)
	if n := countType(evs, EvRepairStarted); n != maxRepairs {
		t.Fatalf("want %d repair attempts, got %d", maxRepairs, n)
	}
	if n := countType(evs, EvVersionCreated); n != 0 {
		t.Fatalf("an unservable run minted %d version(s)", n)
	}
	if n := countType(evs, EvRunCompleted); n != 0 {
		t.Fatal("an unservable run reported completion")
	}

	// And the failure the caller emits is plain language, not a Go error.
	f.h.EmitFailure(context.Background(), r, err)
	last := f.events(t)
	found := false
	for _, e := range last {
		if e.Type == EvRunFailed {
			found = true
			if strings.Contains(e.UserText, "artifact") || strings.Contains(e.UserText, "commit") {
				t.Fatalf("failure text leaks implementation: %q", e.UserText)
			}
		}
	}
	if !found {
		t.Fatal("no run.failed emitted")
	}
}

// The budget lives in the log, so a worker taking over mid-repair inherits the
// attempts already spent rather than starting fresh.
func TestRepairBudgetSurvivesTakeover(t *testing.T) {
	gw := &model.Fake{ScriptName: "hopeless", Steps: []model.FakeStep{
		{Text: "Styling only.", Tools: []model.FakeToolCall{page("style.css", "body{}")}},
		{Text: "All done."},
	}}
	f := setup(t, gw)
	f.gateArtifacts()
	r := f.claimRun(t, "w1")

	// Pretend a previous worker already spent the whole budget on this run.
	ctx := context.Background()
	for range maxRepairs {
		if _, err := f.log.Append(ctx, "s1", []eventlog.NewEvent{{
			Type: EvRepairStarted, RunID: r.ID,
			Detail: RepairDetail{Reason: "no index.html", Instruction: "Create it now."},
		}}, &r.Lease); err != nil {
			t.Fatal(err)
		}
	}
	before := countType(f.events(t), EvRepairStarted)

	if _, err := f.h.Execute(ctx, r); !errors.Is(err, profile.ErrArtifactInvalid) {
		t.Fatalf("err = %v, want ErrArtifactInvalid", err)
	}
	if after := countType(f.events(t), EvRepairStarted); after != before {
		t.Fatalf("takeover granted %d extra repair attempt(s)", after-before)
	}
}

// A logged repair instruction is part of the conversation, so a resumed run
// sees it in order and does not simply re-declare itself finished.
func TestReconstructReplaysRepairInstruction(t *testing.T) {
	instruction := "The site is not finished yet: no index.html."
	detail, err := json.Marshal(RepairDetail{Reason: "no index.html", Instruction: instruction})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := json.Marshal(assistantDetail{Blocks: []model.Block{{Type: model.BlockText, Text: "All done."}}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := reconstruct([]eventlog.Event{
		{Type: EvUserMessage, UserText: "build me a site"},
		{Type: EvAssistant, Detail: assistant},
		{Type: EvRepairStarted, Detail: detail},
	})
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(msgs), msgs)
	}
	last := msgs[2]
	if last.Role != model.RoleUser || last.Blocks[0].Text != instruction {
		t.Fatalf("repair instruction not replayed as a user turn: %+v", last)
	}
}
