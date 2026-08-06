package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOAI stands in for any OpenAI-compatible server. It records the request
// the adapter sent and replies with a canned body.
type fakeOAI struct {
	*httptest.Server
	gotPath string
	gotAuth string
	gotBody oaiRequest
}

func newFakeOAI(t *testing.T, status int, response string) *fakeOAI {
	t.Helper()
	f := &fakeOAI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotPath = r.URL.Path
		f.gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &f.gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, response)
	}))
	t.Cleanup(f.Close)
	return f
}

func testRequest() Request {
	return Request{
		System: "You build websites.",
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "build me a bakery site"}}},
			{Role: RoleAssistant, Blocks: []Block{
				{Type: BlockText, Text: "Creating your home page."},
				{Type: BlockToolUse, ToolID: "call_1", ToolName: "write_file",
					ToolInput: json.RawMessage(`{"path":"index.html","content":"<h1>Hi</h1>"}`)},
			}},
			{Role: RoleUser, Blocks: []Block{
				{Type: BlockToolResult, ToolID: "call_1", ToolName: "write_file", Content: "wrote index.html"},
			}},
		},
		Tools: []ToolDef{
			{Name: "write_file", Description: "Write a file.", InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}},
			{Name: "list_files", Description: "List files."}, // no schema: must still be valid
		},
	}
}

func TestRequestMapping(t *testing.T) {
	f := newFakeOAI(t, 200, `{"model":"qwen","choices":[{"message":{"role":"assistant","content":"Done."},
		"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
	t.Setenv("CREO_OPENAI_KEY", "sk-test")
	o := NewOpenAICompat("qwen3", f.URL+"/v1")

	if _, err := o.Complete(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if f.gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", f.gotPath)
	}
	if f.gotAuth != "Bearer sk-test" {
		t.Fatalf("auth header = %q", f.gotAuth)
	}
	if f.gotBody.Model != "qwen3" {
		t.Fatalf("model = %q", f.gotBody.Model)
	}

	// The system prompt becomes a message; tool results become their own
	// top-level messages rather than blocks inside a user turn.
	var roles []string
	for _, m := range f.gotBody.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant", "tool"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	assistant := f.gotBody.Messages[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool call not carried: %+v", assistant)
	}
	// Arguments travel as a JSON *string* on this protocol, not an object.
	if !strings.HasPrefix(assistant.ToolCalls[0].Function.Arguments, `{"path"`) {
		t.Fatalf("arguments = %q", assistant.ToolCalls[0].Function.Arguments)
	}
	toolMsg := f.gotBody.Messages[3]
	if toolMsg.ToolCallID != "call_1" || toolMsg.Content != "wrote index.html" {
		t.Fatalf("tool result not carried: %+v", toolMsg)
	}
	// A tool declared without a schema must still present a valid object
	// schema — servers reject a null `parameters`.
	if len(f.gotBody.Tools) != 2 || f.gotBody.Tools[1].Function.Parameters["type"] != "object" {
		t.Fatalf("tool schemas = %+v", f.gotBody.Tools)
	}
}

func TestResponseMapping(t *testing.T) {
	f := newFakeOAI(t, 200, `{"model":"qwen3","choices":[{"message":{"role":"assistant",
		"content":"Adding your page.","tool_calls":[
		  {"id":"call_9","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.html\"}"}}
		]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":120,"completion_tokens":45}}`)
	o := NewOpenAICompat("qwen3", f.URL+"/v1")

	comp, err := o.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if comp.StopReason != StopToolUse {
		t.Fatalf("stop reason = %q", comp.StopReason)
	}
	if comp.Usage.InputTokens != 120 || comp.Usage.OutputTokens != 45 {
		t.Fatalf("usage = %+v — metering must work for every provider", comp.Usage)
	}
	if len(comp.Blocks) != 2 {
		t.Fatalf("blocks = %+v", comp.Blocks)
	}
	if comp.Blocks[0].Type != BlockText || comp.Blocks[0].Text != "Adding your page." {
		t.Fatalf("text block = %+v", comp.Blocks[0])
	}
	tu := comp.Blocks[1]
	if tu.Type != BlockToolUse || tu.ToolID != "call_9" || tu.ToolName != "write_file" {
		t.Fatalf("tool block = %+v", tu)
	}
	var args map[string]string
	if err := json.Unmarshal(tu.ToolInput, &args); err != nil || args["path"] != "a.html" {
		t.Fatalf("arguments did not survive the string encoding: %s", tu.ToolInput)
	}
}

// Local servers report finish_reason inconsistently — several say "stop" while
// emitting tool calls. Trusting the label there would end the run mid-build,
// so the presence of tool calls decides.
func TestStopReasonTrustsToolCallsOverLabel(t *testing.T) {
	f := newFakeOAI(t, 200, `{"model":"local","choices":[{"message":{"role":"assistant","content":"",
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"list_files","arguments":""}}]},
		"finish_reason":"stop"}],"usage":{}}`)
	o := NewOpenAICompat("local", f.URL+"/v1")

	comp, err := o.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if comp.StopReason != StopToolUse {
		t.Fatalf(`finish_reason "stop" with tool calls must still mean tool use, got %q`, comp.StopReason)
	}
	// Empty arguments are valid JSON by the time the harness sees them.
	if string(comp.Blocks[0].ToolInput) != "{}" {
		t.Fatalf("empty arguments = %q, want {}", comp.Blocks[0].ToolInput)
	}
}

func TestNoKeyMeansNoAuthHeader(t *testing.T) {
	f := newFakeOAI(t, 200, `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	t.Setenv("CREO_OPENAI_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	o := NewOpenAICompat("llama", f.URL+"/v1")

	if _, err := o.Complete(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if f.gotAuth != "" {
		t.Fatalf("sent an Authorization header to a keyless local server: %q", f.gotAuth)
	}
}

func TestErrorsAreLegible(t *testing.T) {
	t.Run("api error body", func(t *testing.T) {
		f := newFakeOAI(t, 400, `{"error":{"message":"model not found","type":"invalid_request_error"}}`)
		o := NewOpenAICompat("nope", f.URL+"/v1")
		_, err := o.Complete(context.Background(), testRequest())
		if err == nil || !strings.Contains(err.Error(), "model not found") {
			t.Fatalf("error = %v, want the server's message", err)
		}
	})

	t.Run("wrong base url returns html", func(t *testing.T) {
		f := newFakeOAI(t, 404, `<html><body>404 not found</body></html>`)
		o := NewOpenAICompat("qwen", f.URL+"/v1")
		_, err := o.Complete(context.Background(), testRequest())
		if err == nil || !strings.Contains(err.Error(), "non-JSON") {
			t.Fatalf("error = %v, want a hint that the endpoint is wrong", err)
		}
	})

	t.Run("empty choices", func(t *testing.T) {
		f := newFakeOAI(t, 200, `{"choices":[]}`)
		o := NewOpenAICompat("qwen", f.URL+"/v1")
		if _, err := o.Complete(context.Background(), testRequest()); err == nil {
			t.Fatal("want an error for a response with no choices")
		}
	})
}
