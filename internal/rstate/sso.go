package rstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/erickardus/ai-gateway/internal/sso"
)

// Namespaces for the two kinds of pending SSO state.
const (
	ssoLoginNamespace  = "sso.login"
	ssoTicketNamespace = "sso.ticket"
)

// SSOStore keeps pending SSO logins in Redis so a fleet can complete one.
//
// This is the piece a single-instance gateway does not need. A login leaves the
// gateway for the identity provider and comes back as a fresh request, and
// behind a load balancer it comes back wherever the balancer sends it — so with
// process-local state a login succeeds only when both halves happen to land on
// the same replica, which looks to a developer like SSO working intermittently.
//
// Unlike the rate limiters, this does not degrade to local state when Redis is
// unreachable. A limit counted per instance is a weaker version of the right
// answer; a login completed against state another instance holds is not
// possible at all, and pretending otherwise would substitute a confusing
// failure for a clear one. Logins fail while Redis is down, and inference —
// which is what the keys already issued are for — carries on.
type SSOStore struct {
	store *Store
}

// SSOStore returns a shared store for pending SSO logins.
func (s *Store) SSOStore() *SSOStore { return &SSOStore{store: s} }

// PutLogin implements sso.Store.
func (s *SSOStore) PutLogin(ctx context.Context, state string, l sso.Login, ttl time.Duration) error {
	return s.put(ctx, ssoLoginNamespace, state, l, ttl)
}

// TakeLogin implements sso.Store.
func (s *SSOStore) TakeLogin(ctx context.Context, state string) (sso.Login, bool, error) {
	var l sso.Login
	ok, err := s.take(ctx, ssoLoginNamespace, state, &l)
	return l, ok, err
}

// PutTicket implements sso.Store.
func (s *SSOStore) PutTicket(ctx context.Context, code string, t sso.Ticket, ttl time.Duration) error {
	return s.put(ctx, ssoTicketNamespace, code, t, ttl)
}

// TakeTicket implements sso.Store.
func (s *SSOStore) TakeTicket(ctx context.Context, code string) (sso.Ticket, bool, error) {
	var t sso.Ticket
	ok, err := s.take(ctx, ssoTicketNamespace, code, &t)
	return t, ok, err
}

func (s *SSOStore) put(ctx context.Context, namespace, id string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode pending %s: %w", namespace, err)
	}
	rctx, cancel := s.store.ctx(ctx)
	defer cancel()
	if err := s.store.client.Set(rctx, s.store.key(namespace, id), raw, ttl).Err(); err != nil {
		return fmt.Errorf("store pending %s: %w", namespace, err)
	}
	return nil
}

// take reads and removes an entry in one round trip. GETDEL is what makes these
// single-use across a fleet: two instances handling a replayed callback cannot
// both see the state, because only one of them gets the value.
func (s *SSOStore) take(ctx context.Context, namespace, id string, into any) (bool, error) {
	if id == "" {
		return false, nil
	}
	rctx, cancel := s.store.ctx(ctx)
	defer cancel()
	raw, err := s.store.client.GetDel(rctx, s.store.key(namespace, id)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// Expired, already redeemed, or never existed. All three are the
			// same answer to the caller, and deliberately indistinguishable to
			// anyone probing the endpoint.
			return false, nil
		}
		return false, fmt.Errorf("read pending %s: %w", namespace, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return false, fmt.Errorf("decode pending %s: %w", namespace, err)
	}
	return true, nil
}
