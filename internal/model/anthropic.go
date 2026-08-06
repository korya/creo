package model

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// Anthropic adapts the official SDK to the Gateway interface. The client reads
// ANTHROPIC_API_KEY from the environment — the key never leaves this package.
type Anthropic struct {
	client anthropic.Client
	model  string
}

func NewAnthropic(modelID string) *Anthropic {
	return &Anthropic{client: anthropic.NewClient(), model: modelID}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Complete(ctx context.Context, req Request) (*Completion, error) {
	maxTokens := int64(req.MaxTokens)
	if maxTokens == 0 {
		maxTokens = 16000
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: maxTokens,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	for _, t := range req.Tools {
		properties := t.InputSchema["properties"]
		var required []string
		if r, ok := t.InputSchema["required"].([]string); ok {
			required = r
		}
		tool := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: properties,
				Required:   required,
			},
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	for _, m := range req.Messages {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch b.Type {
			case BlockText:
				blocks = append(blocks, anthropic.NewTextBlock(b.Text))
			case BlockToolUse:
				var input any
				if err := json.Unmarshal(b.ToolInput, &input); err != nil {
					input = map[string]any{}
				}
				tu := anthropic.ToolUseBlockParam{ID: b.ToolID, Name: b.ToolName, Input: input}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{OfToolUse: &tu})
			case BlockToolResult:
				blocks = append(blocks, anthropic.NewToolResultBlock(b.ToolID, b.Content, b.IsError))
			}
		}
		switch m.Role {
		case RoleUser:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(blocks...))
		case RoleAssistant:
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(blocks...))
		default:
			return nil, fmt.Errorf("unsupported role %q", m.Role)
		}
	}

	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, err
	}
	comp := &Completion{
		StopReason: string(resp.StopReason),
		Usage:      Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
		Model:      string(resp.Model),
	}
	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			comp.Blocks = append(comp.Blocks, Block{Type: BlockText, Text: v.Text})
		case anthropic.ToolUseBlock:
			comp.Blocks = append(comp.Blocks, Block{
				Type:      BlockToolUse,
				ToolID:    v.ID,
				ToolName:  v.Name,
				ToolInput: json.RawMessage(v.JSON.Input.Raw()),
			})
		}
	}
	return comp, nil
}
