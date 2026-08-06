package identity

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/korya/creo/internal/store"
	"github.com/korya/creo/internal/tenant"
)

func testSetup(t *testing.T) (*store.DB, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tn, err := tenant.New(db).Create(context.Background(), "family", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	return db, tn.ID
}

func login(t *testing.T, svc *Service, account string) (Session, error) {
	t.Helper()
	flow, err := svc.BeginLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return svc.CompleteLogin(context.Background(), CompleteLoginRequest{
		FlowID: flow.FlowID,
		Params: map[string]string{"account": account},
	})
}

func TestStaticLoginMintAuthenticate(t *testing.T) {
	db, tid := testSetup(t)
	ctx := context.Background()
	anna, err := CreateUser(ctx, db, tid, "Anna", "#e07a5f")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, NewStatic(db, tid))

	flow, err := svc.BeginLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if flow.Kind != FlowChoice || len(flow.Choices) != 1 || flow.Choices[0].ID != anna.ID {
		t.Fatalf("unexpected flow: %+v", flow)
	}

	sess, err := login(t, svc, anna.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Authenticate(ctx, sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != anna.ID || p.TenantID != tid || p.Method != "static" || p.Assurance != Attributed {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

func TestFlowSingleUseAndUnknown(t *testing.T) {
	db, tid := testSetup(t)
	ctx := context.Background()
	anna, _ := CreateUser(ctx, db, tid, "Anna", "")
	svc := NewService(db, NewStatic(db, tid))

	flow, _ := svc.BeginLogin(ctx)
	req := CompleteLoginRequest{FlowID: flow.FlowID, Params: map[string]string{"account": anna.ID}}
	if _, err := svc.CompleteLogin(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteLogin(ctx, req); !errors.Is(err, ErrUnknownFlow) {
		t.Fatalf("flow reuse: want ErrUnknownFlow, got %v", err)
	}
	if _, err := svc.CompleteLogin(ctx, CompleteLoginRequest{FlowID: "lf_nope", Params: req.Params}); !errors.Is(err, ErrUnknownFlow) {
		t.Fatalf("unknown flow: want ErrUnknownFlow, got %v", err)
	}
}

func TestRevokeAndDisable(t *testing.T) {
	db, tid := testSetup(t)
	ctx := context.Background()
	anna, _ := CreateUser(ctx, db, tid, "Anna", "")
	svc := NewService(db, NewStatic(db, tid))

	sess, err := login(t, svc, anna.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeSession(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, sess.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked session: want ErrUnauthorized, got %v", err)
	}

	// Disable revokes live sessions and blocks new logins.
	sess2, err := login(t, svc, anna.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := DisableUser(ctx, db, anna.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, sess2.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("session of disabled user: want ErrUnauthorized, got %v", err)
	}
	if _, err := login(t, svc, anna.ID); !errors.Is(err, ErrDisabled) {
		t.Fatalf("login as disabled: want ErrDisabled, got %v", err)
	}
	// The picker no longer offers the account.
	flow, _ := svc.BeginLogin(ctx)
	if len(flow.Choices) != 0 {
		t.Fatalf("disabled account still offered: %+v", flow.Choices)
	}
}

func TestExpiredSessionRejectedAndRollingRenewal(t *testing.T) {
	db, tid := testSetup(t)
	ctx := context.Background()
	anna, _ := CreateUser(ctx, db, tid, "Anna", "")
	svc := NewService(db, NewStatic(db, tid))
	sess, err := login(t, svc, anna.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Age the session to just under the renewal threshold: Authenticate must
	// succeed AND roll the expiry forward.
	nearExpiry := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE web_sessions SET expires_at = ?`, nearExpiry)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	var expiresStr string
	if err := db.R.QueryRow(`SELECT expires_at FROM web_sessions`).Scan(&expiresStr); err != nil {
		t.Fatal(err)
	}
	expires, _ := time.Parse(time.RFC3339Nano, expiresStr)
	if time.Until(expires) < 80*24*time.Hour {
		t.Fatalf("expiry not rolled forward: %s", expiresStr)
	}

	// A session past its expiry is dead even though un-revoked.
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE web_sessions SET expires_at = ?`, past)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, sess.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired session: want ErrUnauthorized, got %v", err)
	}
}

func TestPickerScopedToBoundTenant(t *testing.T) {
	db, tid := testSetup(t)
	ctx := context.Background()
	other, err := tenant.New(db).Create(ctx, "other", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	CreateUser(ctx, db, tid, "Anna", "")
	CreateUser(ctx, db, other.ID, "Mallory", "")
	svc := NewService(db, NewStatic(db, tid))
	flow, _ := svc.BeginLogin(ctx)
	if len(flow.Choices) != 1 || flow.Choices[0].Name != "Anna" {
		t.Fatalf("picker leaked across tenants: %+v", flow.Choices)
	}
}
