// Package api is the API layer: authentication, tenant scoping, idempotent
// commands, and the SSE event stream — a thin translation onto the components.
// Every route except /healthz requires a bearer token; a resource outside the
// caller's tenant is indistinguishable from a missing one (404, never 403).
package api

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/eventlog"
	"github.com/korya/creo/internal/harness"
	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/publish"
	"github.com/korya/creo/internal/run"
	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/tenant"
)

type ctxKey int

const tenantKey ctxKey = 0

func tenantID(r *http.Request) string { return r.Context().Value(tenantKey).(string) }

type API struct {
	db        *store.DB
	log       *eventlog.Log
	coord     *run.Coordinator
	projects  *project.Store
	tenants   *tenant.Store
	publish   *publish.Store
	publicURL string       // base URL of the serving port, e.g. http://127.0.0.1:8081
	web       http.Handler // the embedded SPA app shell, served at /

	// insecureTenant, when non-empty, maps unauthenticated requests to that
	// tenant. Set only by `serve --insecure` (loopback-only, dev mode).
	insecureTenant string
}

func New(db *store.DB, log *eventlog.Log, coord *run.Coordinator, projects *project.Store, tenants *tenant.Store, pub *publish.Store, publicURL, insecureTenant string, web http.Handler) *API {
	return &API{db: db, log: log, coord: coord, projects: projects, tenants: tenants, publish: pub, publicURL: publicURL, insecureTenant: insecureTenant, web: web}
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("POST /v1/projects", a.auth(a.createProject))
	mux.Handle("GET /v1/projects", a.auth(a.listProjects))
	mux.Handle("GET /v1/projects/{id}/versions", a.auth(a.listVersions))
	mux.Handle("POST /v1/projects/{id}/publish", a.auth(a.publishProject))
	mux.Handle("POST /v1/projects/{id}/rollback", a.auth(a.rollbackProject))
	mux.Handle("GET /v1/projects/{id}/preview", a.auth(a.previewURL))
	mux.Handle("GET /v1/projects/{id}/export", a.auth(a.exportProject))
	mux.Handle("POST /v1/sessions/{id}/messages", a.auth(a.postMessage))
	mux.Handle("GET /v1/sessions/{id}/events", a.auth(a.streamEvents))
	mux.Handle("GET /v1/runs/{id}", a.auth(a.getRun))
	// The web client app shell at / (and its static assets). Unauthenticated —
	// it carries no tenant data; the client authenticates its own /v1 calls.
	if a.web != nil {
		mux.Handle("/", a.web)
	}
	return mux
}

// sessionOfProject returns a project's (single, M0/M1) session, for attaching
// publish lifecycle events to the log.
func (a *API) sessionOfProject(ctx context.Context, projectID string) (string, error) {
	var sessionID string
	err := a.db.R.QueryRowContext(ctx, `SELECT id FROM sessions WHERE project_id = ? ORDER BY created_at LIMIT 1`, projectID).Scan(&sessionID)
	return sessionID, err
}

func (a *API) resolveVersion(ctx context.Context, projectID, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	return a.projects.Latest(ctx, projectID)
}

func (a *API) publishProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if err := a.ownsProject(r.Context(), tenantID(r), projectID); err != nil {
		httpError(w, http.StatusNotFound, "unknown project")
		return
	}
	var body struct {
		VersionID string `json:"versionId"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	versionID, err := a.resolveVersion(r.Context(), projectID, body.VersionID)
	if err != nil || versionID == "" {
		httpError(w, http.StatusBadRequest, "no version to publish")
		return
	}
	live, err := a.publish.Publish(r.Context(), projectID, versionID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	url := a.publicURL + "/sites/" + live.Slug + "/"
	a.appendProjectEvent(r.Context(), projectID, "publish.completed",
		"Your site is live.", map[string]string{"versionId": versionID, "url": url})
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "versionId": versionID})
}

func (a *API) rollbackProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if err := a.ownsProject(r.Context(), tenantID(r), projectID); err != nil {
		httpError(w, http.StatusNotFound, "unknown project")
		return
	}
	live, err := a.publish.Rollback(r.Context(), projectID)
	if errors.Is(err, publish.ErrNoParent) || errors.Is(err, publish.ErrNotPublished) {
		httpError(w, http.StatusConflict, "nothing to roll back to")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	url := a.publicURL + "/sites/" + live.Slug + "/"
	a.appendProjectEvent(r.Context(), projectID, "publish.rolled_back",
		"Your site was rolled back to the previous version.", map[string]string{"versionId": live.VersionID, "url": url})
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "versionId": live.VersionID})
}

func (a *API) previewURL(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if err := a.ownsProject(r.Context(), tenantID(r), projectID); err != nil {
		httpError(w, http.StatusNotFound, "unknown project")
		return
	}
	versionID, err := a.resolveVersion(r.Context(), projectID, r.URL.Query().Get("version"))
	if err != nil || versionID == "" {
		httpError(w, http.StatusBadRequest, "no version to preview")
		return
	}
	secret, err := a.publish.EnsurePreviewSecret(r.Context(), projectID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"url":       a.publicURL + "/preview/" + projectID + "/" + secret + "/" + versionID + "/",
		"versionId": versionID,
	})
}

func (a *API) exportProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if err := a.ownsProject(r.Context(), tenantID(r), projectID); err != nil {
		httpError(w, http.StatusNotFound, "unknown project")
		return
	}
	versionID, err := a.resolveVersion(r.Context(), projectID, r.URL.Query().Get("version"))
	if err != nil || versionID == "" {
		httpError(w, http.StatusBadRequest, "no version to export")
		return
	}
	files, err := a.projects.VersionFiles(r.Context(), projectID, versionID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", projectID+".zip"))
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, f := range files {
		blob, err := a.projects.Open(f.BlobSHA)
		if err != nil {
			return
		}
		fw, err := zw.Create(f.Path)
		if err != nil {
			blob.Close()
			return
		}
		io.Copy(fw, blob)
		blob.Close()
	}
}

func (a *API) appendProjectEvent(ctx context.Context, projectID, typ, userText string, detail any) {
	sessionID, err := a.sessionOfProject(ctx, projectID)
	if err != nil {
		return
	}
	evs, err := a.log.Append(ctx, sessionID, []eventlog.NewEvent{{Type: typ, UserText: userText, Detail: detail}}, nil)
	if err == nil {
		a.log.Publish(sessionID, evs)
	}
}

// auth resolves the bearer token to a tenant and stores it in the context.
func (a *API) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if header == "" && a.insecureTenant != "" {
			next(w, r.WithContext(context.WithValue(r.Context(), tenantKey, a.insecureTenant)))
			return
		}
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			// EventSource cannot set an Authorization header, so the SSE stream
			// accepts the token as a query param (T1 posture — same secrecy as
			// the bearer header over a trusted/loopback link; tightened at T2).
			if q := r.URL.Query().Get("token"); q != "" {
				token, ok = q, true
			}
		}
		if !ok || token == "" {
			httpError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		tid, err := a.tenants.Authenticate(r.Context(), token)
		if errors.Is(err, tenant.ErrUnauthorized) {
			httpError(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		} else if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), tenantKey, tid)))
	})
}

// ownsProject / ownsSession return sql.ErrNoRows for both "does not exist"
// and "belongs to someone else" — callers turn either into a 404.
func (a *API) ownsProject(ctx context.Context, tid, projectID string) error {
	var one int
	return a.db.R.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = ? AND tenant_id = ?`, projectID, tid).Scan(&one)
}

func (a *API) sessionProject(ctx context.Context, tid, sessionID string) (string, error) {
	var projectID string
	err := a.db.R.QueryRowContext(ctx,
		`SELECT project_id FROM sessions WHERE id = ? AND tenant_id = ?`, sessionID, tid).Scan(&projectID)
	return projectID, err
}

type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SessionID string `json:"sessionId"`
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	tid := tenantID(r)
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
		if _, err := tx.Exec(`INSERT INTO projects (id, tenant_id, name, created_at) VALUES (?,?,?,?)`, p.ID, tid, p.Name, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sessions (id, tenant_id, project_id, created_at) VALUES (?,?,?,?)`, p.SessionID, tid, p.ID, now)
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
		SELECT p.id, p.name, s.id FROM projects p JOIN sessions s ON s.project_id = p.id
		WHERE p.tenant_id = ? ORDER BY p.created_at`, tenantID(r))
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
	projectID := r.PathValue("id")
	if err := a.ownsProject(r.Context(), tenantID(r), projectID); err != nil {
		httpError(w, http.StatusNotFound, "unknown project")
		return
	}
	versions, err := a.projects.ListVersions(r.Context(), projectID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// postMessage appends the user message and requests a run — atomically, keyed
// by the mandatory Idempotency-Key header (RC-1 + S6).
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
	projectID, err := a.sessionProject(r.Context(), tenantID(r), sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		httpError(w, http.StatusNotFound, "unknown session")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
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
// registration commit in one transaction (S6). Publish and worker wake-up
// happen after commit.
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
	if err := a.ownsProject(r.Context(), tenantID(r), got.ProjectID); err != nil {
		httpError(w, http.StatusNotFound, "unknown run")
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
	if _, err := a.sessionProject(r.Context(), tenantID(r), sessionID); err != nil {
		httpError(w, http.StatusNotFound, "unknown session")
		return
	}
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
