package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/profile"
	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/publish"
	"github.com/korya/creo/internal/store"
)

// gateFixture wires just enough of the API to exercise the publish handlers:
// a project with two versions, the second of which is unservable, and a
// publish store carrying the same gate the server wires.
//
// The invalid version is seeded through a project store with no Validate hook,
// because the point of this gate is versions that predate it — post-fix the
// product cannot create one, which is why this cannot be an e2e scenario.
func gateFixture(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	const projectID, sessionID = "p1", "s1"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = db.Write(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects (id, tenant_id, name, created_at) VALUES (?,'t_default','p',?)`, projectID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO sessions (id, tenant_id, project_id, created_at) VALUES (?,'t_default',?,?)`, sessionID, projectID, now); err != nil {
			return err
		}
		// Lineage v1 <- v2 <- v3, with the middle one unservable. That shape
		// reaches both refusal paths: publishing v2 directly, and rolling back
		// from v3 onto it.
		lineage := []struct {
			id, parent string
			seq        int
		}{{"v1", "", 1}, {"v2", "v1", 2}, {"v3", "v2", 3}}
		for _, v := range lineage {
			if _, err := tx.Exec(
				`INSERT INTO versions (id, project_id, seq, parent_id, produced_by_event, created_at) VALUES (?,?,?,?,'e1',?)`,
				v.id, projectID, v.seq, v.parent, now); err != nil {
				return err
			}
		}
		files := []struct {
			version, path string
			size          int
		}{
			{"v1", "index.html", 120},
			{"v2", "css/style.css", 90}, // styling only — no home page
			{"v3", "index.html", 200},
		}
		for _, f := range files {
			if _, err := tx.Exec(
				`INSERT INTO version_files (project_id, version_id, path, blob_sha, size) VALUES (?,?,?,'sha-'||?,?)`,
				projectID, f.version, f.path, f.version, f.size); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	ps, err := project.New(db, filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	prof := profile.Websites()
	pub := publish.New(db)
	pub.Validate = func(ctx context.Context, projectID, versionID string) error {
		files, err := ps.VersionFiles(ctx, projectID, versionID)
		if err != nil {
			return err
		}
		sizes := make(map[string]int64, len(files))
		for _, f := range files {
			sizes[f.Path] = f.Size
		}
		return prof.ValidateArtifact(sizes)
	}

	api := New(Deps{
		DB: db, Log: eventlog.New(db), Projects: ps, Publish: pub,
		InsecureTenant: "t_default", // no cookie, no token: resolves to the one tenant
	})
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return srv, projectID
}

func postJSON(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Error
}

// Jargon that must never reach a person, mirroring the bar e2e/language_test.go
// holds every other user-facing string to.
var apiJargon = regexp.MustCompile(`\b(artifact|servable|commit|version_files|sql|nil|err|invalid|unknown)\b`)

func assertRefusal(t *testing.T, where string, status int, msg string) {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("%s: HTTP %d, want 409", where, status)
	}
	if msg == "" {
		t.Fatalf("%s: refused with no message at all", where)
	}
	if m := apiJargon.FindString(strings.ToLower(msg)); m != "" {
		t.Fatalf("%s leaks implementation (%q): %q", where, m, msg)
	}
	// It has to say what is wrong in the user's terms, or the sentence is
	// polite noise they cannot act on.
	if !strings.Contains(strings.ToLower(msg), "home page") {
		t.Fatalf("%s does not name what is missing: %q", where, msg)
	}
	if !strings.Contains(msg, ".") {
		t.Fatalf("%s is not a sentence: %q", where, msg)
	}
}

// The gate's refusal has to survive as a *sentence*, not just as a non-200.
// Without this the handler could quietly degrade to serverError's generic
// "something went wrong on our side" and nothing would notice.
func TestPublishRefusalIsAPlainSentence(t *testing.T) {
	srv, projectID := gateFixture(t)

	status, msg := postJSON(t, srv, "/v1/projects/"+projectID+"/publish", `{"versionId":"v2"}`)
	assertRefusal(t, "publish", status, msg)

	// Not a blanket veto: a servable version still goes live.
	if status, _ := postJSON(t, srv, "/v1/projects/"+projectID+"/publish", `{"versionId":"v3"}`); status != http.StatusOK {
		t.Fatalf("servable version refused: HTTP %d", status)
	}
}

// Rollback resolves its target inside the transaction, so its refusal travels a
// separate path with separate copy — and gets a separate chance to regress.
func TestRollbackRefusalIsAPlainSentence(t *testing.T) {
	srv, projectID := gateFixture(t)

	// Live at v3, whose parent v2 is the unservable one.
	if status, _ := postJSON(t, srv, "/v1/projects/"+projectID+"/publish", `{"versionId":"v3"}`); status != http.StatusOK {
		t.Fatalf("setup publish failed: HTTP %d", status)
	}
	status, msg := postJSON(t, srv, "/v1/projects/"+projectID+"/rollback", `{}`)
	assertRefusal(t, "rollback", status, msg)
}
