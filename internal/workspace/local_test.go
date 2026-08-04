package workspace

import (
	"errors"
	"os"
	"testing"
)

// Path confinement: traversal attempts in model-supplied paths must fail.
func TestPathConfinement(t *testing.T) {
	wp, err := NewProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := wp.Open("p1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("ok/nested.txt", []byte("fine")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"../escape.txt",
		"a/../../escape.txt",
		"/etc/passwd",
		"..",
		"",
	} {
		if err := ws.WriteFile(p, []byte("evil")); !errors.Is(err, ErrPathEscape) {
			t.Errorf("WriteFile(%q): want ErrPathEscape, got %v", p, err)
		}
		if _, err := ws.ReadFile(p); !errors.Is(err, ErrPathEscape) {
			t.Errorf("ReadFile(%q): want ErrPathEscape, got %v", p, err)
		}
		if err := ws.DeleteFile(p); !errors.Is(err, ErrPathEscape) {
			t.Errorf("DeleteFile(%q): want ErrPathEscape, got %v", p, err)
		}
	}
}

// A symlink planted inside the workspace (out-of-band) pointing outside must
// not be readable or writable through.
func TestSymlinkContainment(t *testing.T) {
	root := t.TempDir()
	wp, _ := NewProvider(root)
	ws, _ := wp.Open("p1")

	outside := t.TempDir()
	if err := os.WriteFile(outside+"/secret.txt", []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ws.Dir()+"/evil"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ReadFile("evil/secret.txt"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("read through symlink: want ErrPathEscape, got %v", err)
	}
	if err := ws.WriteFile("evil/dropped.txt", []byte("x")); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("write through symlink: want ErrPathEscape, got %v", err)
	}
	if err := ws.DeleteFile("evil/secret.txt"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("delete through symlink: want ErrPathEscape, got %v", err)
	}
	if _, err := os.Stat(outside + "/secret.txt"); err != nil {
		t.Fatalf("outside file harmed: %v", err)
	}
}

func TestListReflectsWrites(t *testing.T) {
	wp, _ := NewProvider(t.TempDir())
	ws, _ := wp.Open("p1")
	ws.WriteFile("b.txt", []byte("b"))
	ws.WriteFile("a/x.txt", []byte("x"))
	files, err := ws.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != "a/x.txt" || files[1] != "b.txt" {
		t.Fatalf("got %v", files)
	}
}
