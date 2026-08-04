// Package profile is the ProductProfile component: a vertical defined as data —
// prompts, tool palette, execution level, artifact policy, vocabulary. A new
// product is configuration, not a fork; restrictions are enforced by the
// platform (ValidatePalette), not merely described to the model.
package profile

import (
	"fmt"
	"strings"

	"github.com/korya/creo/internal/model"
)

// ExecutionLevel is the sandbox capability ladder (docs/components.md §5):
// L0 no execution, L1 trusted tooling only, L2 arbitrary toolchain (container).
type ExecutionLevel string

const (
	L0 ExecutionLevel = "L0"
	L1 ExecutionLevel = "L1"
	L2 ExecutionLevel = "L2"
)

type Profile struct {
	ID             string
	Version        string
	System         string
	Tools          []model.ToolDef
	MaxIterations  int
	ExecutionLevel ExecutionLevel
	CSP            string // served-content Content-Security-Policy (R-PUB-3)
	SiteLanguage   string // explicit, never inferred (spike-01 finding)
}

// execTools names tool families that run arbitrary commands — permitted only
// at L2 (a container). Their presence in an L0/L1 palette is a policy error.
var execTools = []string{"bash", "exec", "shell", "run_command", "install"}

// ValidatePalette refuses a palette that exceeds the execution level: at L0/L1
// no tool may execute commands. This is capability-by-construction — the check
// runs before any run starts, so a profile cannot grant what the level forbids.
func (p Profile) ValidatePalette() error {
	if p.ExecutionLevel == L2 {
		return nil
	}
	for _, t := range p.Tools {
		name := strings.ToLower(t.Name)
		for _, ex := range execTools {
			if name == ex || strings.HasPrefix(name, ex+"_") {
				return fmt.Errorf("profile %s is %s but palette contains execution tool %q (needs L2)", p.ID, p.ExecutionLevel, t.Name)
			}
		}
	}
	return nil
}

// SystemPrompt renders the profile's system prompt with the site language
// substituted — the model is told the language, never left to infer it.
func (p Profile) SystemPrompt() string {
	lang := p.SiteLanguage
	if lang == "" {
		lang = "English"
	}
	return strings.ReplaceAll(p.System, "{{SiteLanguage}}", lang)
}

const defaultCSP = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:; connect-src 'none'; object-src 'none'; base-uri 'self'; form-action 'self'"

// Websites is the M3 websites vertical: static sites, no execution (L0),
// English by default. The palette is file-tools-only — capability by absence.
func Websites() Profile {
	pathProp := map[string]any{"type": "string", "description": "Relative path inside the site workspace"}
	return Profile{
		ID:             "websites",
		Version:        "0",
		ExecutionLevel: L0,
		CSP:            defaultCSP,
		SiteLanguage:   "English",
		MaxIterations:  40,
		System: `You are the build engine of a website builder for people who cannot code. You edit a static website in the workspace using the provided tools.

Rules:
- The site is plain static HTML/CSS/JS with relative paths only. No frameworks, no build tools, no external network resources (no CDN fonts or scripts, no remote images). For images, create local SVG placeholder files in assets/.
- Write all site text in {{SiteLanguage}} unless the user explicitly asks for another language.
- Always inspect the current site (list_files, read_file) before editing an existing site.
- Scope discipline: change what was asked and nothing else. Do not restyle, rewrite, or reorganize unrelated parts of the site.
- If a request needs server-side functionality (payments, databases, accounts), do not attempt it. Explain in plain language that the site is a simple static site and suggest a realistic alternative.
- Your final message is shown to a non-technical user: 1-3 short plain-language sentences about what changed. No file names, no code talk.`,
		Tools: []model.ToolDef{
			{Name: "list_files", Description: "List every file in the site workspace (relative paths).",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
			{Name: "read_file", Description: "Read a file from the site workspace.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": pathProp}, "required": []string{"path"}}},
			{Name: "write_file", Description: "Create or fully overwrite a file in the site workspace.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"path": pathProp, "content": map[string]any{"type": "string", "description": "Full new file content"}},
					"required": []string{"path", "content"}}},
			{Name: "delete_file", Description: "Delete a file from the site workspace.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": pathProp}, "required": []string{"path"}}},
		},
	}
}
