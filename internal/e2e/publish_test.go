package e2e

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (e *authEnv) projectIDFor(t *testing.T, token, name string) (projectID, sessionID string) {
	t.Helper()
	resp := doAuthed(t, e.env, token, "POST", "/v1/projects", map[string]string{"name": name}, nil)
	defer resp.Body.Close()
	var p struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(resp.Body).Decode(&p)
	return p.ID, p.SessionID
}

func postJSON(t *testing.T, e *env, token, path string, body any) map[string]string {
	t.Helper()
	resp := doAuthed(t, e, token, "POST", path, body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST %s: HTTP %d", path, resp.StatusCode)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func getURL(t *testing.T, url, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// The full M2 choreography: build -> publish -> serve live -> new version ->
// publish -> rollback -> export -> preview, with the origin-isolation and CSP
// assertions.
func TestPublishRollbackExportPreview(t *testing.T) {
	e := newAuthEnv(t, "fake:site")
	e.start()
	_, token := e.newTenantToken(t, "acme")
	projectID, sessionID := e.projectIDFor(t, token, "site")

	// Build v1.
	sayAuthed(t, e.env, token, sessionID, "build me a site", "k1")
	waitCompletedAuthed(t, e.env, token, sessionID, 1)
	assertServable(t, e.env, token, projectID)

	// Publish -> live URL serves the built HTML with a strict CSP (S2, S4).
	pub := postJSON(t, e.env, token, "/v1/projects/"+projectID+"/publish", nil)
	resp, body := getURL(t, pub["url"], "")
	if resp.StatusCode != 200 {
		t.Fatalf("live site: HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(body, "<h1>Home</h1>") {
		t.Fatalf("live site content: %q", body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "connect-src 'none'") {
		t.Fatalf("CSP missing or too loose: %q", csp)
	}
	// publish.completed event landed in the session log (S3).
	evs := eventsAuthed(t, e.env, token, sessionID)
	if count(evs, "publish.completed") != 1 {
		t.Fatalf("publish.completed events: %d", count(evs, "publish.completed"))
	}

	// Build v2 (changes index.html via the fake script's fresh run) and publish it.
	sayAuthed(t, e.env, token, sessionID, "add a page", "k2")
	waitCompletedAuthed(t, e.env, token, sessionID, 2)
	var versions []struct {
		ID string `json:"id"`
	}
	vresp := doAuthed(t, e.env, token, "GET", "/v1/projects/"+projectID+"/versions", nil, nil)
	json.NewDecoder(vresp.Body).Decode(&versions)
	vresp.Body.Close()
	if len(versions) < 2 {
		t.Fatalf("expected >=2 versions, got %d", len(versions))
	}
	v2 := versions[len(versions)-1].ID
	postJSON(t, e.env, token, "/v1/projects/"+projectID+"/publish", map[string]string{"versionId": v2})

	// Rollback -> live serves v1 again (S3). Its style.css must be present.
	rb := postJSON(t, e.env, token, "/v1/projects/"+projectID+"/rollback", nil)
	if resp, _ := getURL(t, rb["url"]+"style.css", ""); resp.StatusCode != 200 {
		t.Fatalf("rolled-back site missing style.css: HTTP %d", resp.StatusCode)
	}
	if count(eventsAuthed(t, e.env, token, sessionID), "publish.rolled_back") != 1 {
		t.Fatal("publish.rolled_back not logged")
	}

	// Export returns a valid zip whose entries match the version (S5).
	exResp := doAuthed(t, e.env, token, "GET", "/v1/projects/"+projectID+"/export", nil, nil)
	zipBytes, _ := io.ReadAll(exResp.Body)
	exResp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("export not a valid zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["index.html"] {
		t.Fatalf("export missing index.html; entries: %v", names)
	}

	// Preview URL serves the version; a wrong secret 404s (S1).
	prev := getPreview(t, e.env, token, projectID)
	if resp, body := getURL(t, prev, ""); resp.StatusCode != 200 || !strings.Contains(body, "<h1>") {
		t.Fatalf("preview: HTTP %d body=%q", resp.StatusCode, body)
	}
	tampered := tamperSecret(prev)
	if resp, _ := getURL(t, tampered, ""); resp.StatusCode != 404 {
		t.Fatalf("preview with wrong secret: HTTP %d (want 404)", resp.StatusCode)
	}

	// S6: the serving port exposes no product API.
	if resp, _ := getURL(t, e.serveURL("/v1/projects"), token); resp.StatusCode == 200 {
		t.Fatal("serving port exposed a /v1 route")
	}
}

func getPreview(t *testing.T, e *env, token, projectID string) string {
	t.Helper()
	resp := doAuthed(t, e, token, "GET", "/v1/projects/"+projectID+"/preview", nil, nil)
	defer resp.Body.Close()
	var out struct {
		URL string `json:"url"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.URL
}

// tamperSecret flips the secret segment of a /preview/<project>/<secret>/<version>/ URL.
func tamperSecret(previewURL string) string {
	parts := strings.Split(previewURL, "/")
	for i, p := range parts {
		if p == "preview" && i+2 < len(parts) {
			parts[i+2] = "deadbeefdeadbeefdeadbeefdeadbeef"
			break
		}
	}
	return strings.Join(parts, "/")
}

var _ = time.Second
