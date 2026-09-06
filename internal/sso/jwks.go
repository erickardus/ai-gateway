package sso

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"time"
)

// jwksMinRefetch bounds how often an unknown key id may trigger a refetch.
//
// A provider rotating keys is normal and the gateway must follow it, but
// without a bound every token carrying an unrecognised kid — which is what a
// flood of forged tokens looks like — would become a request to the provider.
// A few seconds settles both: an unannounced rotation is followed almost at
// once, and the worst an attacker can extract is a handful of fetches a minute.
const jwksMinRefetch = 5 * time.Second

// jwksTTL is how long a key set is reused before a scheduled refresh.
const jwksTTL = time.Hour

// jwkSet is a provider's published key set.
type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// jwk is one published key. Only the parameters of the key types the gateway
// verifies are read.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`

	// RSA
	N string `json:"n"`
	E string `json:"e"`

	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKey returns the verification key for a key id, fetching the key set if
// the id is unknown.
//
// An unknown kid is the ordinary signal that the provider has rotated, so it is
// a refetch rather than a failure — but a rate-limited one, so the same signal
// cannot be used to make the gateway hammer its provider.
func (p *Provider) publicKey(ctx context.Context, kid string) (any, error) {
	p.mu.Lock()
	key, known := p.keys[kid]
	fresh := p.now().Sub(p.keysAt) < jwksTTL
	canRefetch := p.now().Sub(p.keysAt) >= jwksMinRefetch
	p.mu.Unlock()

	if known && fresh {
		return key, nil
	}
	if known && !canRefetch {
		return key, nil
	}
	if !known && !canRefetch && len(p.snapshotKeys()) > 0 {
		return nil, errorf("no key with id %q in the provider's key set", kid)
	}

	keys, err := p.fetchKeys(ctx)
	if err != nil {
		if known {
			// The cached key verified tokens a moment ago and still will. A
			// login failing because the key set was briefly unreachable would
			// be a worse answer than using it.
			p.log.Warn("sso: key set refresh failed, using cached keys", "error", err)
			return key, nil
		}
		return nil, err
	}

	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errorf("no key with id %q in the provider's key set", kid)
}

func (p *Provider) snapshotKeys() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keys
}

func (p *Provider) fetchKeys(ctx context.Context) (map[string]any, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	var set jwkSet
	if err := p.getJSON(ctx, d.JWKSURI, &set); err != nil {
		return nil, errorf("fetch jwks: %w", err)
	}

	keys := make(map[string]any, len(set.Keys))
	for _, k := range set.Keys {
		// A key published for encryption is not a key for verifying a
		// signature, and treating it as one would accept a signature the
		// provider never intended as an assertion of identity.
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.parse()
		if err != nil {
			// One unusable key does not invalidate the set: providers publish
			// key types this gateway does not verify, and refusing the whole
			// document would take out the keys it does.
			p.log.Debug("sso: skipping unusable key in the provider's key set", "kid", k.Kid, "kty", k.Kty, "error", err)
			continue
		}
		if k.Kid == "" {
			// A set with a single unidentified key is legal, and a token from
			// such a provider carries no kid either; both sides use "".
			keys[""] = pub
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errorf("the provider's key set at %s contains no usable signing key", d.JWKSURI)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys, p.keysAt = keys, p.now()
	return keys, nil
}

// parse converts a published key into a verification key.
func (k jwk) parse() (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64uint(k.N)
		if err != nil {
			return nil, errorf("rsa modulus: %w", err)
		}
		e, err := b64uint(k.E)
		if err != nil {
			return nil, errorf("rsa exponent: %w", err)
		}
		if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return nil, errorf("rsa exponent out of range")
		}
		// A short modulus is a key an attacker could factor. 2048 bits is the
		// floor every provider meets and well below anything in current use.
		if n.BitLen() < 2048 {
			return nil, errorf("rsa modulus is %d bits, want at least 2048", n.BitLen())
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil

	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, errorf("unsupported curve %q", k.Crv)
		}
		x, err := b64uint(k.X)
		if err != nil {
			return nil, errorf("ec x: %w", err)
		}
		y, err := b64uint(k.Y)
		if err != nil {
			return nil, errorf("ec y: %w", err)
		}
		pub := &ecdsa.PublicKey{Curve: curve, X: x, Y: y}
		if !curve.IsOnCurve(x, y) {
			return nil, errorf("ec point is not on curve %s", k.Crv)
		}
		return pub, nil

	default:
		return nil, errorf("unsupported key type %q", k.Kty)
	}
}

// b64uint decodes a base64url-encoded big-endian unsigned integer, which is how
// JWK encodes every numeric key parameter.
func b64uint(s string) (*big.Int, error) {
	if s == "" {
		return nil, errorf("empty value")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(raw), nil
}
