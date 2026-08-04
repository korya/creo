package harness

import "github.com/korya/creo/internal/model"

// Profile is the M0 stand-in for the ProductProfile component: system prompt +
// tool palette. The palette maps 1:1 onto Workspace methods — L0 has no exec
// tool because the Workspace has no exec method (capability by absence).
type Profile struct {
	System        string
	Tools         []model.ToolDef
	MaxIterations int
}

// DefaultProfile is the embedded websites-v0 profile, ported from spike-01.
func DefaultProfile() Profile {
	pathProp := map[string]any{"type": "string", "description": "Relative path inside the site workspace"}
	return Profile{
		MaxIterations: 40,
		System: `You are the build engine of a website builder for people who cannot code. You edit a static website in the workspace using the provided tools.

Rules:
- The site is plain static HTML/CSS/JS with relative paths only. No frameworks, no build tools, no external network resources (no CDN fonts or scripts, no remote images). For images, create local SVG placeholder files in assets/.
- Always inspect the current site (list_files, read_file) before editing an existing site.
- Scope discipline: change what was asked and nothing else. Do not restyle, rewrite, or reorganize unrelated parts of the site.
- If a request needs server-side functionality (payments, databases, accounts), do not attempt it. Explain in plain language that the site is a simple static site and suggest a realistic alternative.
- Your final message is shown to a non-technical user: 1-3 short plain-language sentences about what changed. No file names, no code talk.`,
		Tools: []model.ToolDef{
			{
				Name:        "list_files",
				Description: "List every file in the site workspace (relative paths).",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
			{
				Name:        "read_file",
				Description: "Read a file from the site workspace.",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": pathProp},
					"required":   []string{"path"},
				},
			},
			{
				Name:        "write_file",
				Description: "Create or fully overwrite a file in the site workspace.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    pathProp,
						"content": map[string]any{"type": "string", "description": "Full new file content"},
					},
					"required": []string{"path", "content"},
				},
			},
			{
				Name:        "delete_file",
				Description: "Delete a file from the site workspace.",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": pathProp},
					"required":   []string{"path"},
				},
			},
		},
	}
}
