package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedOAI is a stand-in for Ollama / LM Studio / vLLM: it speaks the
// OpenAI chat-completions protocol and replies from a fixed script, choosing
// the step by how many assistant turns the transcript already contains.
//
// This is the closest CI can get to AC-14 without a GPU: the real binary, the
// real adapter, the real wire format. What it deliberately does NOT prove is
// that any particular model is *good* at the task — that is what
// scripts/demo-local-model.sh is for.
type scriptedOAI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
}

func newScriptedOAI(t *testing.T) *scriptedOAI {
	t.Helper()
	s := &scriptedOAI{}
	// Deliberately quirky in the ways local servers really are: finish_reason
	// says "stop" even while emitting tool calls, and arguments arrive as a
	// JSON-encoded string.
	steps := []string{
		`{"role":"assistant","content":"Creating your home page.","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"write_file",
			 "arguments":"{\"path\":\"index.html\",\"content\":\"<h1>Spoke</h1>\"}"}}]}`,
		`{"role":"assistant","content":"Adding styling.","tool_calls":[
			{"id":"c2","type":"function","function":{"name":"write_file",
			 "arguments":"{\"path\":\"style.css\",\"content\":\"body{font-family:serif}\"}"}}]}`,
		`{"role":"assistant","content":"Your site is ready — a home page with simple styling."}`,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(raw, &req)
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()

		assistants := 0
		if msgs, ok := req["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok && mm["role"] == "assistant" {
					assistants++
				}
			}
		}
		idx := min(assistants, len(steps)-1)
		finish := "stop" // never "tool_calls" — the adapter must not need it
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"scripted-local","choices":[{"message":%s,"finish_reason":%q}],
			"usage":{"prompt_tokens":100,"completion_tokens":25}}`, steps[idx], finish)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *scriptedOAI) lastRequest() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	return s.requests[len(s.requests)-1]
}

// AC-14: a second provider — reached over the OpenAI-compatible protocol that
// every self-hosted server speaks — drives the complete loop with no
// client-visible difference. The assertions are the same ones the Anthropic
// path satisfies, deliberately: that is what "no behavior change" means.
func TestOpenAICompatDrivesTheWholeLoop(t *testing.T) {
	oai := newScriptedOAI(t)
	e := newEnv(t, "openai:scripted-local@"+oai.URL+"/v1")
	e.start()

	projectID, sessionID := e.newProject("spoke")
	e.say(sessionID, "build me a bike shop site", "k1")

	evs := e.waitFor(sessionID, 30*time.Second, func(evs []event) bool {
		return count(evs, "run.completed") >= 1
	})
	if got := count(evs, "run.failed"); got != 0 {
		t.Fatalf("the local-model path failed %d time(s)", got)
	}
	if got := count(evs, "tool.result"); got < 2 {
		t.Fatalf("tool calling did not round-trip: %d tool results", got)
	}
	if got := count(evs, "artifact.version.created"); got < 1 {
		t.Fatal("no version produced")
	}
	done, _ := lastOfType(evs, "run.completed")
	if !strings.Contains(done.UserText, "ready") {
		t.Fatalf("final message = %q", done.UserText)
	}

	// Usage is metered for every provider, or budgets (R-LLM-5) would only
	// bind the provider we happened to test.
	var input, output int64
	e.queryDB(t, `SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0) FROM usage`, &input, &output)
	if input == 0 || output == 0 {
		t.Fatalf("no usage recorded for the openai-compat provider (in=%d out=%d)", input, output)
	}

	// The site really was built, and publishing serves it.
	var pub struct {
		URL string `json:"url"`
	}
	e.call("POST", "/v1/projects/"+projectID+"/publish", map[string]string{}, nil, &pub)
	resp, err := httpGet(pub.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Spoke") {
		t.Fatalf("published site does not contain the built content: %.120s", body)
	}

	// The profile's tools reached the model in the protocol's own shape —
	// including ask_user, so a local model can pause for the user too.
	req := oai.lastRequest()
	tools, _ := req["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		if m, ok := tl.(map[string]any); ok {
			if fn, ok := m["function"].(map[string]any); ok {
				names[fmt.Sprint(fn["name"])] = true
			}
		}
	}
	for _, want := range []string{"write_file", "read_file", "list_files", "ask_user"} {
		if !names[want] {
			t.Fatalf("tool %q not offered to the model; got %v", want, names)
		}
	}
	// The system prompt travels as a message on this protocol.
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages sent")
	}
	if first, ok := msgs[0].(map[string]any); !ok || first["role"] != "system" {
		t.Fatalf("first message = %v, want the system prompt", msgs[0])
	}
}

// queryDB runs a read-only query against the server's SQLite file via the
// sqlite3 CLI if present; otherwise it skips the assertion rather than
// failing on a missing dev tool.
func (e *env) queryDB(t *testing.T, query string, out ...*int64) {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 CLI not available; skipping the usage-metering assertion")
	}
	raw, err := exec.Command(bin, e.dataDir+"/creo.db", query).Output()
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	fields := strings.Split(strings.TrimSpace(string(raw)), "|")
	for i, p := range out {
		if i < len(fields) {
			fmt.Sscanf(fields[i], "%d", p)
		}
	}
}
