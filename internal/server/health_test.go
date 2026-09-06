package server

import (
	"context"
	"errors"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
)

// countingStore is a key store that records how readiness asked whether it was
// reachable. Only the two methods the probe could plausibly call do anything.
type countingStore struct {
	lists   int
	pings   int
	pingErr error
	listErr error
}

func (s *countingStore) Get(context.Context, string) (*core.Key, error) { return nil, nil }
func (s *countingStore) Put(context.Context, *core.Key) error           { return nil }
func (s *countingStore) Delete(context.Context, string) error           { return nil }

func (s *countingStore) List(context.Context) ([]*core.Key, error) {
	s.lists++
	return nil, s.listErr
}

// pingingStore adds the Ping that storePinger asserts for. It is a separate
// type so the fallback case is exercised by a store that genuinely cannot
// satisfy the assertion, rather than by a flag the production code never sees.
type pingingStore struct{ *countingStore }

func (s pingingStore) Ping(context.Context) error {
	s.pings++
	return s.pingErr
}

func TestProbeKeyStorePrefersPing(t *testing.T) {
	counts := &countingStore{}
	srv := &Server{store: pingingStore{counts}}

	if err := srv.probeKeyStore(context.Background()); err != nil {
		t.Fatalf("probeKeyStore: %v", err)
	}
	if counts.pings != 1 {
		t.Errorf("Ping called %d times, want 1", counts.pings)
	}
	// The point of the change: an orchestrator probing every few seconds must
	// not scan the key table to learn that the database is up.
	if counts.lists != 0 {
		t.Errorf("List called %d times, want 0", counts.lists)
	}
}

func TestProbeKeyStoreReportsAFailedPing(t *testing.T) {
	want := errors.New("connection refused")
	counts := &countingStore{pingErr: want}
	srv := &Server{store: pingingStore{counts}}

	if err := srv.probeKeyStore(context.Background()); !errors.Is(err, want) {
		t.Fatalf("probeKeyStore returned %v, want %v", err, want)
	}
}

func TestProbeKeyStoreFallsBackToList(t *testing.T) {
	counts := &countingStore{}
	srv := &Server{store: counts}

	if err := srv.probeKeyStore(context.Background()); err != nil {
		t.Fatalf("probeKeyStore: %v", err)
	}
	if counts.lists != 1 {
		t.Errorf("List called %d times, want 1", counts.lists)
	}
}
