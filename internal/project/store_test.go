package project

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/workspace"
)

func setup(t *testing.T) (*Store, *workspace.Provider) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	err = db.Write(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := tx.Exec(`INSERT INTO projects (id, name, created_at) VALUES ('p1','test',?)`, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := New(db, filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	wp, err := workspace.NewProvider(filepath.Join(dir, "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	return ps, wp
}

// Round-trip: materialize(commit(w)) reproduces w byte-for-byte.
func TestCommitMaterializeRoundTrip(t *testing.T) {
	ps, wp := setup(t)
	ctx := context.Background()
	ws, _ := wp.Open("p1")
	files := map[string]string{
		"index.html":      "<h1>hi</h1>",
		"css/style.css":   "body { color: tomato }",
		"assets/hero.svg": "<svg/>",
	}
	for p, c := range files {
		if err := ws.WriteFile(p, []byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	v1, err := ps.Commit(ctx, "p1", ws, "ev1")
	if err != nil {
		t.Fatal(err)
	}

	// Mutate, commit v2, then restore v1 and verify bytes.
	ws.WriteFile("index.html", []byte("<h1>changed</h1>"))
	ws.DeleteFile("assets/hero.svg")
	v2, err := ps.Commit(ctx, "p1", ws, "ev2")
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 {
		t.Fatal("different content produced same version id")
	}
	if err := ps.Materialize(ctx, "p1", v1, ws); err != nil {
		t.Fatal(err)
	}
	got, _ := ws.ListFiles()
	want := []string{"assets/hero.svg", "css/style.css", "index.html"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files after restore: %v", got)
	}
	for p, c := range files {
		b, err := ws.ReadFile(p)
		if err != nil || string(b) != c {
			t.Fatalf("content mismatch for %s: %q %v", p, b, err)
		}
	}
}

// Content addressing: identical content => identical id, no duplicate version row.
func TestContentAddressing(t *testing.T) {
	ps, wp := setup(t)
	ctx := context.Background()
	ws, _ := wp.Open("p1")
	ws.WriteFile("a.txt", []byte("same"))
	v1, _ := ps.Commit(ctx, "p1", ws, "ev1")
	v2, _ := ps.Commit(ctx, "p1", ws, "ev2")
	if v1 != v2 {
		t.Fatalf("identical content: %s != %s", v1, v2)
	}
	versions, _ := ps.ListVersions(ctx, "p1")
	if len(versions) != 1 {
		t.Fatalf("duplicate version rows: %d", len(versions))
	}
}

// Versions record lineage and producing events.
func TestVersionLineage(t *testing.T) {
	ps, wp := setup(t)
	ctx := context.Background()
	ws, _ := wp.Open("p1")
	ws.WriteFile("a.txt", []byte("one"))
	v1, _ := ps.Commit(ctx, "p1", ws, "ev1")
	ws.WriteFile("a.txt", []byte("two"))
	v2, _ := ps.Commit(ctx, "p1", ws, "ev2")

	versions, _ := ps.ListVersions(ctx, "p1")
	if len(versions) != 2 {
		t.Fatalf("want 2 versions, got %d", len(versions))
	}
	if versions[1].ParentID != v1 || versions[1].ID != v2 || versions[1].ProducedByEvent != "ev2" {
		t.Fatalf("lineage wrong: %+v", versions[1])
	}
	latest, _ := ps.Latest(ctx, "p1")
	if latest != v2 {
		t.Fatalf("latest: %s", latest)
	}
}

// Workspace loss is survivable: destroy the dir, materialize from the store.
func TestWorkspaceLossRecovery(t *testing.T) {
	ps, wp := setup(t)
	ctx := context.Background()
	ws, _ := wp.Open("p1")
	ws.WriteFile("site.html", []byte("precious"))
	v1, _ := ps.Commit(ctx, "p1", ws, "ev1")

	if err := wp.Destroy("p1"); err != nil {
		t.Fatal(err)
	}
	ws2, _ := wp.Open("p1")
	if err := ps.Materialize(ctx, "p1", v1, ws2); err != nil {
		t.Fatal(err)
	}
	b, err := ws2.ReadFile("site.html")
	if err != nil || string(b) != "precious" {
		t.Fatalf("recovery failed: %q %v", b, err)
	}
}

func TestMaterializeUnknownVersion(t *testing.T) {
	ps, wp := setup(t)
	ws, _ := wp.Open("p1")
	err := ps.Materialize(context.Background(), "p1", "v_nope", ws)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// The mint gate: a refused artifact must leave the store exactly as it was —
// no blobs on disk, no rows — or a project stuck in a bad state would grow the
// store on every retry, and retrying is what people do.
func TestCommitRefusedByValidateWritesNothing(t *testing.T) {
	s, wp := setup(t)
	ctx := context.Background()
	ws, err := wp.Open("p1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("style.css", []byte("body{}")); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("not servable")
	var got []File
	s.Validate = func(files []File) error {
		got = files
		return sentinel
	}

	if _, err := s.Commit(ctx, "p1", ws, "e1"); !errors.Is(err, sentinel) {
		t.Fatalf("commit err = %v, want the injected policy error to propagate", err)
	}
	// The hook sees the manifest it needs to judge servability.
	if len(got) != 1 || got[0].Path != "style.css" || got[0].Size != 6 {
		t.Fatalf("hook received %+v, want one sized manifest entry", got)
	}

	var versions, files int
	if err := s.db.R.QueryRow(`SELECT COUNT(*) FROM versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := s.db.R.QueryRow(`SELECT COUNT(*) FROM version_files`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if versions != 0 || files != 0 {
		t.Fatalf("refused commit left %d versions / %d file rows", versions, files)
	}
	entries, err := os.ReadDir(s.casDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused commit wrote %d blob(s) to the content store", len(entries))
	}
}

// Servability is decided before capacity. A tenant near their storage limit
// mid-repair must hear "there is no home page yet" — which another turn can
// fix — rather than "you are out of space", which it cannot.
func TestValidateRunsBeforeQuota(t *testing.T) {
	s, wp := setup(t)
	ctx := context.Background()
	ws, _ := wp.Open("p1")
	if err := ws.WriteFile("style.css", []byte("body{}")); err != nil {
		t.Fatal(err)
	}

	invalid := errors.New("not servable")
	overQuota := errors.New("out of space")
	quotaCalled := false
	s.Validate = func([]File) error { return invalid }
	s.Quota = func(context.Context, string, []Blob) error {
		quotaCalled = true
		return overQuota
	}

	_, err := s.Commit(ctx, "p1", ws, "e1")
	if !errors.Is(err, invalid) {
		t.Fatalf("commit err = %v, want the servability refusal", err)
	}
	if errors.Is(err, overQuota) {
		t.Fatal("quota refusal masked the servability refusal")
	}
	if quotaCalled {
		t.Fatal("quota was consulted for an artifact that is not a site")
	}
}

// A nil hook is the pre-existing behaviour, so every other caller and test is
// unaffected by the gate's existence.
func TestCommitWithoutValidateHook(t *testing.T) {
	s, wp := setup(t)
	ws, _ := wp.Open("p1")
	if err := ws.WriteFile("style.css", []byte("body{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(context.Background(), "p1", ws, "e1"); err != nil {
		t.Fatalf("nil Validate must not gate: %v", err)
	}
}
