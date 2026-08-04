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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/server"
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
	fmt.Fprint(os.Stderr, `creo — self-hosted app building platform (M0 spine)

  creo serve   [--addr 127.0.0.1:8080] [--data ./data] [--model anthropic:claude-sonnet-5|fake:site]
  creo project new NAME | ls        [--server URL]
  creo say SESSION_ID "message"     [--server URL] [--key IDEMPOTENCY_KEY]
  creo watch SESSION_ID             [--server URL] [--after N] [--all]
`)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	data := fs.String("data", "./data", "data directory")
	modelSpec := fs.String("model", "anthropic:claude-sonnet-5", "model spec: anthropic:<id> or fake:<script>")
	workers := fs.Int("workers", 2, "concurrent runs")
	leaseTTL := fs.Duration("lease-ttl", 15*time.Second, "run lease TTL")
	fs.Parse(args)

	s, err := server.New(server.Config{
		DataDir: *data, Addr: *addr, Model: *modelSpec, Workers: *workers, LeaseTTL: *leaseTTL,
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

func cmdProject(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: creo project new NAME | ls")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("project", flag.ExitOnError)
	srv := serverFlag(fs)
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
		if err := call(http.MethodPost, *srv+"/v1/projects", map[string]string{"name": name}, nil, &out); err != nil {
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
		if err := call(http.MethodGet, *srv+"/v1/projects", nil, nil, &out); err != nil {
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
	headers := map[string]string{"Idempotency-Key": *key}
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

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	srv := serverFlag(fs)
	after := fs.Int64("after", 0, "start after sequence number")
	all := fs.Bool("all", false, "show every event type, not just user-facing ones")
	if len(args) < 1 {
		return fmt.Errorf("usage: creo watch SESSION_ID")
	}
	sessionID := args[0]
	fs.Parse(args[1:])

	resp, err := http.Get(*srv + "/v1/sessions/" + sessionID + "/events?after=" + fmt.Sprint(*after))
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
		json.NewDecoder(resp.Body).Decode(&e)
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
