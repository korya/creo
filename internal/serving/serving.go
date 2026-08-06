// Package serving is the PreviewGateway's read side: an origin-isolated HTTP
// server (a different port from the product API) that streams artifact-version
// files straight from the content-addressed store, under a strict CSP.
//
// Trust-tier note (T1): preview access is a capability secret in the URL path,
// and preview + published sites share one origin keyed by path. Per-user auth
// and per-site origins arrive at T2 (docs/components.md §8).
package serving

import (
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/publish"
)

type Gateway struct {
	projects *project.Store
	publish  *publish.Store
	csp      string // from the vertical profile's artifact policy (R-PUB-3)
}

func New(projects *project.Store, pub *publish.Store, csp string) *Gateway {
	return &Gateway{projects: projects, publish: pub, csp: csp}
}

func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	// {path...} matches the empty remainder too, so these also serve the
	// directory root (which resolves to index.html in serveFile).
	mux.HandleFunc("GET /preview/{project}/{secret}/{version}/{path...}", g.servePreview)
	mux.HandleFunc("GET /sites/{slug}/{path...}", g.serveSite)
	return mux
}

func (g *Gateway) servePreview(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	if !g.publish.CheckPreviewSecret(r.Context(), projectID, r.PathValue("secret")) {
		http.NotFound(w, r)
		return
	}
	g.serveFile(w, r, projectID, r.PathValue("version"), r.PathValue("path"))
}

func (g *Gateway) serveSite(w http.ResponseWriter, r *http.Request) {
	live, err := g.publish.BySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	g.serveFile(w, r, live.ProjectID, live.VersionID, r.PathValue("path"))
}

// serveFile streams one file of a version from CAS. Only paths present in the
// version's manifest are served — no filesystem walk, no traversal surface.
func (g *Gateway) serveFile(w http.ResponseWriter, r *http.Request, projectID, versionID, reqPath string) {
	reqPath = strings.TrimPrefix(path.Clean("/"+reqPath), "/")
	if reqPath == "" || strings.HasSuffix(reqPath, "/") {
		reqPath = path.Join(reqPath, "index.html")
	}
	files, err := g.projects.VersionFiles(r.Context(), projectID, versionID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var match *project.File
	for i := range files {
		if files[i].Path == reqPath {
			match = &files[i]
			break
		}
	}
	if match == nil {
		http.NotFound(w, r)
		return
	}
	blob, err := g.projects.Open(match.BlobSHA)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer blob.Close()

	// CSP on every response — from the vertical profile (R-PUB-3).
	w.Header().Set("Content-Security-Policy", g.csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ct := mime.TypeByExtension(path.Ext(reqPath)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Headers are already out, so a failure here cannot become a status code.
	// It still matters: a read error means the content store is damaged, and a
	// visitor just received a half-rendered page.
	if _, err := io.Copy(w, blob); err != nil {
		slog.Warn("serving a site file was cut short", "path", reqPath, "err", err)
	}
}
