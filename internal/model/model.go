// Package model is the ModelGateway component: the single choke point for all
// LLM traffic. Provider credentials live here and nowhere else; usage is
// recorded per run even on failure.
package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/korya/creo/internal/store"
)

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"

	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"

	StopEndTurn = "end_turn"
	StopToolUse = "tool_use"
)

type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ToolID    string          `json:"toolId,omitempty"`
	ToolName  string          `json:"toolName,omitempty"`
	ToolInput json.RawMessage `json:"toolInput,omitempty"`
	Content   string          `json:"content,omitempty"` // tool_result payload
	IsError   bool            `json:"isError,omitempty"`
}

type Message struct {
	Role   string  `json:"role"`
	Blocks []Block `json:"blocks"`
}

type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any // full JSON schema: {type: object, properties, required}
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

type Request struct {
	System    string
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int
}

type Completion struct {
	Blocks     []Block
	StopReason string
	Usage      Usage
	Model      string
}

// Gateway is implemented by provider adapters (anthropic, fake; openai-compat later).
type Gateway interface {
	Complete(ctx context.Context, req Request) (*Completion, error)
	Name() string
}

// Metered wraps a Gateway and records usage per run — even for failed calls.
// Budget, when set, is the hard stop (R-LLM-5): checked before every call at
// the one point no model traffic can bypass.
type Metered struct {
	Inner  Gateway
	DB     *store.DB
	Budget func(ctx context.Context, runID string) error
}

func (m *Metered) Complete(ctx context.Context, runID string, req Request) (*Completion, error) {
	if m.Budget != nil {
		if err := m.Budget(ctx, runID); err != nil {
			return nil, err
		}
	}
	comp, err := m.Inner.Complete(ctx, req)
	var usage Usage
	modelName := ""
	if comp != nil {
		usage = comp.Usage
		modelName = comp.Model
	}
	recErr := m.DB.Write(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			`INSERT INTO usage (run_id, provider, model, input_tokens, output_tokens, created_at) VALUES (?,?,?,?,?,?)`,
			runID, m.Inner.Name(), modelName, usage.InputTokens, usage.OutputTokens,
			time.Now().UTC().Format(time.RFC3339Nano),
		)
		return e
	})
	if err != nil {
		return nil, err
	}
	if recErr != nil {
		return nil, recErr
	}
	return comp, nil
}
