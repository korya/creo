// Package server wires the components into the v-min single process: API,
// worker pool, lease renewal, and the boot-time + periodic recovery scan.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/api"
	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/harness"
	"github.com/korya/creo/internal/model"
	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/run"
	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/workspace"
)

type Config struct {
	DataDir  string
	Addr     string        // default :8080
	Model    string        // "anthropic:<model-id>" or "fake:<script>"
	Workers  int           // default 2
	LeaseTTL time.Duration // default 15s
}

type Server struct {
	cfg   Config
	db    *store.DB
	log   *eventlog.Log
	coord *run.Coordinator
	h     *harness.Harness
	http  *http.Server
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
	s := &Server{
		cfg:   cfg,
		db:    db,
		log:   elog,
		coord: coord,
		h: &harness.Harness{
			Log:        elog,
			Projects:   ps,
			Workspaces: wp,
			Gateway:    &model.Metered{Inner: gw, DB: db},
			Profile:    harness.DefaultProfile(),
		},
	}
	s.http = &http.Server{
		Addr:    cfg.Addr,
		Handler: api.New(db, elog, coord, ps).Routes(),
	}
	return s, nil
}

func buildGateway(spec string) (model.Gateway, error) {
	kind, arg, _ := strings.Cut(spec, ":")
	switch kind {
	case "anthropic":
		if arg == "" {
			arg = "claude-sonnet-5"
		}
		return model.NewAnthropic(arg), nil
	case "fake":
		return model.FakeScript(arg)
	default:
		return nil, fmt.Errorf("unknown model spec %q (want anthropic:<id> or fake:<script>)", spec)
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
		go s.worker(ctx, fmt.Sprintf("worker-%d-%s", i, ulid.Make().String()))
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("creo serving", "addr", s.cfg.Addr, "model", s.cfg.Model, "data", s.cfg.DataDir)
		if err := s.http.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		s.http.Shutdown(shutdownCtx)
		return s.db.Close()
	case err := <-errCh:
		return err
	}
}

func (s *Server) worker(ctx context.Context, workerID string) {
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
		slog.Warn("run failed", "run", r.ID, "err", err)
		if !errors.Is(err, eventlog.ErrStaleLease) {
			s.h.EmitFailure(context.WithoutCancel(ctx), r, err)
			s.coord.Complete(context.WithoutCancel(ctx), r.Lease, run.StatusFailed, err.Error())
		}
		return
	}
	if err := s.coord.Complete(ctx, r.Lease, run.StatusCompleted, text); err != nil {
		slog.Warn("complete failed", "run", r.ID, "err", err)
	}
	slog.Info("run completed", "run", r.ID)
}
