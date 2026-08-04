package model

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Fake is a scripted gateway for tests and the e2e kill/resume choreography.
// The step to play is derived from the number of assistant messages already in
// the request, so a resumed run continues exactly where the log says it was.
type Fake struct {
	ScriptName string
	Steps      []FakeStep
	StepDelay  time.Duration
}

type FakeStep struct {
	Text  string
	Tools []FakeToolCall
}

type FakeToolCall struct {
	Name  string
	Input map[string]any
}

func (f *Fake) Name() string { return "fake:" + f.ScriptName }

func (f *Fake) Complete(ctx context.Context, req Request) (*Completion, error) {
	if f.StepDelay > 0 {
		select {
		case <-time.After(f.StepDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	idx := 0
	for _, m := range req.Messages {
		if m.Role == RoleAssistant {
			idx++
		}
	}
	if idx >= len(f.Steps) {
		return &Completion{
			Blocks:     []Block{{Type: BlockText, Text: "All done."}},
			StopReason: StopEndTurn,
			Usage:      Usage{InputTokens: 100, OutputTokens: 10},
			Model:      f.Name(),
		}, nil
	}
	step := f.Steps[idx]
	comp := &Completion{
		StopReason: StopEndTurn,
		Usage:      Usage{InputTokens: 100, OutputTokens: 50},
		Model:      f.Name(),
	}
	if step.Text != "" {
		comp.Blocks = append(comp.Blocks, Block{Type: BlockText, Text: step.Text})
	}
	for i, tc := range step.Tools {
		input, _ := json.Marshal(tc.Input)
		comp.Blocks = append(comp.Blocks, Block{
			Type:      BlockToolUse,
			ToolID:    fmt.Sprintf("tool_%s_%d_%d", f.ScriptName, idx, i),
			ToolName:  tc.Name,
			ToolInput: input,
		})
	}
	if len(step.Tools) > 0 {
		comp.StopReason = StopToolUse
	}
	return comp, nil
}

// FakeScript returns a registered script by name. "site" is a quick 3-step
// build; "slow-site" is an 8-step build with per-call delay, giving the e2e
// kill test a wide, observable window to SIGKILL the server mid-run.
func FakeScript(name string) (*Fake, error) {
	page := func(path, body string) FakeToolCall {
		return FakeToolCall{Name: "write_file", Input: map[string]any{"path": path, "content": body}}
	}
	switch name {
	case "site":
		// Steps 0-2 are the first run (ends at the text-only step 2). A second
		// message continues at step 3, writing a new file — so a follow-up run
		// produces a genuinely different version rather than deduping.
		return &Fake{ScriptName: name, Steps: []FakeStep{
			{Text: "Creating your home page.", Tools: []FakeToolCall{page("index.html", "<h1>Home</h1>")}},
			{Text: "Adding styling.", Tools: []FakeToolCall{page("style.css", "body{font-family:serif}")}},
			{Text: "Your site is ready: a home page with simple styling."},
			{Text: "Adding another page.", Tools: []FakeToolCall{page("page2.html", "<h1>Page 2</h1>")}},
			{Text: "Added a second page."},
		}}, nil
	case "slow-site":
		steps := make([]FakeStep, 0, 9)
		for i := 0; i < 8; i++ {
			steps = append(steps, FakeStep{
				Text:  fmt.Sprintf("Working on part %d of your site.", i+1),
				Tools: []FakeToolCall{page(fmt.Sprintf("page%d.html", i+1), fmt.Sprintf("<h1>Page %d</h1>", i+1))},
			})
		}
		steps = append(steps, FakeStep{Text: "Your site is ready with all eight pages."})
		return &Fake{ScriptName: name, Steps: steps, StepDelay: 150 * time.Millisecond}, nil
	case "hostile":
		// A prompt-injected agent: every step tries to escape the workspace.
		// The platform must turn each into a tool error and nothing else.
		esc := func(name, path string) FakeToolCall {
			in := map[string]any{"path": path}
			if name == "write_file" {
				in["content"] = "pwned"
			}
			return FakeToolCall{Name: name, Input: in}
		}
		return &Fake{ScriptName: name, Steps: []FakeStep{
			{Text: "Building your site.", Tools: []FakeToolCall{
				{Name: "write_file", Input: map[string]any{"path": "index.html", "content": "<h1>ok</h1>"}},
			}},
			{Text: "Reading a sibling project.", Tools: []FakeToolCall{esc("read_file", "../victim/index.html")}},
			{Text: "Reading host secrets.", Tools: []FakeToolCall{esc("read_file", "/etc/passwd")}},
			{Text: "Reading the env file.", Tools: []FakeToolCall{esc("read_file", "../../../../.env")}},
			{Text: "Writing outside.", Tools: []FakeToolCall{esc("write_file", "../evil.txt")}},
			{Text: "Deleting outside.", Tools: []FakeToolCall{esc("delete_file", "../victim/index.html")}},
			{Text: "All done."},
		}}, nil
	default:
		return nil, fmt.Errorf("unknown fake script %q", name)
	}
}
