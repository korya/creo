package profile

import (
	"errors"
	"strings"
	"testing"

	"github.com/korya/creo/internal/model"
)

func TestWebsitesIsL0AndValid(t *testing.T) {
	p := Websites()
	if p.ExecutionLevel != L0 {
		t.Fatalf("websites should be L0, got %s", p.ExecutionLevel)
	}
	if err := p.ValidatePalette(); err != nil {
		t.Fatalf("websites palette should validate: %v", err)
	}
	if p.CSP == "" || !strings.Contains(p.CSP, "connect-src 'none'") {
		t.Fatalf("websites CSP missing or too loose: %q", p.CSP)
	}
}

func TestValidatePaletteRefusesExecAtL0(t *testing.T) {
	p := Websites()
	p.Tools = append(p.Tools, model.ToolDef{Name: "bash", Description: "run commands"})
	if err := p.ValidatePalette(); err == nil {
		t.Fatal("L0 profile with a bash tool must fail validation")
	}
	// Prefix form is also caught.
	p2 := Websites()
	p2.Tools = append(p2.Tools, model.ToolDef{Name: "run_command", Description: "x"})
	if err := p2.ValidatePalette(); err == nil {
		t.Fatal("L0 profile with run_command must fail validation")
	}
	// The same palette is allowed at L2.
	p.ExecutionLevel = L2
	if err := p.ValidatePalette(); err != nil {
		t.Fatalf("L2 should permit exec tools: %v", err)
	}
}

func TestProgressPhrase(t *testing.T) {
	p := Websites()
	cases := []struct{ tool, path, want string }{
		{"write_file", "index.html", "Working on your home page"},
		{"write_file", "about.html", "Working on your About page"},
		{"write_file", "contact-us.html", "Working on your Contact Us page"},
		{"write_file", "styles.css", "Working on the styling"},
		{"write_file", "assets/hero.svg", "Working on the images"},
		{"write_file", "photo.png", "Working on the images"},
		{"delete_file", "old.html", "Removing a page"},
		{"read_file", "index.html", ""}, // inspection: silent
		{"list_files", "", ""},          // inspection: silent
		{"bash", "whatever", ""},        // unknown: silent
	}
	for _, c := range cases {
		if got := p.ProgressPhrase(c.tool, c.path); got != c.want {
			t.Errorf("ProgressPhrase(%q,%q) = %q, want %q", c.tool, c.path, got, c.want)
		}
	}
	// Never emit the capitalized canary a leak test keys on.
	if strings.Contains(p.ProgressPhrase("write_file", "index.html"), "Home") {
		t.Fatal("home-page phrase must not contain capital 'Home'")
	}
}

func TestSiteLanguageSubstituted(t *testing.T) {
	p := Websites()
	p.SiteLanguage = "Dutch"
	got := p.SystemPrompt()
	if !strings.Contains(got, "Write all site text in Dutch") {
		t.Fatalf("site language not substituted: %q", got)
	}
	if strings.Contains(got, "{{SiteLanguage}}") {
		t.Fatal("placeholder left unsubstituted")
	}
}

// ValidateArtifact is the floor of the artifact policy: the palette check stops
// a vertical granting too much, this stops it claiming a site that a visitor
// would get a 404 from.
func TestValidateArtifact(t *testing.T) {
	web := Websites()
	cases := []struct {
		name  string
		files map[string]int64
		valid bool
	}{
		{"a real site", map[string]int64{"index.html": 2048, "css/style.css": 900}, true},
		{"home page only", map[string]int64{"index.html": 12}, true},
		{"styling but no page", map[string]int64{"css/style.css": 2114}, false},
		{"nothing at all", map[string]int64{}, false},
		// Written but empty is unservable in the only way that matters.
		{"empty home page", map[string]int64{"index.html": 0, "css/style.css": 900}, false},
		// The gateway joins index.html onto the root and nowhere else.
		{"nested home page", map[string]int64{"pages/index.html": 2048}, false},
		{"wrong case", map[string]int64{"Index.html": 2048}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := web.ValidateArtifact(c.files)
			if c.valid && err != nil {
				t.Fatalf("want servable, got %v", err)
			}
			if !c.valid {
				if err == nil {
					t.Fatal("want a refusal, got nil")
				}
				if !errors.Is(err, ErrArtifactInvalid) {
					t.Fatalf("refusal must be branchable with errors.Is: %v", err)
				}
				// The message names the file so the harness can tell the model
				// what to create.
				if !strings.Contains(err.Error(), "index.html") {
					t.Fatalf("refusal does not name the missing file: %v", err)
				}
			}
		})
	}
}

// A profile that requires nothing accepts anything — the gate is opt-in per
// vertical, not a platform-wide opinion about what a project must contain.
func TestValidateArtifactWithoutRequirements(t *testing.T) {
	var p Profile
	if err := p.ValidateArtifact(map[string]int64{}); err != nil {
		t.Fatalf("no requirements should accept an empty artifact: %v", err)
	}
}
