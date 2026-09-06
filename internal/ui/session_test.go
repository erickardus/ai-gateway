package ui

import (
	"strings"
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	s, err := NewSessions("sk-master-abc", time.Hour)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	token, expires := s.Issue("master")
	if !expires.After(time.Now()) {
		t.Fatalf("expiry %v is not in the future", expires)
	}
	subject, ok := s.Validate(token)
	if !ok || subject != "master" {
		t.Fatalf("Validate(%q) = %q, %v; want master, true", token, subject, ok)
	}
}

// A token signed by one master key must not verify under another. This is what
// makes rotating the master key sign every operator out, which is the only
// revocation a stateless session has.
func TestSessionDoesNotSurviveMasterKeyRotation(t *testing.T) {
	before, err := NewSessions("sk-master-one", time.Hour)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	after, err := NewSessions("sk-master-two", time.Hour)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	token, _ := before.Issue("master")
	if _, ok := after.Validate(token); ok {
		t.Fatal("a session issued under the old master key still validates under the new one")
	}
}

// Two instances configured with the same master key must accept each other's
// sessions, or a fleet behind a load balancer signs the operator out on every
// other request.
func TestSessionsAreVerifiableAcrossInstances(t *testing.T) {
	one, _ := NewSessions("sk-master-abc", time.Hour)
	two, _ := NewSessions("sk-master-abc", time.Hour)
	token, _ := one.Issue("master")
	if _, ok := two.Validate(token); !ok {
		t.Fatal("a second instance with the same master key rejected the session")
	}
}

func TestSessionRejectsTamperedToken(t *testing.T) {
	s, _ := NewSessions("sk-master-abc", time.Hour)
	token, _ := s.Issue("master")
	payload, mac, _ := strings.Cut(token, ".")

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"no separator", payload + mac},
		{"empty", ""},
		{"payload swapped", encode("admin|99999999999") + "." + mac},
		{"mac truncated", payload + "." + mac[:len(mac)-4]},
		{"payload not base64", "!!!." + mac},
		{"mac not base64", payload + ".!!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := s.Validate(tc.token); ok {
				t.Fatalf("Validate(%q) accepted a tampered token", tc.token)
			}
		})
	}
}

// The expiry is signed into the token, so a client that holds the cookie past
// the TTL cannot use it — the browser's own Max-Age is a courtesy, not the
// enforcement.
func TestSessionExpires(t *testing.T) {
	s, _ := NewSessions("sk-master-abc", time.Minute)
	token, _ := s.Issue("master")

	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, ok := s.Validate(token); ok {
		t.Fatal("an expired session still validates")
	}
}

// A malformed payload that nonetheless carries a valid signature can only come
// from this code, but the parse is still checked: a Validate that returned true
// on an unparseable expiry would treat it as never expiring.
func TestSessionRejectsUnparseablePayload(t *testing.T) {
	s, _ := NewSessions("sk-master-abc", time.Hour)
	payload := "master|not-a-number"
	token := encode(payload) + "." + encode(string(s.sign(payload)))
	if _, ok := s.Validate(token); ok {
		t.Fatal("a token with an unparseable expiry validated")
	}
}

// An empty master key must still produce a usable, unpredictable secret rather
// than a zero one.
func TestSessionsWithoutMasterKeyAreStillSigned(t *testing.T) {
	one, err := NewSessions("", time.Hour)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	two, _ := NewSessions("", time.Hour)
	token, _ := one.Issue("master")
	if _, ok := one.Validate(token); !ok {
		t.Fatal("a session was not valid in the process that issued it")
	}
	if _, ok := two.Validate(token); ok {
		t.Fatal("two processes with no master key derived the same signing secret")
	}
}
