// Package auth issues and validates the gateway's own virtual keys, and decides
// which header on an inbound request carries one.
//
// The gateway sits between a client and a provider, so two distinct credentials
// are in play on every request: the caller's virtual key, which authenticates
// them to the gateway, and the caller's own provider credential, which may need
// to reach the upstream untouched. Telling the two apart is this package's most
// important job; see ExtractGatewayKey.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/erickardus/ai-gateway/internal/core"
)

// keyEntropyBytes is the number of random bytes behind a generated key.
const keyEntropyBytes = 32

// Generate returns a new virtual key and its storage hash. The plaintext is
// returned exactly once, to be handed to the caller; only the hash is ever
// persisted.
func Generate() (plaintext, hash string, err error) {
	buf := make([]byte, keyEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate virtual key: %w", err)
	}
	plaintext = core.PrefixVirtualKey + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashKey(plaintext), nil
}

// HashKey returns the SHA-256 hex digest under which a key is stored. Keys are
// high-entropy random values rather than user-chosen secrets, so a plain digest
// is appropriate here: there is nothing to brute-force.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// SecretsEqual compares two secrets in constant time.
func SecretsEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Redact renders a credential safe to log. It keeps only enough of the value to
// correlate log lines, never enough to use. The empty string stays empty so
// callers can distinguish "absent" from "present but hidden".
func Redact(secret string) string {
	if secret == "" {
		return ""
	}
	bare := core.StripScheme(secret)
	if len(bare) <= 8 {
		return "***"
	}
	return bare[:6] + "***" + fmt.Sprintf("(%d)", len(bare))
}
