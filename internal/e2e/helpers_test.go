package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func httpGet(url string) (*http.Response, error) { return http.Get(url) }

func doAuthed(t *testing.T, e *env, token, method, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(method, e.url(path), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func statusFor(t *testing.T, e *env, token, method, path string) int {
	t.Helper()
	resp := doAuthed(t, e, token, method, path, nil, nil)
	resp.Body.Close()
	return resp.StatusCode
}

func createProject(t *testing.T, e *env, token, name string) (sessionID string) {
	t.Helper()
	resp := doAuthed(t, e, token, "POST", "/v1/projects", map[string]string{"name": name}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("createProject: HTTP %d", resp.StatusCode)
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(resp.Body).Decode(&p)
	return p.SessionID
}

func sayAuthed(t *testing.T, e *env, token, sessionID, text, key string) {
	t.Helper()
	resp := doAuthed(t, e, token, "POST", "/v1/sessions/"+sessionID+"/messages",
		map[string]string{"text": text}, map[string]string{"Idempotency-Key": key})
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("say: HTTP %d", resp.StatusCode)
	}
}

func eventsAuthed(t *testing.T, e *env, token, sessionID string) []event {
	t.Helper()
	resp := doAuthed(t, e, token, "GET", "/v1/sessions/"+sessionID+"/events?stream=false", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events: HTTP %d", resp.StatusCode)
	}
	var evs []event
	json.NewDecoder(resp.Body).Decode(&evs)
	return evs
}

func waitForAuthed(t *testing.T, e *env, token, sessionID string, timeout time.Duration, pred func([]event) bool) []event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs := eventsAuthed(t, e, token, sessionID)
		if pred(evs) {
			return evs
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not reached within %s", timeout)
	return nil
}

func waitCompletedAuthed(t *testing.T, e *env, token, sessionID string, n int) []event {
	return waitForAuthed(t, e, token, sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= n
	})
}

func waitCompleted(t *testing.T, e *env, token, sessionID string, n int) []event {
	return waitCompletedAuthed(t, e, token, sessionID, n)
}

func listProjects(t *testing.T, e *env, token string) []string {
	t.Helper()
	resp := doAuthed(t, e, token, "GET", "/v1/projects", nil, nil)
	defer resp.Body.Close()
	var ps []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&ps)
	var out []string
	for _, p := range ps {
		out = append(out, p.ID+" "+p.Name)
	}
	return out
}
