// Package webui embeds the built reference web client and serves it as the
// SPA app shell at /. The client is a thin consumer of the /v1 API — this
// package only serves static assets; all data flows through authenticated API
// calls the client makes.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Handler returns an http.Handler serving the web client. If dir is non-empty
// it serves from disk (dev: `npm run build` output without re-embedding);
// otherwise it serves the embedded bundle. Unknown non-asset paths fall back
// to index.html (SPA routing).
func Handler(dir string) (http.Handler, error) {
	var files fs.FS
	if dir != "" {
		files = os.DirFS(dir)
	} else {
		sub, err := fs.Sub(embedded, "dist")
		if err != nil {
			return nil, err
		}
		files = sub
	}
	fileServer := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(files, p); err != nil {
			// Not a real asset → serve the app shell (client-side routing).
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}
