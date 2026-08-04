package workspace

import (
	"errors"
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
