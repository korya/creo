// Package identity is the human-login surface (components.md §11). The
// pluggable part is login, not tokens: an Authenticator driver answers exactly
// one question — which human just proved themselves — and the fixed Service
// maps that onto a canonical local user row and mints a Creo-native session.
// External tokens never flow past the login step; the rest of the platform
// consumes only Principal.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/store"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrUnknownFlow  = errors.New("unknown or expired login flow")
	ErrDisabled     = errors.New("account is disabled")
	ErrNotFound     = errors.New("not found")
)

// Assurance is the only identity property policy code may branch on — never
// the driver name. attributed: the account was selected, not proven (static).
// proven: possession of a secret or an IdP-verified login.
type Assurance string

const (
	Attributed Assurance = "attributed"
	Proven     Assurance = "proven"
)

type AuthnMethod struct {
	ID        string    `json:"id"` // "static" | "oidc" (M5) | "api-token"
	Assurance Assurance `json:"assurance"`
}

// VerifiedIdentity is an authentication *event*: a claim about who just logged
// in, never a session. Issuer+Subject key the user_identities linking table.
type VerifiedIdentity struct {
	Issuer      string
	Subject     string
	DisplayName string
	Assurance   Assurance
}

type FlowKind string

const (
	FlowChoice   FlowKind = "choice"   // render Choices locally (static picker)
	FlowRedirect FlowKind = "redirect" // follow RedirectURL (oidc, M5)
)

type AccountChoice struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

type LoginFlow struct {
	FlowID      string          `json:"flowId"`
	Kind        FlowKind        `json:"kind"`
	Choices     []AccountChoice `json:"choices,omitempty"`
	RedirectURL string          `json:"redirectUrl,omitempty"`
	ExpiresAt   time.Time       `json:"expiresAt"`
}

type CompleteLoginRequest struct {
	FlowID string            `json:"flowId"`
	Params map[string]string `json:"params"` // static: {"account": id} · oidc: {"code","state"}
}

// Authenticator is the pluggable seam — the ONLY pluggable part. Drivers
// return identity claims; they can never mint sessions.
type Authenticator interface {
	Describe() AuthnMethod
	BeginLogin(ctx context.Context) (LoginFlow, error)
	CompleteLogin(ctx context.Context, req CompleteLoginRequest) (VerifiedIdentity, error)
}

// Principal is what the entire rest of the platform sees, regardless of how
// the caller authenticated. UserID is empty for tenant-scoped API tokens.
type Principal struct {
	UserID    string    `json:"userId,omitempty"`
	TenantID  string    `json:"tenantId"`
	Name      string    `json:"name,omitempty"`
	Method    string    `json:"method"` // audit/display only — never branch on it
	Assurance Assurance `json:"assurance"`
}

type User struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	Name     string `json:"name"`
	Color    string `json:"color,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

type Session struct {
	ID        string
	Token     string // plaintext, surfaced once (as the cookie value)
	UserID    string
	ExpiresAt time.Time
}

// SessionTTL is the rolling browser-session lifetime (decided 2026-08-05).
const SessionTTL = 90 * 24 * time.Hour

// renewBelow: a session authenticated with less than this remaining is bumped
// back to the full TTL — "rolling" without a write on every request.
const renewBelow = 60 * 24 * time.Hour

// Service is the fixed (non-pluggable) IdentityService: login pipeline for the
// /login handlers, Authenticate for everything else.
type Service struct {
	db    *store.DB
	authn Authenticator
}

func NewService(db *store.DB, authn Authenticator) *Service {
	return &Service{db: db, authn: authn}
}

func (s *Service) Method() AuthnMethod { return s.authn.Describe() }

func (s *Service) BeginLogin(ctx context.Context) (LoginFlow, error) {
	return s.authn.BeginLogin(ctx)
}

// CompleteLogin runs the fixed pipeline: driver claim → canonical user row
// (via user_identities) → Creo-native session. The identity link is the
// lookup key; the user row is the authority (components.md §11).
func (s *Service) CompleteLogin(ctx context.Context, req CompleteLoginRequest) (Session, error) {
	vid, err := s.authn.CompleteLogin(ctx, req)
	if err != nil {
		return Session{}, err
	}
	var userID string
	var disabledAt sql.NullString
	err = s.db.R.QueryRowContext(ctx, `
		SELECT u.id, u.disabled_at FROM user_identities i
		JOIN users u ON u.id = i.user_id
		WHERE i.issuer = ? AND i.subject = ?`, vid.Issuer, vid.Subject).Scan(&userID, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: no user for identity %s/%s", ErrUnauthorized, vid.Issuer, vid.Subject)
	} else if err != nil {
		return Session{}, err
	}
	if disabledAt.Valid {
		return Session{}, ErrDisabled
	}
	return s.mintSession(ctx, userID)
}

func (s *Service) mintSession(ctx context.Context, userID string) (Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, err
	}
	sess := Session{
		ID:        "ws_" + ulid.Make().String(),
		Token:     "sess_" + hex.EncodeToString(raw),
		UserID:    userID,
		ExpiresAt: time.Now().UTC().Add(SessionTTL),
	}
	sum := sha256.Sum256([]byte(sess.Token))
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO web_sessions (id, user_id, token_sha256, created_at, expires_at) VALUES (?,?,?,?,?)`,
			sess.ID, userID, hex.EncodeToString(sum[:]), now(), sess.ExpiresAt.Format(time.RFC3339Nano),
		)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Authenticate resolves a session token (the cookie value) to a Principal,
// rolling the expiry forward when it has decayed below renewBelow.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	sum := sha256.Sum256([]byte(token))
	var sessID, userID, tenantID, name, expiresStr string
	var disabledAt sql.NullString
	err := s.db.R.QueryRowContext(ctx, `
		SELECT ws.id, u.id, u.tenant_id, u.name, ws.expires_at, u.disabled_at
		FROM web_sessions ws JOIN users u ON u.id = ws.user_id
		WHERE ws.token_sha256 = ? AND ws.revoked_at IS NULL`,
		hex.EncodeToString(sum[:])).Scan(&sessID, &userID, &tenantID, &name, &expiresStr, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	} else if err != nil {
		return Principal{}, err
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresStr)
	if err != nil || time.Now().UTC().After(expires) {
		return Principal{}, ErrUnauthorized
	}
	if disabledAt.Valid {
		return Principal{}, ErrUnauthorized
	}
	if time.Until(expires) < renewBelow {
		newExp := time.Now().UTC().Add(SessionTTL).Format(time.RFC3339Nano)
		s.db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE web_sessions SET expires_at = ? WHERE id = ? AND revoked_at IS NULL`, newExp, sessID)
			return err
		})
	}
	m := s.authn.Describe()
	return Principal{UserID: userID, TenantID: tenantID, Name: name, Method: m.ID, Assurance: m.Assurance}, nil
}

func (s *Service) RevokeSession(ctx context.Context, token string) error {
	sum := sha256.Sum256([]byte(token))
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE web_sessions SET revoked_at = ? WHERE token_sha256 = ? AND revoked_at IS NULL`,
			now(), hex.EncodeToString(sum[:]))
		return err
	})
}

// --- account administration (the `creo account` CLI surface) ---

// CreateUser creates a canonical user row plus its static identity link, so
// the account is immediately loginable via the static driver.
func CreateUser(ctx context.Context, db *store.DB, tenantID, name, color string) (User, error) {
	u := User{ID: "u_" + ulid.Make().String(), TenantID: tenantID, Name: name, Color: color}
	err := db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO users (id, tenant_id, name, color, created_at) VALUES (?,?,?,?,?)`,
			u.ID, tenantID, name, color, now()); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO user_identities (issuer, subject, user_id, created_at) VALUES ('static',?,?,?)`,
			u.ID, u.ID, now())
		return err
	})
	return u, err
}

// DisableUser marks the account disabled and revokes its live sessions.
func DisableUser(ctx context.Context, db *store.DB, userID string) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET disabled_at = ? WHERE id = ? AND disabled_at IS NULL`, now(), userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("user %s: %w", userID, ErrNotFound)
		}
		_, err = tx.Exec(`UPDATE web_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now(), userID)
		return err
	})
}

func ListUsers(ctx context.Context, db *store.DB, tenantID string) ([]User, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, tenant_id, name, color, disabled_at IS NOT NULL FROM users WHERE tenant_id = ? ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.TenantID, &u.Name, &u.Color, &u.Disabled); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
