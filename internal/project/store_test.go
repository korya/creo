package project

import (
	"context"
	"database/sql"
	"errors"
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
