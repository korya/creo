// Package api is the API layer: a thin, idempotency-keyed translation onto
// the components, plus the SSE event stream. Everything a client can do goes
// through here — the CLI and any future web client share this exact surface.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/harness"
	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/run"
	"github.com/korya/creo/internal/store"
)

type API struct {
	db       *store.DB
	log      *eventlog.Log
	coord    *run.Coordinator
	projects *project.Store
}

func New(db *store.DB, log *eventlog.Log, coord *run.Coordinator, projects *project.Store) *API {
	return &API{db: db, log: log, coord: coord, projects: projects}
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("POST /v1/projects", a.createProject)
	mux.HandleFunc("GET /v1/projects", a.listProjects)
	mux.HandleFunc("GET /v1/projects/{id}/versions", a.listVersions)
	mux.HandleFunc("POST /v1/sessions/{id}/messages", a.postMessage)
	mux.HandleFunc("GET /v1/sessions/{id}/events", a.streamEvents)
	mux.HandleFunc("GET /v1/runs/{id}", a.getRun)
	return mux
}

type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SessionID string `json:"sessionId"`
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		httpError(w, http.StatusBadRequest, "body must be {\"name\": \"...\"}")
		return
	}
	p := Project{ID: "p_" + ulid.Make().String(), Name: body.Name, SessionID: "s_" + ulid.Make().String()}
	err := a.db.Write(r.Context(), func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO projects (id, name, created_at) VALUES (?,?,?)`, p.ID, p.Name, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sessions (id, project_id, created_at) VALUES (?,?,?)`, p.SessionID, p.ID, now)
		return err
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (a *API) listProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.R.QueryContext(r.Context(), `
		SELECT p.id, p.name, s.id FROM projects p JOIN sessions s ON s.project_id = p.id ORDER BY p.created_at`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.SessionID); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := a.projects.ListVersions(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// postMessage appends the user message and requests a run. The Idempotency-Key
// header is mandatory: replays return the original run, never a second one.
func (a *API) postMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		httpError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
		httpError(w, http.StatusBadRequest, "body must be {\"text\": \"...\"}")
		return
	}
	var projectID string
	err := a.db.R.QueryRowContext(r.Context(), `SELECT project_id FROM sessions WHERE id = ?`, sessionID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		httpError(w, http.StatusNotFound, "unknown session")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Dedup check happens in RequestRun; the user.message event is only
	// appended for fresh keys, so a replay does not duplicate the message.
	// We do this by asking the coordinator first with a reserved probe.
	ref, appended, err := a.submit(r.Context(), sessionID, projectID, idemKey, body.Text)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"runId":         ref.RunID,
		"deduped":       ref.Deduped,
		"eventAppended": appended,
	})
}

// submit is atomic: idempotency check, user.message append, and run
// registration commit in one transaction (S6 — closes the M0 race). Publish
// and worker wake-up happen after commit.
func (a *API) submit(ctx context.Context, sessionID, projectID, idemKey, text string) (run.RunRef, bool, error) {
	var ref run.RunRef
	var appended []eventlog.Event
	err := a.db.Write(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRow(
			`SELECT run_id FROM idempotency_keys WHERE session_id = ? AND key = ?`, sessionID, idemKey).Scan(&existing)
		if err == nil {
			ref = run.RunRef{RunID: existing, Deduped: true}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		appended, err = a.log.AppendTx(tx, sessionID, []eventlog.NewEvent{{Type: harness.EvUserMessage, UserText: text}}, nil)
		if err != nil {
			return err
		}
		ref, err = a.coord.RequestRunTx(tx, sessionID, projectID, appended[0].ID, idemKey)
		return err
	})
	if err != nil {
		return run.RunRef{}, false, err
	}
	if len(appended) > 0 {
		a.log.Publish(sessionID, appended)
		a.coord.Poke()
	}
	return ref, len(appended) > 0, nil
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	got, err := a.coord.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, run.ErrNotFound) {
		httpError(w, http.StatusNotFound, "unknown run")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id": got.ID, "status": got.Status, "outcome": got.Outcome, "sessionId": got.SessionID,
	})
}

// streamEvents serves the session log as SSE: backfill from ?after=N, then
// live tail. ?stream=false returns a plain JSON array instead.
func (a *API) streamEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	if r.URL.Query().Get("stream") == "false" {
		evs, err := a.log.Read(r.Context(), sessionID, after, nil)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if evs == nil {
			evs = []eventlog.Event{}
		}
		writeJSON(w, http.StatusOK, evs)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := a.log.Subscribe(r.Context(), sessionID, after)
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case e, open := <-ch:
			if !open {
				return
			}
			payload, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, payload)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
