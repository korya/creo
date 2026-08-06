package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenAICompat speaks the OpenAI chat-completions protocol, which is the lingua
// franca of self-hosted inference: Ollama, LM Studio, vLLM, llama.cpp's server,
// OpenRouter, and OpenAI itself all serve it. One adapter, written against the
// wire format rather than a vendor SDK, is what keeps R-LLM-1 from becoming a
// per-provider maintenance tax.
//
// The API key lives here and nowhere else (§5.1); local servers usually need
// none, so an empty key is normal rather than an error.
type OpenAICompat struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
}

// NewOpenAICompat builds an adapter for a model at an OpenAI-compatible
// endpoint. baseURL points at the API root (…/v1); the key comes from
// CREO_OPENAI_KEY or OPENAI_API_KEY when set.
func NewOpenAICompat(modelID, baseURL string) *OpenAICompat {
	key := os.Getenv("CREO_OPENAI_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	return &OpenAICompat{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		model:   modelID,
		apiKey:  key,
		// Local models on modest hardware are slow; the harness's own context
		// governs cancellation, this is only a backstop against a hung socket.
		http: &http.Client{Timeout: 10 * time.Minute},
	}
}

func (o *OpenAICompat) Name() string { return "openai-compat" }

// --- wire types (only the fields the harness actually needs) ---

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Refusal    json.RawMessage `json:"refusal,omitempty"`
}

type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiFunctionCall `json:"function"`
}

type oaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON *string*, not an object
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type oaiRequest struct {
	Model     string       `json:"model"`
	Messages  []oaiMessage `json:"messages"`
	Tools     []oaiTool    `json:"tools,omitempty"`
	MaxTokens int          `json:"max_tokens,omitempty"`
}

type oaiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (o *OpenAICompat) Complete(ctx context.Context, req Request) (*Completion, error) {
	body := oaiRequest{Model: o.model, MaxTokens: req.MaxTokens, Messages: toOAIMessages(req)}
	for _, t := range req.Tools {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		body.Tools = append(body.Tools, oaiTool{
			Type:     "function",
			Function: oaiFunction{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}

	var out oaiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		// A non-JSON body is usually a proxy or a wrong base URL; say which,
		// with enough of the body to identify it and no more.
		return nil, fmt.Errorf("%s returned %s with a non-JSON body: %.200s", o.baseURL, resp.Status, raw)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", resp.Status, out.Error.Message)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s returned %s", o.baseURL, resp.Status)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("%s returned no choices", o.baseURL)
	}

	choice := out.Choices[0]
	comp := &Completion{
		Model:      out.Model,
		StopReason: stopReasonFor(choice.FinishReason, len(choice.Message.ToolCalls) > 0),
		Usage:      Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens},
	}
	if choice.Message.Content != "" {
		comp.Blocks = append(comp.Blocks, Block{Type: BlockText, Text: choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}" // some local models omit arguments for no-parameter tools
		}
		comp.Blocks = append(comp.Blocks, Block{
			Type:      BlockToolUse,
			ToolID:    tc.ID,
			ToolName:  tc.Function.Name,
			ToolInput: json.RawMessage(args),
		})
	}
	return comp, nil
}

// stopReasonFor normalizes finish_reason to the harness's vocabulary. Local
// servers are inconsistent here — some report "stop" even while emitting tool
// calls — so the presence of tool calls decides, and the label only breaks
// ties.
func stopReasonFor(finish string, hasToolCalls bool) string {
	if hasToolCalls {
		return StopToolUse
	}
	if finish == "tool_calls" || finish == "function_call" {
		return StopToolUse
	}
	return StopEndTurn
}

// toOAIMessages converts the harness's block-structured conversation to the
// OpenAI shape. The two models differ in one structural way: Anthropic carries
// tool results as blocks inside a user message, while OpenAI wants one
// top-level message per tool result. That flattening is the whole adapter.
func toOAIMessages(req Request) []oaiMessage {
	var msgs []oaiMessage
	if req.System != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		var text []string
		var calls []oaiToolCall
		var results []oaiMessage
		for _, b := range m.Blocks {
			switch b.Type {
			case BlockText:
				if b.Text != "" {
					text = append(text, b.Text)
				}
			case BlockToolUse:
				args := string(b.ToolInput)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				calls = append(calls, oaiToolCall{
					ID: b.ToolID, Type: "function",
					Function: oaiFunctionCall{Name: b.ToolName, Arguments: args},
				})
			case BlockToolResult:
				results = append(results, oaiMessage{
					Role: "tool", ToolCallID: b.ToolID, Name: b.ToolName, Content: b.Content,
				})
			}
		}
		if len(text) > 0 || len(calls) > 0 {
			msgs = append(msgs, oaiMessage{
				Role: m.Role, Content: strings.Join(text, "\n"), ToolCalls: calls,
			})
		}
		msgs = append(msgs, results...)
	}
	return msgs
}
