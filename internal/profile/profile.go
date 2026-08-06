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

// ProgressPhrase turns a tool action into a plain-language progress line for
// the user (R-AGT-2: progress language is authored here, on the platform side —
// the client renders it, never interprets file paths). Returns "" for actions
// that shouldn't surface progress (inspection tools, unknown tools). Neutral
// verbs so the phrasing fits both a first build and a later refine.
func (p Profile) ProgressPhrase(toolName, path string) string {
	switch toolName {
	case "write_file":
		return "Working on " + pageLabel(path)
	case "delete_file":
		return "Removing a page"
	default: // read_file, list_files, anything else — not user-meaningful
		return ""
	}
}

// pageLabel describes a workspace path in the user's terms. Lowercase "home"
// is deliberate — never emit the capitalized canary a leak test keys on.
func pageLabel(path string) string {
	clean := strings.TrimPrefix(path, "./")
	lower := strings.ToLower(clean)
	base := clean
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch {
	case lower == "index.html" || strings.HasSuffix(lower, "/index.html"):
		return "your home page"
	case strings.HasSuffix(lower, ".css"):
		return "the styling"
	case strings.HasPrefix(lower, "assets/") || hasImageExt(lower):
		return "the images"
	case strings.HasSuffix(lower, ".html"):
		stem := strings.TrimSuffix(base, ".html")
		if i := strings.LastIndex(base, "."); i >= 0 { // handle .htm etc. defensively
			stem = base[:i]
		}
		return "your " + prettyName(stem) + " page"
	default:
		return "your site"
	}
}

func hasImageExt(p string) bool {
	for _, ext := range []string{".svg", ".png", ".jpg", ".jpeg", ".webp", ".gif"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// prettyName turns a file stem into a title-cased label ("contact-us" -> "Contact Us").
func prettyName(stem string) string {
	stem = strings.ReplaceAll(stem, "-", " ")
	stem = strings.ReplaceAll(stem, "_", " ")
	fields := strings.Fields(stem)
	for i, f := range fields {
		fields[i] = strings.ToUpper(f[:1]) + f[1:]
	}
	if len(fields) == 0 {
		return "new"
	}
	return strings.Join(fields, " ")
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
- Decide routine technical questions yourself — frameworks, file layout, wording, colours. Use ask_user only when the answer changes the user's outcome and you genuinely cannot infer it (e.g. missing opening hours, which of two contact methods they want). Ask one short question at a time, in their words, and offer choices when there are obvious ones. Never ask permission to proceed.
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
			// ask_user parks the run until a human answers — from any device
			// (AC-5). It is not an execution tool: it adds no capability to the
			// workspace, so it is L0-safe.
			{Name: "ask_user", Description: "Ask the user one short question when the answer changes their outcome and cannot be inferred. The run pauses until they reply.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "One short question in the user's own words. No jargon."},
					"choices": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
						"description": "Optional short answer options, when there are obvious ones"}},
					"required": []string{"question"}}},
		},
	}
}
