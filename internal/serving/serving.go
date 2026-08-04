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
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/publish"
)

// StaticCSP forbids external resources — self-contained sites only. 'unsafe-inline'
// is allowed because the agent writes inline styles/scripts; tightened per profile later.
const StaticCSP = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:; connect-src 'none'; object-src 'none'; base-uri 'self'; form-action 'self'"

type Gateway struct {
	projects *project.Store
	publish  *publish.Store
}

func New(projects *project.Store, pub *publish.Store) *Gateway {
	return &Gateway{projects: projects, publish: pub}
}

func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
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

	// CSP on every response — static-site policy (R-PUB-3).
	w.Header().Set("Content-Security-Policy", StaticCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ct := mime.TypeByExtension(path.Ext(reqPath)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	io.Copy(w, blob)
}
