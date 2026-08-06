// creo is the M0 CLI: `serve` runs the single-process server; every other
// subcommand is a pure HTTP client of the API — the proof of headlessness.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/identity"
	"github.com/korya/creo/internal/server"
	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/tenant"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "project":
		err = cmdProject(os.Args[2:])
	case "say":
		err = cmdSay(os.Args[2:])
	case "watch":
		err = cmdWatch(os.Args[2:])
	case "answer":
		err = cmdRunAction(os.Args[2:], "input")
	case "cancel":
		err = cmdRunAction(os.Args[2:], "cancel")
	case "publish":
		err = cmdProjectAction(os.Args[2:], "publish")
	case "rollback":
		err = cmdProjectAction(os.Args[2:], "rollback")
	case "preview":
		err = cmdPreview(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "tenant":
		err = cmdTenant(os.Args[2:])
	case "token":
		err = cmdToken(os.Args[2:])
	case "account":
		err = cmdAccount(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `creo — self-hosted app building platform

server:
  creo serve   [--addr 127.0.0.1:8080] [--data ./data] [--model SPEC] [--insecure] [--allow-unsecured]

admin (local, operates on the data directory):
  creo tenant new NAME [--daily-tokens N] [--max-runs N] [--max-storage-mb N] [--data ./data]
  creo tenant ls                        [--data ./data]
  creo token new TENANT_ID [--name X]   [--data ./data]
  creo token revoke TOKEN_ID            [--data ./data]
  creo account new NAME [--tenant ID] [--color HEX] [--data ./data]
  creo account ls [--tenant ID]         [--data ./data]
  creo account disable USER_ID          [--data ./data]

client (HTTP; auth via --token or CREO_TOKEN):
  creo project new NAME | ls        [--server URL] [--token T]
  creo say SESSION_ID "message"     [--server URL] [--token T] [--key IDEMPOTENCY_KEY]
  creo answer RUN_ID "reply"        [--server URL] [--token T]
  creo cancel RUN_ID                [--server URL] [--token T]
  creo watch SESSION_ID             [--server URL] [--token T] [--after N] [--all]
`)
}

// openData opens the admin database, creating the data directory if this is
// the first command run on a fresh install (`creo account new` before the
// server has ever started is a normal first step).
func openData(dir string) (*store.DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dir, "creo.db"))
}

func cmdTenant(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: creo tenant new NAME | ls")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("tenant", flag.ExitOnError)
	data := fs.String("data", "./data", "data directory")
	switch sub {
	case "new":
		if len(rest) < 1 {
			return fmt.Errorf("usage: creo tenant new NAME")
		}
		name := rest[0]
		daily := fs.Int64("daily-tokens", 0, "daily token budget (0 = unlimited)")
		maxRuns := fs.Int64("max-runs", 2, "max concurrent runs")
		maxStorageMB := fs.Int64("max-storage-mb", 0, "storage limit in MB (0 = unlimited)")
		fs.Parse(rest[1:])
		db, err := openData(*data)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		var limit *int64
		if *daily > 0 {
			limit = daily
		}
		var storage *int64
		if *maxStorageMB > 0 {
			bytes := *maxStorageMB << 20
			storage = &bytes
		}
		t, err := tenant.New(db).Create(context.Background(), name, limit, *maxRuns, storage)
		if err != nil {
			return err
		}
		fmt.Printf("tenant %s  (%s)\n", t.ID, t.Name)
		return nil
	case "ls":
		fs.Parse(rest)
		db, err := openData(*data)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		tenants, err := tenant.New(db).List(context.Background())
		if err != nil {
			return err
		}
		for _, t := range tenants {
			limit := "unlimited"
			if t.DailyTokenLimit != nil {
				limit = fmt.Sprintf("%d/day", *t.DailyTokenLimit)
			}
			storage := "unlimited"
			if t.MaxStorageBytes != nil {
				storage = fmt.Sprintf("%dMB", *t.MaxStorageBytes>>20)
			}
			fmt.Printf("%s  %-16s  budget=%-12s  storage=%-10s  max-runs=%d\n", t.ID, t.Name, limit, storage, t.MaxConcurrentRuns)
		}
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func cmdToken(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: creo token new TENANT_ID | revoke TOKEN_ID")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	data := fs.String("data", "./data", "data directory")
	switch sub {
	case "new":
		if len(rest) < 1 {
			return fmt.Errorf("usage: creo token new TENANT_ID")
		}
		tenantID := rest[0]
		name := fs.String("name", "cli", "token name")
		fs.Parse(rest[1:])
		db, err := openData(*data)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		plaintext, id, err := tenant.New(db).CreateToken(context.Background(), tenantID, *name)
		if err != nil {
			return err
		}
		fmt.Printf("token %s\n%s\n", id, plaintext)
		fmt.Fprintln(os.Stderr, "shown once — store it now (e.g. export CREO_TOKEN=...)")
		return nil
	case "revoke":
		if len(rest) < 1 {
			return fmt.Errorf("usage: creo token revoke TOKEN_ID")
		}
		tokenID := rest[0]
		fs.Parse(rest[1:])
		db, err := openData(*data)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		if err := tenant.New(db).RevokeToken(context.Background(), tokenID); err != nil {
			return err
		}
		fmt.Println("revoked", tokenID)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

// cmdAccount administers the static-login accounts (components.md §11): a few
// manually precreated, mutually trusting humans — the permanent T1 answer.
func cmdAccount(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: creo account new NAME | ls | disable USER_ID")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("account", flag.ExitOnError)
	data := fs.String("data", "./data", "data directory")
	tenantID := fs.String("tenant", "t_default", "owning tenant")
	switch sub {
	case "new":
		if len(rest) < 1 {
			return fmt.Errorf("usage: creo account new NAME")
		}
		name := rest[0]
		color := fs.String("color", "", "display colour (hex)")
		fs.Parse(rest[1:])
		db, err := openData(*data)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		u, err := identity.CreateUser(context.Background(), db, *tenantID, name, *color)
		if err != nil {
			return err
		}
		fmt.Printf("account %s  (%s)\n", u.ID, u.Name)
		return nil
	case "ls":
		fs.Parse(rest)
		db, err := openData(*data)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		users, err := identity.ListUsers(context.Background(), db, *tenantID)
		if err != nil {
			return err
		}
		for _, u := range users {
			state := ""
			if u.Disabled {
				state = "  (disabled)"
			}
			fmt.Printf("%s  %-16s%s\n", u.ID, u.Name, state)
		}
		return nil
	case "disable":
		if len(rest) < 1 {
			return fmt.Errorf("usage: creo account disable USER_ID")
		}
		userID := rest[0]
		fs.Parse(rest[1:])
		db, err := openData(*data)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		if err := identity.DisableUser(context.Background(), db, userID); err != nil {
			return err
		}
		fmt.Println("disabled", userID, "(live sessions revoked)")
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	data := fs.String("data", "./data", "data directory")
	serveAddr := fs.String("serve-addr", "127.0.0.1:8081", "listen address for published/preview sites")
	publicURL := fs.String("public-url", "", "public base URL of serve-addr (default: http://serve-addr)")
	modelSpec := fs.String("model", "anthropic:claude-sonnet-5",
		"model spec: anthropic:<id>, openai:<id>[@<base-url>], or fake:<script>")
	workers := fs.Int("workers", 2, "concurrent runs")
	leaseTTL := fs.Duration("lease-ttl", 15*time.Second, "run lease TTL")
	insecure := fs.Bool("insecure", false, "map unauthenticated requests to t_default (loopback only, dev mode)")
	webDir := fs.String("web-dir", "", "serve the web client from a directory instead of the embedded bundle (dev)")
	allowUnsecured := fs.Bool("allow-unsecured", false,
		"DANGEROUS: allow passwordless account login on a globally reachable address")
	fs.Parse(args)

	s, err := server.New(server.Config{
		DataDir: *data, Addr: *addr, ServeAddr: *serveAddr, PublicURL: *publicURL,
		Model: *modelSpec, Workers: *workers, LeaseTTL: *leaseTTL, Insecure: *insecure, WebDir: *webDir,
		AllowUnsecured: *allowUnsecured,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Run(ctx)
}

func serverFlag(fs *flag.FlagSet) *string {
	return fs.String("server", envOr("CREO_SERVER", "http://127.0.0.1:8080"), "server URL")
}

func tokenFlag(fs *flag.FlagSet) *string {
	return fs.String("token", os.Getenv("CREO_TOKEN"), "bearer token (default: CREO_TOKEN env)")
}

func authHeaders(token string, extra map[string]string) map[string]string {
	h := map[string]string{}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func cmdProject(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: creo project new NAME | ls")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("project", flag.ExitOnError)
	srv := serverFlag(fs)
	tok := tokenFlag(fs)
	switch sub {
	case "new":
		if len(rest) < 1 {
			return fmt.Errorf("usage: creo project new NAME")
		}
		name := rest[0]
		fs.Parse(rest[1:])
		var out struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			SessionID string `json:"sessionId"`
		}
		if err := call(http.MethodPost, *srv+"/v1/projects", map[string]string{"name": name}, authHeaders(*tok, nil), &out); err != nil {
			return err
		}
		fmt.Printf("project  %s  (%s)\nsession  %s\n", out.ID, out.Name, out.SessionID)
		return nil
	case "ls":
		fs.Parse(rest)
		var out []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			SessionID string `json:"sessionId"`
		}
		if err := call(http.MethodGet, *srv+"/v1/projects", nil, authHeaders(*tok, nil), &out); err != nil {
			return err
		}
		for _, p := range out {
			fmt.Printf("%s  %-20s  session=%s\n", p.ID, p.Name, p.SessionID)
		}
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func cmdSay(args []string) error {
	fs := flag.NewFlagSet("say", flag.ExitOnError)
	srv := serverFlag(fs)
	tok := tokenFlag(fs)
	key := fs.String("key", "", "idempotency key (default: generated)")
	if len(args) < 2 {
		return fmt.Errorf("usage: creo say SESSION_ID \"message\"")
	}
	sessionID, text := args[0], args[1]
	fs.Parse(args[2:])
	if *key == "" {
		*key = "cli-" + ulid.Make().String()
	}
	var out struct {
		RunID   string `json:"runId"`
		Deduped bool   `json:"deduped"`
	}
	headers := authHeaders(*tok, map[string]string{"Idempotency-Key": *key})
	if err := call(http.MethodPost, *srv+"/v1/sessions/"+sessionID+"/messages", map[string]string{"text": text}, headers, &out); err != nil {
		return err
	}
	status := "accepted"
	if out.Deduped {
		status = "deduped (already submitted)"
	}
	fmt.Printf("run %s  %s\n", out.RunID, status)
	return nil
}

// cmdProjectAction handles publish and rollback (both POST, both return a URL).
func cmdProjectAction(args []string, action string) error {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	srv := serverFlag(fs)
	tok := tokenFlag(fs)
	var version *string
	if action == "publish" {
		version = fs.String("version", "", "version id (default: latest)")
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: creo %s PROJECT_ID", action)
	}
	projectID := args[0]
	fs.Parse(args[1:])
	var body any
	if action == "publish" && *version != "" {
		body = map[string]string{"versionId": *version}
	}
	var out struct {
		URL       string `json:"url"`
		VersionID string `json:"versionId"`
	}
	if err := call(http.MethodPost, *srv+"/v1/projects/"+projectID+"/"+action, body, authHeaders(*tok, nil), &out); err != nil {
		return err
	}
	fmt.Printf("%s\n", out.URL)
	return nil
}

func cmdPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ExitOnError)
	srv := serverFlag(fs)
	tok := tokenFlag(fs)
	version := fs.String("version", "", "version id (default: latest)")
	if len(args) < 1 {
		return fmt.Errorf("usage: creo preview PROJECT_ID")
	}
	projectID := args[0]
	fs.Parse(args[1:])
	path := "/v1/projects/" + projectID + "/preview"
	if *version != "" {
		path += "?version=" + *version
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := call(http.MethodGet, *srv+path, nil, authHeaders(*tok, nil), &out); err != nil {
		return err
	}
	fmt.Println(out.URL)
	return nil
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	srv := serverFlag(fs)
	tok := tokenFlag(fs)
	version := fs.String("version", "", "version id (default: latest)")
	out := fs.String("o", "export.zip", "output file")
	if len(args) < 1 {
		return fmt.Errorf("usage: creo export PROJECT_ID [-o out.zip]")
	}
	projectID := args[0]
	fs.Parse(args[1:])
	path := "/v1/projects/" + projectID + "/export"
	if *version != "" {
		path += "?version=" + *version
	}
	req, err := http.NewRequest(http.MethodGet, *srv+path, nil)
	if err != nil {
		return err
	}
	if *tok != "" {
		req.Header.Set("Authorization", "Bearer "+*tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("export failed: %s", resp.Status)
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *out)
	return nil
}

// cmdRunAction handles answer (POST body) and cancel (no body) on a run.
func cmdRunAction(args []string, action string) error {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	srv := serverFlag(fs)
	tok := tokenFlag(fs)
	need := 1
	if action == "input" {
		need = 2
	}
	if len(args) < need {
		if action == "input" {
			return fmt.Errorf("usage: creo answer RUN_ID \"reply\"")
		}
		return fmt.Errorf("usage: creo cancel RUN_ID")
	}
	runID := args[0]
	var body any
	if action == "input" {
		body = map[string]string{"text": args[1]}
		fs.Parse(args[2:])
	} else {
		fs.Parse(args[1:])
	}
	if err := call(http.MethodPost, *srv+"/v1/runs/"+runID+"/"+action, body, authHeaders(*tok, nil), nil); err != nil {
		return err
	}
	if action == "input" {
		fmt.Println("answered", runID)
	} else {
		fmt.Println("stopped", runID)
	}
	return nil
}

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	srv := serverFlag(fs)
	tok := tokenFlag(fs)
	after := fs.Int64("after", 0, "start after sequence number")
	all := fs.Bool("all", false, "show every event type, not just user-facing ones")
	if len(args) < 1 {
		return fmt.Errorf("usage: creo watch SESSION_ID")
	}
	sessionID := args[0]
	fs.Parse(args[1:])

	req, err := http.NewRequest(http.MethodGet, *srv+"/v1/sessions/"+sessionID+"/events?after="+fmt.Sprint(*after), nil)
	if err != nil {
		return err
	}
	if *tok != "" {
		req.Header.Set("Authorization", "Bearer "+*tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e struct {
			Seq      int64  `json:"seq"`
			Type     string `json:"type"`
			UserText string `json:"userText"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			continue
		}
		if *all {
			fmt.Printf("%4d  %-26s  %s\n", e.Seq, e.Type, e.UserText)
		} else if e.UserText != "" {
			fmt.Printf("[%s] %s\n", strings.TrimSuffix(e.Type, ".message"), e.UserText)
		}
	}
	return sc.Err()
}

func call(method, url string, body any, headers map[string]string, out any) error {
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		// Best-effort: a non-JSON error body still leaves the status worth reporting.
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
