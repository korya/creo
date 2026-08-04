package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// S4: the SPA app shell is served at / from the binary, referencing its bundle;
// S5: the exact API loop the client makes (create -> say -> events -> preview
// -> publish) drives a site from blank to a live URL.
func TestWebClientServedAndLoop(t *testing.T) {
	e := newEnv(t, "fake:site") // --insecure; the browser dev posture
	e.start()

	// App shell at /.
	resp, body := getURL(t, e.url("/"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /: HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(body, "<title>Creo") {
		t.Fatalf("/ did not return the SPA shell: %.120q", body)
	}
	m := regexp.MustCompile(`src="\.?/?(assets/[^"]+\.js)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("SPA shell references no bundle: %.200q", body)
	}
	// The referenced bundle is served.
	if r, _ := getURL(t, e.url("/"+m[1]), ""); r.StatusCode != 200 {
		t.Fatalf("bundle %s not served: HTTP %d", m[1], r.StatusCode)
	}
	// Unknown client route falls back to the shell (SPA routing).
	if r, b := getURL(t, e.url("/some/client/route"), ""); r.StatusCode != 200 || !strings.Contains(b, "<title>Creo") {
		t.Fatalf("SPA fallback broken: HTTP %d", r.StatusCode)
	}

	// The no-code loop, via the same API the client calls.
	var p struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
	}
	cp := doAuthed(t, e, "", "POST", "/v1/projects", map[string]string{"name": "My site"}, nil)
	json.NewDecoder(cp.Body).Decode(&p)
	cp.Body.Close()

	sayAuthed(t, e, "", p.SessionID, "A site for my bakery with a menu", "web-k1")
	waitForAuthed(t, e, "", p.SessionID, 30_000_000_000, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})

	// Preview URL resolves and serves the built site.
	prevResp := doAuthed(t, e, "", "GET", "/v1/projects/"+p.ID+"/preview", nil, nil)
	var prev struct {
		URL string `json:"url"`
	}
	json.NewDecoder(prevResp.Body).Decode(&prev)
	prevResp.Body.Close()
	if r, b := getURL(t, prev.URL, ""); r.StatusCode != 200 || !strings.Contains(b, "<h1>") {
		t.Fatalf("preview did not serve the site: HTTP %d", r.StatusCode)
	}

	// Publish -> live URL serves the site with the profile CSP.
	pubResp := doAuthed(t, e, "", "POST", "/v1/projects/"+p.ID+"/publish", map[string]string{}, nil)
	var pub struct {
		URL string `json:"url"`
	}
	json.NewDecoder(pubResp.Body).Decode(&pub)
	pubResp.Body.Close()
	r, b := getURL(t, pub.URL, "")
	if r.StatusCode != 200 || !strings.Contains(b, "<h1>Home</h1>") {
		t.Fatalf("published site not live: HTTP %d body=%.80q", r.StatusCode, b)
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'none'") {
		t.Fatalf("published CSP not from profile: %q", csp)
	}
}

// The serving port must not serve the web client or any API (origin isolation).
func TestServingPortHasNoAppShell(t *testing.T) {
	e := newEnv(t, "fake:site")
	e.start()
	if r, b := getURL(t, e.serveURL("/"), ""); r.StatusCode == 200 && strings.Contains(b, "<title>Creo") {
		t.Fatal("serving port exposed the product web client")
	}
}

var _ = io.Discard
var _ = http.StatusOK
