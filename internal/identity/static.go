package identity

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/korya/creo/internal/store"
)

// Static is the permanent T1 driver (components.md §11): a passwordless
// picker over manually precreated, mutually-trusting accounts. Attribution,
// not proof — its Assurance is Attributed, and the platform's banner, fence,
// and trust-tier copy all key off that property, never off the driver name.
//
// Login-flow state is in-memory with a TTL: v-min is a single process, and an
// interrupted login simply restarts — nothing durable is lost.
type Static struct {
	db     *store.DB
	tenant string // the one tenant whose accounts the picker offers

	mu    sync.Mutex
	flows map[string]time.Time // flowID -> expiry
}

const flowTTL = 5 * time.Minute

func NewStatic(db *store.DB, tenantID string) *Static {
	return &Static{db: db, tenant: tenantID, flows: map[string]time.Time{}}
}

func (s *Static) Describe() AuthnMethod {
	return AuthnMethod{ID: "static", Assurance: Attributed}
}

func (s *Static) BeginLogin(ctx context.Context) (LoginFlow, error) {
	users, err := ListUsers(ctx, s.db, s.tenant)
	if err != nil {
		return LoginFlow{}, err
	}
	var choices []AccountChoice
	for _, u := range users {
		if u.Disabled {
			continue
		}
		choices = append(choices, AccountChoice{ID: u.ID, Name: u.Name, Color: u.Color})
	}
	flow := LoginFlow{
		FlowID:    "lf_" + ulid.Make().String(),
		Kind:      FlowChoice,
		Choices:   choices,
		ExpiresAt: time.Now().UTC().Add(flowTTL),
	}
	s.mu.Lock()
	for id, exp := range s.flows { // opportunistic sweep; the map stays tiny
		if time.Now().After(exp) {
			delete(s.flows, id)
		}
	}
	s.flows[flow.FlowID] = flow.ExpiresAt
	s.mu.Unlock()
	return flow, nil
}

func (s *Static) CompleteLogin(ctx context.Context, req CompleteLoginRequest) (VerifiedIdentity, error) {
	s.mu.Lock()
	exp, ok := s.flows[req.FlowID]
	if ok {
		delete(s.flows, req.FlowID) // single-use
	}
	s.mu.Unlock()
	if !ok || time.Now().After(exp) {
		return VerifiedIdentity{}, ErrUnknownFlow
	}
	account := req.Params["account"]
	if account == "" {
		return VerifiedIdentity{}, fmt.Errorf("%w: missing account", ErrUnauthorized)
	}
	users, err := ListUsers(ctx, s.db, s.tenant)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	for _, u := range users {
		if u.ID != account {
			continue
		}
		if u.Disabled {
			return VerifiedIdentity{}, ErrDisabled
		}
		return VerifiedIdentity{Issuer: "static", Subject: u.ID, DisplayName: u.Name, Assurance: Attributed}, nil
	}
	return VerifiedIdentity{}, fmt.Errorf("%w: unknown account", ErrUnauthorized)
}
