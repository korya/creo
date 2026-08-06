// Package server wires the components into the v-min single process: API,
// worker pool, lease renewal, and the boot-time + periodic recovery scan.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/api"
	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/harness"
	"github.com/korya/creo/internal/identity"
	"github.com/korya/creo/internal/model"
	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/publish"
	"github.com/korya/creo/internal/run"
	"github.com/korya/creo/internal/serving"
	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/tenant"
	"github.com/korya/creo/internal/webui"
	"github.com/korya/creo/internal/workspace"
)

type Config struct {
	DataDir   string
	Addr      string        // API, default 127.0.0.1:8080
	ServeAddr string        // published/preview sites, default 127.0.0.1:8081
	PublicURL string        // base URL of ServeAddr as seen by clients
	Model     string        // "anthropic:<model-id>" or "fake:<script>"
	Workers   int           // default 2
	LeaseTTL  time.Duration // default 15s
	Insecure  bool          // map unauthenticated requests to t_default; loopback only
	WebDir    string        // serve the web client from disk instead of the embedded bundle (dev)

	// AllowUnsecured overrides the static-login exposure fence (components.md
	// §11): passwordless account-switch on a globally reachable address. The
	// override is logged at startup and surfaced on /healthz — a decision,
	// never a default.
	AllowUnsecured bool
}

type Server struct {
	cfg     Config
	db      *store.DB
	log     *eventlog.Log
	coord   *run.Coordinator
	h       *harness.Harness
	http    *http.Server
	serving *http.Server // origin-isolated site serving on ServeAddr
	wg      sync.WaitGroup
}

func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8080"
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 15 * time.Second
	}
	if cfg.Model == "" {
		cfg.Model = "anthropic:claude-sonnet-5"
	}
	if cfg.ServeAddr == "" {
		cfg.ServeAddr = "127.0.0.1:8081"
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = "http://" + cfg.ServeAddr
	}
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	if cfg.Insecure && !isLoopback(cfg.Addr) {
		return nil, fmt.Errorf("--insecure refuses to bind non-loopback address %q", cfg.Addr)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "creo.db"))
	if err != nil {
		return nil, err
	}
	gw, err := buildGateway(cfg.Model)
	if err != nil {
		db.Close()
		return nil, err
	}
	elog := eventlog.New(db)
	coord := run.New(db, cfg.LeaseTTL)
	ps, err := project.New(db, filepath.Join(cfg.DataDir, "cas"))
	if err != nil {
		db.Close()
		return nil, err
	}
	wp, err := workspace.NewProvider(filepath.Join(cfg.DataDir, "workspaces"))
	if err != nil {
		db.Close()
		return nil, err
	}
	tenants := tenant.New(db)
	// D2 / R-TEN-3: the storage cap, enforced where the store actually grows.
	ps.Quota = func(ctx context.Context, projectID string, blobs []project.Blob) error {
		tid, err := tenants.TenantOfProject(ctx, projectID)
		if err != nil {
			return err
		}
		sizes := make(map[string]int64, len(blobs))
		for _, b := range blobs {
			sizes[b.SHA] = b.Size
		}
		return tenants.CheckStorage(ctx, tid, sizes)
	}
	// The hard budget stop (R-LLM-5): resolved per run, checked before every
	// model call inside Metered — the one point no model traffic can bypass.
	budget := func(ctx context.Context, runID string) error {
		tid, err := tenants.TenantOfRun(ctx, runID)
		if err != nil {
			return err
		}
		return tenants.CheckBudget(ctx, tid)
	}
	insecureTenant := ""
	if cfg.Insecure {
		insecureTenant = "t_default"
		slog.Warn("INSECURE MODE: unauthenticated requests map to t_default — dev only")
	}
	// Human login: the static driver binds to the single tenant that owns
	// accounts (the family). Accounts in a second tenant are a T2-shaped
	// deployment and static login refuses to serve it ambiguously.
	boundTenant, userCount, err := staticTenant(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := checkExposure(cfg.Addr, userCount > 0, cfg.AllowUnsecured, net.InterfaceAddrs); err != nil {
		db.Close()
		return nil, err
	}
	unsecured := false
	if cfg.AllowUnsecured && userCount > 0 && checkExposure(cfg.Addr, true, false, net.InterfaceAddrs) != nil {
		unsecured = true
		slog.Warn("ALLOW-UNSECURED OVERRIDE ACTIVE: passwordless account login is reachable beyond your private network")
	}
	ids := identity.NewService(db, identity.NewStatic(db, boundTenant))
	s := &Server{
		cfg:   cfg,
		db:    db,
		log:   elog,
		coord: coord,
		h: &harness.Harness{
			Log:        elog,
			Projects:   ps,
			Workspaces: wp,
			Gateway:    &model.Metered{Inner: gw, DB: db, Budget: budget},
			Profile:    harness.DefaultProfile(),
		},
	}
	pub := publish.New(db)
	web, err := webui.Handler(cfg.WebDir)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.http = &http.Server{
		Addr: cfg.Addr,
		Handler: api.New(api.Deps{
			DB: db, Log: elog, Coord: coord, Projects: ps, Tenants: tenants, Publish: pub,
			Identity: ids, PublicURL: cfg.PublicURL, Web: web,
			InsecureTenant: insecureTenant, Unsecured: unsecured,
		}).Routes(),
	}
	s.serving = &http.Server{
		Addr:    cfg.ServeAddr,
		Handler: serving.New(ps, pub, harness.DefaultProfile().CSP).Routes(),
	}
	return s, nil
}

// staticTenant finds the one tenant owning human accounts (the static
// driver's bound tenant). No accounts → bind to t_default (empty picker).
// Accounts in more than one tenant → refuse with a plain explanation.
func staticTenant(db *store.DB) (tenantID string, users int, err error) {
	rows, err := db.R.Query(`SELECT tenant_id, COUNT(*) FROM users WHERE disabled_at IS NULL GROUP BY tenant_id`)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var tenants []string
	for rows.Next() {
		var tid string
		var n int
		if err := rows.Scan(&tid, &n); err != nil {
			return "", 0, err
		}
		tenants = append(tenants, tid)
		users += n
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	switch len(tenants) {
	case 0:
		return "t_default", 0, nil
	case 1:
		return tenants[0], users, nil
	default:
		return "", users, fmt.Errorf(
			"accounts exist in %d tenants, but account-switch login serves exactly one; "+
				"other tenants use API tokens (this multi-tenant shape is the T2 profile — see docs)", len(tenants))
	}
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// buildGateway parses a model spec:
//
//	anthropic:<model-id>
//	openai:<model-id>[@<base-url>]   base-url defaults to OpenAI's own API
//	fake:<script>                    tests and demos; no key, no network
func buildGateway(spec string) (model.Gateway, error) {
	kind, arg, _ := strings.Cut(spec, ":")
	switch kind {
	case "anthropic":
		if arg == "" {
			arg = "claude-sonnet-5"
		}
		return model.NewAnthropic(arg), nil
	case "openai":
		modelID, baseURL, hasURL := strings.Cut(arg, "@")
		if modelID == "" {
			return nil, fmt.Errorf("model spec %q needs a model id (openai:<id>[@<base-url>])", spec)
		}
		if !hasURL || baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		return model.NewOpenAICompat(modelID, baseURL), nil
	case "fake":
		return model.FakeScript(arg)
	default:
		return nil, fmt.Errorf("unknown model spec %q (want anthropic:<id>, openai:<id>[@<url>], or fake:<script>)", spec)
	}
}

// Run starts workers, recovery, and the HTTP listener; blocks until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// RC-5: boot-time orphan scan, then periodic.
	if n, err := s.coord.RecoverOrphans(ctx); err != nil {
		return err
	} else if n > 0 {
		slog.Info("recovered orphaned runs", "count", n)
	}
	go func() {
		t := time.NewTicker(s.cfg.LeaseTTL / 2)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.coord.RecoverOrphans(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	for i := 0; i < s.cfg.Workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx, fmt.Sprintf("worker-%d-%s", i, ulid.Make().String()))
	}

	errCh := make(chan error, 2)
	go func() {
		slog.Info("creo API", "addr", s.cfg.Addr, "model", s.cfg.Model, "data", s.cfg.DataDir)
		if err := s.http.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		slog.Info("creo serving sites", "addr", s.cfg.ServeAddr, "public", s.cfg.PublicURL)
		if err := s.serving.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		s.http.Shutdown(shutdownCtx)
		s.serving.Shutdown(shutdownCtx)
		// Give workers a brief, bounded window to relinquish in-flight runs to
		// the recoverable pool before the DB closes. This waits for the
		// relinquish *write*, not the build — on timeout, runs are still
		// recovered via lease expiry (SIGKILL parity).
		drained := make(chan struct{})
		go func() { s.wg.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			slog.Warn("worker drain timed out; in-flight runs recovered via lease expiry")
		}
		return s.db.Close()
	case err := <-errCh:
		return err
	}
}

func (s *Server) worker(ctx context.Context, workerID string) {
	defer s.wg.Done()
	for {
		r, err := s.coord.Claim(ctx, workerID)
		if err != nil {
			slog.Error("claim failed", "worker", workerID, "err", err)
		}
		if r == nil {
			select {
			case <-s.coord.Wake():
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			continue
		}
		s.executeRun(ctx, workerID, r)
	}
}

func (s *Server) executeRun(ctx context.Context, workerID string, r *run.Run) {
	slog.Info("run claimed", "worker", workerID, "run", r.ID, "gen", r.Lease.Gen)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.emitState(runCtx, r, run.StatusRunning)

	// Renew the lease while the harness works; losing it aborts the run.
	go func() {
		t := time.NewTicker(s.cfg.LeaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.coord.Renew(runCtx, r.Lease); err != nil {
					slog.Warn("lease lost", "run", r.ID, "worker", workerID)
					cancel()
					return
				}
			case <-runCtx.Done():
				return
			}
		}
	}()

	text, err := s.h.Execute(runCtx, r)
	if err != nil {
		// Classify by cancellation origin — a shutdown/lease-loss cancel is NOT
		// a genuine failure and must not destroy recoverable work.
		switch {
		case errors.Is(err, harness.ErrAwaitingInput):
			// The agent asked the user something. Park the run: the worker is
			// released and the question waits in the log for any device.
			if e := s.coord.Await(context.WithoutCancel(runCtx), r.Lease); e != nil {
				slog.Warn("await failed", "run", r.ID, "err", e)
			} else {
				s.emitState(context.WithoutCancel(runCtx), r, run.StatusWaiting)
				slog.Info("run waiting for input", "run", r.ID)
			}
		case errors.Is(err, eventlog.ErrStaleLease):
			// Zombie after takeover — already fenced; write nothing.
		case s.wasCancelled(r.ID):
			// The user pressed stop. The cancel event was already recorded by
			// the API; the worker just stands down.
			slog.Info("run cancelled by user", "run", r.ID)
		case ctx.Err() != nil:
			// Graceful shutdown — leave the run recoverable so the next boot
			// resumes it, exactly like the SIGKILL path.
			slog.Info("run left recoverable (shutdown)", "run", r.ID)
			if e := s.coord.Relinquish(context.WithoutCancel(ctx), r.Lease); e != nil {
				slog.Warn("relinquish failed", "run", r.ID, "err", e)
			}
		case runCtx.Err() != nil:
			// Lease lost to a takeover — the new holder owns the narrative.
			slog.Info("run yielded (lease lost)", "run", r.ID)
		default:
			// Genuine failure.
			slog.Warn("run failed", "run", r.ID, "err", err)
			s.h.EmitFailure(context.WithoutCancel(ctx), r, err)
			s.coord.Complete(context.WithoutCancel(ctx), r.Lease, run.StatusFailed, err.Error())
			s.emitState(context.WithoutCancel(ctx), r, run.StatusFailed)
		}
		return
	}
	if err := s.coord.Complete(ctx, r.Lease, run.StatusCompleted, text); err != nil {
		slog.Warn("complete failed", "run", r.ID, "err", err)
	}
	s.emitState(context.WithoutCancel(ctx), r, run.StatusCompleted)
	slog.Info("run completed", "run", r.ID)
}

// emitState publishes the session's state as an explicit event (R-SES-5).
// Clients render state from these; they never infer it from event patterns.
// Emitted after the fenced transition it describes has already succeeded.
func (s *Server) emitState(ctx context.Context, r *run.Run, status string) {
	state := api.SessionStateFor(status)
	evs, err := s.log.Append(ctx, r.SessionID, []eventlog.NewEvent{{
		Type: harness.EvStateChanged, RunID: r.ID, Detail: map[string]string{"state": state},
	}}, nil)
	if err != nil {
		slog.Warn("state event failed", "run", r.ID, "state", state, "err", err)
		return
	}
	s.log.Publish(r.SessionID, evs)
}

// wasCancelled distinguishes a user's stop from a crash takeover: both abort
// the worker by invalidating its lease, but only one of them is a decision.
func (s *Server) wasCancelled(runID string) bool {
	got, err := s.coord.Get(context.Background(), runID)
	return err == nil && got.Status == run.StatusCancelled
}
