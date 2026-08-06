package server

import "testing"

// The model spec is the operator's whole interface to provider choice, so its
// parsing is worth pinning: a typo must fail at startup with a legible
// message, never silently pick a different provider.
func TestBuildGateway(t *testing.T) {
	cases := []struct {
		spec     string
		wantName string
		wantErr  bool
	}{
		{spec: "fake:site", wantName: "fake:site"},
		{spec: "anthropic:claude-sonnet-5", wantName: "anthropic"},
		{spec: "anthropic", wantName: "anthropic"}, // defaults the model id
		{spec: "openai:gpt-5", wantName: "openai-compat"},
		{spec: "openai:qwen3@http://127.0.0.1:11434/v1", wantName: "openai-compat"},
		{spec: "openai:", wantErr: true},
		{spec: "openai", wantErr: true},
		{spec: "ollama:qwen3", wantErr: true},
		{spec: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			gw, err := buildGateway(c.spec)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q", c.spec)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gw.Name() != c.wantName {
				t.Fatalf("provider = %q, want %q", gw.Name(), c.wantName)
			}
		})
	}
}
