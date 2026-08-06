// Package harness is the AgentHarness component: the stateless model↔tool
// loop. Everything it knows arrives as input (events, workspace, gateway);
// everything it decides leaves as lease-fenced events. Log-first resume: a
// takeover reconstructs the conversation from the SessionLog and continues,
// re-executing any tool calls whose results never made it into the log.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/model"
	"github.com/korya/creo/internal/profile"
	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/run"
	"github.com/korya/creo/internal/tenant"
	"github.com/korya/creo/internal/workspace"
)

// DefaultProfile is the websites vertical (M3). Kept here so server/tests have
// one entry point; the definition lives in the profile package.
func DefaultProfile() profile.Profile { return profile.Websites() }

const (
	EvRunStarted      = "run.started"
	EvRunResumed      = "run.resumed"
	EvAssistant       = "assistant.message"
	EvToolResult      = "tool.result"
	EvVersionCreated  = "artifact.version.created"
	EvRunCompleted    = "run.completed"
	EvRunFailed       = "run.failed"
	EvRunCancelled    = "run.cancelled"
	EvUserMessage     = "user.message"
	EvInputRequested  = "input.requested"
	EvInputProvided   = "input.provided"
	EvStateChanged    = "session.state.changed"
	ToolAskUser       = "ask_user"
	askOneAtATimeNote = "Only one question at a time — ask this again after the current one is answered."
)

// ErrAwaitingInput ends a run's turn without failing it: the agent asked the
// user something. The caller parks the run (RunCoordinator.Await) and the
// question waits in the log until any device answers it (AC-5).
var ErrAwaitingInput = errors.New("awaiting user input")

// InputRequestDetail is the payload of an input.requested event. Choices are a
// convenience for the client; free text is always allowed.
type InputRequestDetail struct {
	ToolID   string   `json:"toolId"`
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
}

// InputProvidedDetail ties an answer back to the question's tool call, so a
// resumed run sees an ordinary tool result rather than a dangling call.
type InputProvidedDetail struct {
	ToolID string `json:"toolId"`
}

type assistantDetail struct {
	Blocks []model.Block `json:"blocks"`
}

type toolResultDetail struct {
	ToolID   string `json:"toolId"`
	ToolName string `json:"toolName"`
	Content  string `json:"content"`
	IsError  bool   `json:"isError"`
}

type Harness struct {
	Log        *eventlog.Log
	Projects   *project.Store
	Workspaces *workspace.Provider
	Gateway    *model.Metered
	Profile    profile.Profile
}

// Execute drives one run to completion. It returns the plain-language final
// text; on error the caller marks the run failed. A stale lease error means a
// takeover happened — nothing this invocation did after losing the lease is
// visible in the log or the version history (workspace files are explicitly
// non-authoritative, so the new holder's Commit snapshots whatever is real).
func (h *Harness) Execute(ctx context.Context, r *run.Run) (string, error) {
	lease := &r.Lease
	sessionID := r.SessionID

	// Capability-by-construction: refuse a palette that exceeds the profile's
	// execution level before doing any work (docs/components.md §10).
	if err := h.Profile.ValidatePalette(); err != nil {
		return "", err
	}

	prior, err := h.Log.Read(ctx, sessionID, 0, nil)
	if err != nil {
		return "", err
	}
	// A run is resuming iff it already announced a start. Keyed on the start
	// event specifically, not on "any event mentioning this run" — lifecycle
	// and state events also carry the run id, and must not fake a takeover.
	startType := EvRunStarted
	for _, e := range prior {
		if e.RunID == r.ID && (e.Type == EvRunStarted || e.Type == EvRunResumed) {
			startType = EvRunResumed
			break
		}
	}
	if _, err := h.Log.Append(ctx, sessionID, []eventlog.NewEvent{{Type: startType, RunID: r.ID}}, lease); err != nil {
		return "", err
	}

	ws, err := h.Workspaces.Open(r.ProjectID)
	if err != nil {
		return "", err
	}
	// Workspace-loss recovery: an empty workspace with committed history is
	// rebuilt from the latest version before the agent touches it.
	if files, err := ws.ListFiles(); err == nil && len(files) == 0 {
		if latest, err := h.Projects.Latest(ctx, r.ProjectID); err == nil && latest != "" {
			if err := h.Projects.Materialize(ctx, r.ProjectID, latest, ws); err != nil {
				return "", fmt.Errorf("workspace recovery: %w", err)
			}
		}
	}

	msgs := reconstruct(prior)
	if len(msgs) == 0 {
		return "", fmt.Errorf("run %s: no user message in session %s", r.ID, sessionID)
	}

	var finalText string
	for it := 0; it < h.Profile.MaxIterations; it++ {
		if pending := pendingTools(msgs); len(pending) > 0 {
			var resultEvents []eventlog.NewEvent
			var resultBlocks []model.Block
			var question *model.Block // the first ask_user, handled after the rest
			for _, call := range pending {
				if call.ToolName == ToolAskUser {
					// One question at a time: the first parks the run; any extras
					// get an ordinary tool result so no call dangles unanswered.
					if question == nil {
						question = &call
						continue
					}
					resultEvents = append(resultEvents, eventlog.NewEvent{
						Type: EvToolResult, RunID: r.ID,
						Detail: toolResultDetail{ToolID: call.ToolID, ToolName: call.ToolName, Content: askOneAtATimeNote},
					})
					resultBlocks = append(resultBlocks, model.Block{
						Type: model.BlockToolResult, ToolID: call.ToolID, Content: askOneAtATimeNote,
					})
					continue
				}
				content, isErr := h.executeTool(ws, call)
				// Plain-language progress for the user, authored by the profile
				// (R-AGT-2). Only on success — failed/blocked tools stay silent.
				var progress string
				if !isErr {
					progress = h.Profile.ProgressPhrase(call.ToolName, toolPath(call))
				}
				resultEvents = append(resultEvents, eventlog.NewEvent{
					Type:     EvToolResult,
					RunID:    r.ID,
					UserText: progress,
					Detail: toolResultDetail{
						ToolID: call.ToolID, ToolName: call.ToolName, Content: content, IsError: isErr,
					},
				})
				resultBlocks = append(resultBlocks, model.Block{
					Type: model.BlockToolResult, ToolID: call.ToolID, Content: content, IsError: isErr,
				})
			}
			if len(resultEvents) > 0 {
				if _, err := h.Log.Append(ctx, sessionID, resultEvents, lease); err != nil {
					return "", err
				}
				msgs = appendToolResults(msgs, resultBlocks)
			}
			if question != nil {
				// Commit the work done so far before parking: the user may take
				// days to answer, and what the agent already built is theirs.
				if err := h.commitProgress(ctx, r, ws); err != nil {
					return "", err
				}
				if err := h.emitQuestion(ctx, r, lease, *question); err != nil {
					return "", err
				}
				return "", ErrAwaitingInput
			}
			continue
		}

		comp, err := h.Gateway.Complete(ctx, r.ID, model.Request{
			System:   h.Profile.SystemPrompt(),
			Messages: msgs,
			Tools:    h.Profile.Tools,
		})
		if err != nil {
			return "", fmt.Errorf("model call: %w", err)
		}
		uiText := joinText(comp.Blocks)
		final := comp.StopReason != model.StopToolUse
		// On the final turn the completion message is delivered once, via
		// run.completed. The final assistant.message carries context (Blocks),
		// not UI text, so the client doesn't render it twice.
		assistantText := uiText
		if final {
			assistantText = ""
		}
		if _, err := h.Log.Append(ctx, sessionID, []eventlog.NewEvent{{
			Type: EvAssistant, RunID: r.ID,
			UserText: assistantText,
			Detail:   assistantDetail{Blocks: comp.Blocks},
		}}, lease); err != nil {
			return "", err
		}
		msgs = append(msgs, model.Message{Role: model.RoleAssistant, Blocks: comp.Blocks})
		if final {
			finalText = uiText
			break
		}
	}
	if finalText == "" {
		finalText = "I stopped after reaching the step limit for one request. The work so far is saved."
	}

	versionID, err := h.Projects.Commit(ctx, r.ProjectID, ws, r.TriggerEventID)
	if err != nil {
		return "", fmt.Errorf("version commit: %w", err)
	}
	closing := []eventlog.NewEvent{
		{Type: EvVersionCreated, RunID: r.ID, Detail: map[string]string{"versionId": versionID}},
		{Type: EvRunCompleted, RunID: r.ID, UserText: finalText},
	}
	if _, err := h.Log.Append(ctx, sessionID, closing, lease); err != nil {
		return "", err
	}
	return finalText, nil
}

// emitQuestion records the agent's question. userText is the question itself,
// verbatim — it was authored for a non-coder by the model under the profile's
// instructions, and no client rewrites it (R-AGT-2).
func (h *Harness) emitQuestion(ctx context.Context, r *run.Run, lease *eventlog.Lease, call model.Block) error {
	var in struct {
		Question string   `json:"question"`
		Choices  []string `json:"choices"`
	}
	if len(call.ToolInput) > 0 {
		json.Unmarshal(call.ToolInput, &in)
	}
	if strings.TrimSpace(in.Question) == "" {
		in.Question = "Could you tell me a bit more about what you'd like?"
	}
	_, err := h.Log.Append(ctx, r.SessionID, []eventlog.NewEvent{{
		Type: EvInputRequested, RunID: r.ID, UserText: in.Question,
		Detail: InputRequestDetail{ToolID: call.ToolID, Question: in.Question, Choices: in.Choices},
	}}, lease)
	return err
}

// commitProgress snapshots the workspace mid-run, so a paused conversation
// still leaves the user with everything built so far. Silent when the
// workspace is unchanged — Commit is content-addressed.
func (h *Harness) commitProgress(ctx context.Context, r *run.Run, ws *workspace.Workspace) error {
	versionID, err := h.Projects.Commit(ctx, r.ProjectID, ws, r.TriggerEventID)
	if err != nil {
		return fmt.Errorf("version commit: %w", err)
	}
	_, err = h.Log.Append(ctx, r.SessionID, []eventlog.NewEvent{
		{Type: EvVersionCreated, RunID: r.ID, Detail: map[string]string{"versionId": versionID}},
	}, &r.Lease)
	return err
}

// EmitFailure records a plain-language failure event (best-effort; a stale
// lease means the takeover worker owns the narrative now). Error translation
// happens here, at emit time — clients render userText, never interpret.
func (h *Harness) EmitFailure(ctx context.Context, r *run.Run, cause error) {
	text := "Something went wrong while working on your site. Your project is safe — please try again."
	switch {
	case errors.Is(cause, tenant.ErrBudgetExceeded):
		text = "The AI budget for this account is used up for today. Your project is safe — try again tomorrow, or ask whoever runs this server to raise the limit."
	case errors.Is(cause, tenant.ErrStorageExceeded):
		text = "There's no room left to save more changes. Your site is safe as it is — ask whoever runs this server for more space, or remove a few images to free some up."
	}
	h.Log.Append(ctx, r.SessionID, []eventlog.NewEvent{{
		Type: EvRunFailed, RunID: r.ID,
		UserText: text,
		Detail:   map[string]string{"error": cause.Error()},
	}}, &r.Lease)
}

// toolPath extracts the "path" argument from a tool call (for progress phrasing).
func toolPath(call model.Block) string {
	if len(call.ToolInput) == 0 {
		return ""
	}
	var in struct {
		Path string `json:"path"`
	}
	json.Unmarshal(call.ToolInput, &in)
	return in.Path
}

func (h *Harness) executeTool(ws *workspace.Workspace, call model.Block) (string, bool) {
	var input struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if len(call.ToolInput) > 0 {
		if err := json.Unmarshal(call.ToolInput, &input); err != nil {
			return "Error: invalid tool input: " + err.Error(), true
		}
	}
	switch call.ToolName {
	case "list_files":
		files, err := ws.ListFiles()
		if err != nil {
			return "Error: " + err.Error(), true
		}
		if len(files) == 0 {
			return "(empty workspace)", false
		}
		return strings.Join(files, "\n"), false
	case "read_file":
		b, err := ws.ReadFile(input.Path)
		if err != nil {
			return "Error: " + err.Error(), true
		}
		return string(b), false
	case "write_file":
		if err := ws.WriteFile(input.Path, []byte(input.Content)); err != nil {
			return "Error: " + err.Error(), true
		}
		return fmt.Sprintf("wrote %s (%d bytes)", input.Path, len(input.Content)), false
	case "delete_file":
		if err := ws.DeleteFile(input.Path); err != nil {
			return "Error: " + err.Error(), true
		}
		return "deleted " + input.Path, false
	default:
		return fmt.Sprintf("Error: unknown tool %q", call.ToolName), true
	}
}

// reconstruct rebuilds the conversation from the event log (log-first resume).
func reconstruct(events []eventlog.Event) []model.Message {
	var msgs []model.Message
	var pendingResults []model.Block
	flush := func() {
		if len(pendingResults) > 0 {
			msgs = append(msgs, model.Message{Role: model.RoleUser, Blocks: pendingResults})
			pendingResults = nil
		}
	}
	for _, e := range events {
		switch e.Type {
		case EvUserMessage:
			flush()
			msgs = append(msgs, model.Message{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: e.UserText}}})
		case EvAssistant:
			flush()
			var d assistantDetail
			if err := json.Unmarshal(e.Detail, &d); err == nil {
				msgs = append(msgs, model.Message{Role: model.RoleAssistant, Blocks: d.Blocks})
			}
		case EvToolResult:
			var d toolResultDetail
			if err := json.Unmarshal(e.Detail, &d); err == nil {
				pendingResults = append(pendingResults, model.Block{
					Type: model.BlockToolResult, ToolID: d.ToolID, ToolName: d.ToolName, Content: d.Content, IsError: d.IsError,
				})
			}
		case EvInputProvided:
			// The human's answer IS the ask_user tool's result. Pairing it here
			// means a resumed run sees an ordinary completed tool call — the
			// pause is invisible to the model, however long it lasted.
			var d InputProvidedDetail
			if err := json.Unmarshal(e.Detail, &d); err == nil && d.ToolID != "" {
				pendingResults = append(pendingResults, model.Block{
					Type: model.BlockToolResult, ToolID: d.ToolID, ToolName: ToolAskUser, Content: e.UserText,
				})
			}
		}
	}
	flush()
	return msgs
}

// pendingTools returns tool_use calls from the last assistant message that
// have no tool_result yet — the exact set a resumed run must re-execute.
func pendingTools(msgs []model.Message) []model.Block {
	lastAssistant := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant {
			lastAssistant = i
			break
		}
	}
	if lastAssistant == -1 {
		return nil
	}
	resolved := map[string]bool{}
	for _, m := range msgs[lastAssistant+1:] {
		for _, b := range m.Blocks {
			if b.Type == model.BlockToolResult {
				resolved[b.ToolID] = true
			}
		}
	}
	var pending []model.Block
	for _, b := range msgs[lastAssistant].Blocks {
		if b.Type == model.BlockToolUse && !resolved[b.ToolID] {
			pending = append(pending, b)
		}
	}
	return pending
}

// appendToolResults merges results into a trailing tool_result user message,
// or starts one — mirroring how reconstruct groups them.
func appendToolResults(msgs []model.Message, results []model.Block) []model.Message {
	if n := len(msgs); n > 0 && msgs[n-1].Role == model.RoleUser && len(msgs[n-1].Blocks) > 0 && msgs[n-1].Blocks[0].Type == model.BlockToolResult {
		msgs[n-1].Blocks = append(msgs[n-1].Blocks, results...)
		return msgs
	}
	return append(msgs, model.Message{Role: model.RoleUser, Blocks: results})
}

func joinText(blocks []model.Block) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == model.BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
