package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
)

// header returns an inbound header carrying a virtual key.
func header(key string) http.Header {
	h := http.Header{}
	h.Set("x-gateway-key", key)
	return h
}

// newTestAuthenticator builds an Authenticator holding one key with the given
// limits, and returns it alongside that key's plaintext.
func newTestAuthenticator(t *testing.T, rpm, tpm int) (*Authenticator, string) {
	t.Helper()
	const plaintext = "sk-vk-test"
	a, err := NewAuthenticator(context.Background(), NewMemStore(), config.VirtualKeysConfig{
		HeaderNames: []string{"x-gateway-key"},
		Keys: []config.KeySpec{{
			Key: plaintext, Alias: "test", RPMLimit: rpm, TPMLimit: tpm,
		}},
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return a, plaintext
}

func authContext(t *testing.T, a *Authenticator, plaintext string) *Context {
	t.Helper()
	ac, err := a.Authenticate(context.Background(), header(plaintext))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return ac
}

// admitN reports how many of n consecutive requests were admitted.
func admitN(t *testing.T, a *Authenticator, ac *Context, n int) int {
	t.Helper()
	admitted := 0
	for range n {
		err := a.Admit(context.Background(), ac)
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, core.ErrRateLimited):
		default:
			t.Fatalf("Admit: %v", err)
		}
	}
	return admitted
}

func TestAdmitEnforcesTheKeysRequestLimit(t *testing.T) {
	a, plaintext := newTestAuthenticator(t, 5, 0)
	ac := authContext(t, a, plaintext)

	if got := admitN(t, a, ac, 20); got != 5 {
		t.Errorf("admitted %d of 20 against rpm_limit 5, want 5", got)
	}
}

// The regression: two instances holding the same key list must enforce one
// allowance between them once they share a limiter. Without one they each
// enforce their own, which is what silently multiplied every key's limit by the
// replica count.
func TestAdmitSharesOneAllowanceAcrossInstances(t *testing.T) {
	shared := NewLocalLimiter()

	a, plaintext := newTestAuthenticator(t, 5, 0)
	b, _ := newTestAuthenticator(t, 5, 0)
	a.UseKeyLimiter(shared)
	b.UseKeyLimiter(shared)

	acA, acB := authContext(t, a, plaintext), authContext(t, b, plaintext)

	admitted := admitN(t, a, acA, 10) + admitN(t, b, acB, 10)
	if admitted != 5 {
		t.Errorf("two instances sharing a limiter admitted %d of 20, want 5", admitted)
	}
}

// The same pair without a shared limiter admits the limit twice over. This is
// the documented single-instance behaviour, asserted so that the difference the
// shared limiter makes is visible rather than assumed.
func TestUnsharedLimitersEnforceOneAllowanceEach(t *testing.T) {
	a, plaintext := newTestAuthenticator(t, 5, 0)
	b, _ := newTestAuthenticator(t, 5, 0)
	acA, acB := authContext(t, a, plaintext), authContext(t, b, plaintext)

	if admitted := admitN(t, a, acA, 10) + admitN(t, b, acB, 10); admitted != 10 {
		t.Errorf("two independent instances admitted %d, want 10 (5 each)", admitted)
	}
}

// Tokens reported after a response count against the key's own tpm allowance,
// on whichever instance is asked next.
func TestRecordKeyTokensChargesTheSharedAllowance(t *testing.T) {
	shared := NewLocalLimiter()
	a, plaintext := newTestAuthenticator(t, 0, 1000)
	b, _ := newTestAuthenticator(t, 0, 1000)
	a.UseKeyLimiter(shared)
	b.UseKeyLimiter(shared)

	acA, acB := authContext(t, a, plaintext), authContext(t, b, plaintext)
	if err := a.Admit(context.Background(), acA); err != nil {
		t.Fatalf("first Admit: %v", err)
	}
	a.RecordKeyTokens(context.Background(), acA.Key.Hash, 1000)

	if err := b.Admit(context.Background(), acB); !errors.Is(err, core.ErrRateLimited) {
		t.Errorf("Admit after the whole token allowance was spent elsewhere = %v, want ErrRateLimited", err)
	}
}

func TestForgetKeyReleasesTheCounters(t *testing.T) {
	a, plaintext := newTestAuthenticator(t, 5, 0)
	ac := authContext(t, a, plaintext)

	if got := admitN(t, a, ac, 5); got != 5 {
		t.Fatalf("admitted %d of 5", got)
	}
	if err := a.Admit(context.Background(), ac); !errors.Is(err, core.ErrRateLimited) {
		t.Fatalf("Admit past the limit = %v, want ErrRateLimited", err)
	}

	a.ForgetKey(context.Background(), ac.Key.Hash)

	if err := a.Admit(context.Background(), ac); err != nil {
		t.Errorf("Admit after ForgetKey = %v, want nil", err)
	}
}

// A nil limiter must leave the existing one in place, so a caller with no
// shared store need not branch.
func TestUseKeyLimiterIgnoresNil(t *testing.T) {
	a, plaintext := newTestAuthenticator(t, 5, 0)
	a.UseKeyLimiter(nil)
	ac := authContext(t, a, plaintext)

	if got := admitN(t, a, ac, 10); got != 5 {
		t.Errorf("admitted %d of 10 after UseKeyLimiter(nil), want 5", got)
	}
}

// An unlimited key is never refused.
func TestAdmitAllowsAnUnlimitedKey(t *testing.T) {
	a, plaintext := newTestAuthenticator(t, 0, 0)
	ac := authContext(t, a, plaintext)

	if got := admitN(t, a, ac, 100); got != 100 {
		t.Errorf("admitted %d of 100 for an unlimited key, want 100", got)
	}
}

func TestAdmitWithoutAKeyIsRefused(t *testing.T) {
	a, _ := newTestAuthenticator(t, 5, 0)
	if err := a.Admit(context.Background(), nil); !errors.Is(err, core.ErrKeyInvalid) {
		t.Errorf("Admit(nil) = %v, want ErrKeyInvalid", err)
	}
	if err := a.Admit(context.Background(), &Context{}); !errors.Is(err, core.ErrKeyInvalid) {
		t.Errorf("Admit with no key = %v, want ErrKeyInvalid", err)
	}
}
