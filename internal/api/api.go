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
	"github.com/korya/creo/internal/identity"
	"github.com/korya/creo/internal/project"
	"github.com/korya/creo/internal/publish"
	"github.com/korya/creo/internal/run"
	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/tenant"
)

type ctxKey int

const principalKey ctxKey = 0

func principal(r *http.Request) identity.Principal {
	return r.Context().Value(principalKey).(identity.Principal)
}
func tenantID(r *http.Request) string { return principal(r).TenantID }
func actor(r *http.Request) string    { return principal(r).UserID }

// SessionCookie carries the web session token minted by the IdentityService.
// HttpOnly + SameSite=Lax; no Secure flag at T1 (the documented plain-HTTP
// LAN concession — PRD open question #4).
const SessionCookie = "creo_session"

type Deps struct {
	DB        *store.DB
	Log       *eventlog.Log
	Coord     *run.Coordinator
	Projects  *project.Store
	Tenants   *tenant.Store
	Publish   *publish.Store
	Identity  *identity.Service
	PublicURL string       // base URL of the serving port, e.g. http://127.0.0.1:8081
	Web       http.Handler // the embedded SPA app shell, served at /

	// InsecureTenant, when non-empty, maps unauthenticated requests to that
	// tenant. Set only by `serve --insecure` (loopback-only, dev mode).
	InsecureTenant string
	// Unsecured marks an active --allow-unsecured override; surfaced on
	// /healthz so the operator can always tell (components.md §11 layer 3).
	Unsecured bool
}

type API struct {
	Deps
}

func New(d Deps) *API { return &API{Deps: d} }

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
		if a.Unsecured {
			fmt.Fprintln(w, "warning: --allow-unsecured override active (passwordless login on an exposed address)")
		}
	})
	// Login endpoints are unauthenticated by nature; begin leaks only the
	// bound tenant's display names, which is the picker's whole purpose.
	mux.HandleFunc("POST /v1/auth/login/begin", a.loginBegin)
	mux.HandleFunc("POST /v1/auth/login/complete", a.loginComplete)
	mux.HandleFunc("POST /v1/auth/logout", a.logout)
	mux.Handle("GET /v1/auth/me", a.auth(a.me))
	mux.Handle("POST /v1/projects", a.auth(a.createProject))
	mux.Handle("GET /v1/projects", a.auth(a.listProjects))
	mux.Handle("GET /v1/projects/{id}/versions", a.auth(a.listVersions))
	mux.Handle("POST /v1/projects/{id}/publish", a.auth(a.publishProject))
	mux.Handle("POST /v1/projects/{id}/rollback", a.auth(a.rollbackProject))
	mux.Handle("GET /v1/projects/{id}/preview", a.auth(a.previewURL))
	mux.Handle("GET /v1/projects/{id}/export", a.auth(a.exportProject))
	mux.Handle("POST /v1/sessions/{id}/messages", a.auth(a.postMessage))
	mux.Handle("GET /v1/sessions/{id}/events", a.auth(a.streamEvents))
	mux.Handle("GET /v1/sessions/{id}", a.auth(a.getSession))
	mux.Handle("GET /v1/runs/{id}", a.auth(a.getRun))
	mux.Handle("POST /v1/runs/{id}/input", a.auth(a.answerRun))
	mux.Handle("POST /v1/runs/{id}/cancel", a.auth(a.cancelRun))
	// The web client app shell at / (and its static assets). Unauthenticated —
	// it carries no tenant data; the client authenticates its own /v1 calls.
	if a.Web != nil {
		mux.Handle("/", a.Web)
	}
	return mux
}

// Session states clients render (R-SES-5). The platform names the state; the
// client never infers one from event patterns.
const (
	StateIdle    = "idle"
	StateQueued  = "queued"
	StateWorking = "working"
	StateWaiting = "waiting-for-input"
	StateFailed  = "failed"
)

// SessionStateFor maps a run status to the state clients render. A finished
// run leaves the session idle: the conversation is ready for the next thing.
func SessionStateFor(runStatus string) string {
	switch runStatus {
	case run.StatusQueued, run.StatusRecovering:
		return StateQueued
	case run.StatusRunning:
		return StateWorking
	case run.StatusWaiting:
		return StateWaiting
	case run.StatusFailed:
		return StateFailed
	default: // completed, cancelled, or no run at all
		return StateIdle
	}
}

// --- human login (components.md §11: drivers claim, the service mints) ---

func (a *API) loginBegin(w http.ResponseWriter, r *http.Request) {
	flow, err := a.Identity.BeginLogin(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

func (a *API) loginComplete(w http.ResponseWriter, r *http.Request) {
	var req identity.CompleteLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FlowID == "" {
		httpError(w, http.StatusBadRequest, "body must be {\"flowId\": \"...\", \"params\": {...}}")
		return
	}
	sess, err := a.Identity.CompleteLogin(r.Context(), req)
	switch {
	case errors.Is(err, identity.ErrUnknownFlow):
		httpError(w, http.StatusBadRequest, "that sign-in attempt expired — please pick your account again")
		return
	case errors.Is(err, identity.ErrDisabled), errors.Is(err, identity.ErrUnauthorized):
		httpError(w, http.StatusUnauthorized, "that account cannot sign in")
		return
	case err != nil:
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: sess.Token, Path: "/",
		Expires: sess.ExpiresAt, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	p, err := a.Identity.Authenticate(r.Context(), sess.Token)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		a.Identity.RevokeSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, principal(r))
}

// sessionOfProject returns a project's (single, M0/M1) session, for attaching
// publish lifecycle events to the log.
func (a *API) sessionOfProject(ctx context.Context, projectID string) (string, error) {
	var sessionID string
	err := a.DB.R.QueryRowContext(ctx, `SELECT id FROM sessions WHERE project_id = ? ORDER BY created_at LIMIT 1`, projectID).Scan(&sessionID)
	return sessionID, err
}

func (a *API) resolveVersion(ctx context.Context, projectID, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	return a.Projects.Latest(ctx, projectID)
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
	live, err := a.Publish.Publish(r.Context(), projectID, versionID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	url := a.PublicURL + "/sites/" + live.Slug + "/"
	a.appendProjectEvent(r.Context(), projectID, "publish.completed",
		"Your site is live.", actor(r), map[string]string{"versionId": versionID, "url": url})
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "versionId": versionID})
}

func (a *API) rollbackProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if err := a.ownsProject(r.Context(), tenantID(r), projectID); err != nil {
		httpError(w, http.StatusNotFound, "unknown project")
		return
	}
	live, err := a.Publish.Rollback(r.Context(), projectID)
	if errors.Is(err, publish.ErrNoParent) || errors.Is(err, publish.ErrNotPublished) {
		httpError(w, http.StatusConflict, "nothing to roll back to")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	url := a.PublicURL + "/sites/" + live.Slug + "/"
	a.appendProjectEvent(r.Context(), projectID, "publish.rolled_back",
		"Your site was rolled back to the previous version.", actor(r), map[string]string{"versionId": live.VersionID, "url": url})
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
	secret, err := a.Publish.EnsurePreviewSecret(r.Context(), projectID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"url":       a.PublicURL + "/preview/" + projectID + "/" + secret + "/" + versionID + "/",
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
	files, err := a.Projects.VersionFiles(r.Context(), projectID, versionID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", projectID+".zip"))
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, f := range files {
		blob, err := a.Projects.Open(f.BlobSHA)
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

func (a *API) appendProjectEvent(ctx context.Context, projectID, typ, userText, eventActor string, detail any) {
	sessionID, err := a.sessionOfProject(ctx, projectID)
	if err != nil {
		return
	}
	evs, err := a.Log.Append(ctx, sessionID, []eventlog.NewEvent{{Type: typ, UserText: userText, Detail: detail, Actor: eventActor}}, nil)
	if err == nil {
		a.Log.Publish(sessionID, evs)
	}
}

// auth resolves the caller to a Principal — the single chokepoint for every
// credential kind: bearer API token (programmatic), web-session cookie
// (human), or the --insecure dev mapping. Handlers never see how the caller
// authenticated; they consume the Principal.
func (a *API) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.resolvePrincipal(r)
		if errors.Is(err, errNoCredential) {
			httpError(w, http.StatusUnauthorized, "missing credentials")
			return
		} else if errors.Is(err, tenant.ErrUnauthorized) || errors.Is(err, identity.ErrUnauthorized) {
			httpError(w, http.StatusUnauthorized, "invalid or revoked credentials")
			return
		} else if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

var errNoCredential = errors.New("no credential presented")

func (a *API) resolvePrincipal(r *http.Request) (identity.Principal, error) {
	// 1. Bearer API token (or its SSE query-param form — EventSource cannot
	//    set an Authorization header; T1 posture, tightened at T2. Browsers
	//    don't need it: same-origin EventSource sends the session cookie).
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token != "" {
		tid, err := a.Tenants.Authenticate(r.Context(), token)
		if err != nil {
			return identity.Principal{}, err
		}
		return identity.Principal{TenantID: tid, Method: "api-token", Assurance: identity.Proven}, nil
	}
	// 2. Web session cookie (human login).
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		return a.Identity.Authenticate(r.Context(), c.Value)
	}
	// 3. Dev mode.
	if a.InsecureTenant != "" {
		return identity.Principal{TenantID: a.InsecureTenant, Method: "insecure", Assurance: identity.Attributed}, nil
	}
	return identity.Principal{}, errNoCredential
}

// ownsProject / ownsSession return sql.ErrNoRows for both "does not exist"
// and "belongs to someone else" — callers turn either into a 404.
func (a *API) ownsProject(ctx context.Context, tid, projectID string) error {
	var one int
	return a.DB.R.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = ? AND tenant_id = ?`, projectID, tid).Scan(&one)
}

func (a *API) sessionProject(ctx context.Context, tid, sessionID string) (string, error) {
	var projectID string
	err := a.DB.R.QueryRowContext(ctx,
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
	err := a.DB.Write(r.Context(), func(tx *sql.Tx) error {
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
	rows, err := a.DB.R.QueryContext(r.Context(), `
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
	versions, err := a.Projects.ListVersions(r.Context(), projectID)
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
	// A message typed while the agent is waiting on a question IS the answer.
	// Without this the message would queue a second run that could never be
	// claimed (RC-2 reserves the project for the parked conversation), and the
	// user would watch their reply vanish.
	if waitingID, toolID, ok := a.waitingRun(r.Context(), sessionID); ok {
		if err := a.provideInput(r.Context(), sessionID, waitingID, toolID, body.Text, actor(r)); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"runId": waitingID, "deduped": false, "eventAppended": true, "answered": true,
		})
		return
	}

	ref, appended, err := a.submit(r.Context(), sessionID, projectID, idemKey, body.Text, actor(r))
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
func (a *API) submit(ctx context.Context, sessionID, projectID, idemKey, text, eventActor string) (run.RunRef, bool, error) {
	var ref run.RunRef
	var appended []eventlog.Event
	err := a.DB.Write(ctx, func(tx *sql.Tx) error {
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
		appended, err = a.Log.AppendTx(tx, sessionID, []eventlog.NewEvent{
			{Type: harness.EvUserMessage, UserText: text, Actor: eventActor},
			{Type: harness.EvStateChanged, Detail: map[string]string{"state": StateQueued}},
		}, nil)
		if err != nil {
			return err
		}
		ref, err = a.Coord.RequestRunTx(tx, sessionID, projectID, appended[0].ID, idemKey)
		return err
	})
	if err != nil {
		return run.RunRef{}, false, err
	}
	if len(appended) > 0 {
		a.Log.Publish(sessionID, appended)
		a.Coord.Poke()
	}
	return ref, len(appended) > 0, nil
}

// waitingRun returns the session's run that is parked on a question, if any.
func (a *API) waitingRun(ctx context.Context, sessionID string) (runID, toolID string, ok bool) {
	err := a.DB.R.QueryRowContext(ctx,
		`SELECT id FROM runs WHERE session_id = ? AND status = ? ORDER BY created_at DESC LIMIT 1`,
		sessionID, run.StatusWaiting).Scan(&runID)
	if err != nil {
		return "", "", false
	}
	// The tool call the answer resolves: the newest unanswered question.
	evs, err := a.Log.Read(ctx, sessionID, 0, []string{harness.EvInputRequested})
	if err != nil || len(evs) == 0 {
		return "", "", false
	}
	var d harness.InputRequestDetail
	json.Unmarshal(evs[len(evs)-1].Detail, &d)
	return runID, d.ToolID, true
}

// provideInput appends the answer and resumes the run. Shared by the explicit
// answer route and by a plain message typed while a question is pending — to
// the user those are the same act, so they must behave identically.
func (a *API) provideInput(ctx context.Context, sessionID, runID, toolID, text, eventActor string) error {
	evs, err := a.Log.Append(ctx, sessionID, []eventlog.NewEvent{
		{
			Type: harness.EvInputProvided, RunID: runID, UserText: text,
			Detail: harness.InputProvidedDetail{ToolID: toolID}, Actor: eventActor,
		},
		{Type: harness.EvStateChanged, RunID: runID, Detail: map[string]string{"state": StateQueued}},
	}, nil)
	if err != nil {
		return err
	}
	a.Log.Publish(sessionID, evs)
	return a.Coord.Resume(ctx, runID)
}

func (a *API) answerRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	got, err := a.Coord.Get(r.Context(), runID)
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
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		httpError(w, http.StatusBadRequest, "body must be {\"text\": \"...\"}")
		return
	}
	_, toolID, ok := a.waitingRun(r.Context(), got.SessionID)
	if !ok || got.Status != run.StatusWaiting {
		// Another device answered first, or the run moved on. Not an error the
		// user caused — say so plainly (R-AGT-3).
		httpError(w, http.StatusConflict, "That question was already answered.")
		return
	}
	if err := a.provideInput(r.Context(), got.SessionID, runID, toolID, body.Text, actor(r)); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"runId": runID})
}

// cancelRun stops the active run (R-RUN-4). The project keeps its last
// committed version — cancelling undoes nothing already saved.
func (a *API) cancelRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	got, err := a.Coord.Get(r.Context(), runID)
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
	err = a.Coord.Cancel(r.Context(), runID)
	if errors.Is(err, run.ErrTerminal) {
		httpError(w, http.StatusConflict, "That was already finished.")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// One event carries both the stop and the state change the clients render.
	evs, err := a.Log.Append(r.Context(), got.SessionID, []eventlog.NewEvent{
		{Type: harness.EvRunCancelled, RunID: runID, UserText: "Stopped. Your site is as it was before this change.", Actor: actor(r)},
		{Type: harness.EvStateChanged, RunID: runID, Detail: map[string]string{"state": StateIdle}},
	}, nil)
	if err == nil {
		a.Log.Publish(got.SessionID, evs)
	}
	writeJSON(w, http.StatusOK, map[string]string{"runId": runID, "status": run.StatusCancelled})
}

// getSession reports the state a client should render on first paint, before
// any events arrive (R-SES-5).
func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if _, err := a.sessionProject(r.Context(), tenantID(r), sessionID); err != nil {
		httpError(w, http.StatusNotFound, "unknown session")
		return
	}
	out := map[string]any{"id": sessionID, "state": StateIdle}
	var runID, status string
	err := a.DB.R.QueryRowContext(r.Context(),
		`SELECT id, status FROM runs WHERE session_id = ? ORDER BY created_at DESC LIMIT 1`, sessionID).Scan(&runID, &status)
	if err == nil {
		out["state"] = SessionStateFor(status)
		out["runId"] = runID
	}
	// A pending question travels with the state, so a second device can render
	// the choices immediately instead of replaying the log to find them.
	if _, _, ok := a.waitingRun(r.Context(), sessionID); ok {
		if evs, err := a.Log.Read(r.Context(), sessionID, 0, []string{harness.EvInputRequested}); err == nil && len(evs) > 0 {
			var d harness.InputRequestDetail
			json.Unmarshal(evs[len(evs)-1].Detail, &d)
			out["question"] = map[string]any{"text": d.Question, "choices": d.Choices}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	got, err := a.Coord.Get(r.Context(), r.PathValue("id"))
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
		evs, err := a.Log.Read(r.Context(), sessionID, after, nil)
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

	ch, cancel := a.Log.Subscribe(r.Context(), sessionID, after)
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
