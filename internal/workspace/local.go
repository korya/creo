// Package workspace is the L0 SandboxProvider: a plain directory per project
// with file tools and no execution. Capability by absence — there is no exec
// method, so no prompt injection can invoke one. Paths are untrusted model
// output and are confined to the workspace root.
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrPathEscape = errors.New("path escapes workspace")

type Provider struct {
	root string // e.g. <data>/workspaces
}

func NewProvider(root string) (*Provider, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Provider{root: root}, nil
}

// Open returns the workspace for a project, creating its directory if needed.
func (p *Provider) Open(projectID string) (*Workspace, error) {
	dir := filepath.Join(p.root, projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Workspace{dir: dir}, nil
}

// Destroy removes a workspace entirely. Workspaces are never authoritative:
// the next Open + ProjectStore.Materialize rebuilds it from the last version.
func (p *Provider) Destroy(projectID string) error {
	return os.RemoveAll(filepath.Join(p.root, projectID))
}

type Workspace struct {
	dir string
}

func (w *Workspace) Dir() string { return w.dir }

// resolve confines a model-supplied path to the workspace root.
func (w *Workspace) resolve(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, p)
	}
	full := filepath.Join(w.dir, filepath.Clean(p))
	rel, err := filepath.Rel(w.dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, p)
	}
	return full, nil
}

func (w *Workspace) ListFiles() ([]string, error) {
	var out []string
	err := filepath.WalkDir(w.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != w.dir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(w.dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

func (w *Workspace) ReadFile(path string) ([]byte, error) {
	full, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (w *Workspace) WriteFile(path string, content []byte) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, content, 0o644)
}

func (w *Workspace) DeleteFile(path string) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	return os.Remove(full)
}

// Clear removes all content, keeping the directory (used by Materialize).
func (w *Workspace) Clear() error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(w.dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
