package sso

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"hash"
	"math/big"
	"strings"
	"time"
)

// clockSkew is how far the gateway's clock may differ from the provider's
// before a token that is genuinely valid is refused. A minute is the usual
// allowance and small enough that it does not meaningfully extend a token.
const clockSkew = time.Minute

// signatureAlg describes one algorithm the gateway will verify.
type signatureAlg struct {
	hash   crypto.Hash
	newSum func() hash.Hash
	// ecKeyBytes is the size of each half of an ECDSA signature, and zero for
	// RSA.
	ecKeyBytes int
}

// supportedAlgs is an allowlist, not a lookup that falls through to a default.
//
// The absences matter more than the entries. "none" is the canonical JWT
// forgery: a token declaring it has no signature to check, and an
// implementation that honours the header's own claim about how to verify it
// accepts anything. The HMAC family is absent for a subtler version of the same
// problem — verifying an asymmetric token with a symmetric algorithm makes the
// public key the verification secret, so anyone who can read the provider's
// published key set can sign an identity.
var supportedAlgs = map[string]signatureAlg{
	"RS256": {hash: crypto.SHA256, newSum: sha256.New},
	"RS384": {hash: crypto.SHA384, newSum: sha512.New384},
	"RS512": {hash: crypto.SHA512, newSum: sha512.New},
	"PS256": {hash: crypto.SHA256, newSum: sha256.New},
	"PS384": {hash: crypto.SHA384, newSum: sha512.New384},
	"PS512": {hash: crypto.SHA512, newSum: sha512.New},
	"ES256": {hash: crypto.SHA256, newSum: sha256.New, ecKeyBytes: 32},
	"ES384": {hash: crypto.SHA384, newSum: sha512.New384, ecKeyBytes: 48},
	"ES512": {hash: crypto.SHA512, newSum: sha512.New, ecKeyBytes: 66},
}

// jwtHeader is the token's own description of how to verify it. Only the
// algorithm and key id are read, and the algorithm is checked against the
// allowlist rather than trusted.
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// audience is an "aud" claim, which the specification allows to be either a
// single string or an array of them.
type audience []string

func (a *audience) UnmarshalJSON(raw []byte) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return errorf("aud: neither a string nor an array of strings")
	}
	*a = many
	return nil
}

func (a audience) contains(s string) bool {
	for _, v := range a {
		if subtle.ConstantTimeCompare([]byte(v), []byte(s)) == 1 {
			return true
		}
	}
	return false
}

// containsAny reports whether the claim names any acceptable audience. An empty
// allowlist matches nothing, so a misconfiguration refuses tokens rather than
// accepting all of them.
func (a audience) containsAny(want []string) bool {
	return len(a.matching(want)) > 0
}

// matching returns the acceptable audiences this token actually carries.
//
// Which ones matched is the question the authorized-party check turns on, and
// it is not answerable from the configured list alone: a gateway accepting two
// audiences may be handed a token naming only one of them.
func (a audience) matching(want []string) []string {
	var out []string
	for _, w := range want {
		if w != "" && a.contains(w) {
			out = append(out, w)
		}
	}
	return out
}

// idClaims is the subset of an ID token the gateway checks or reads. Everything
// else is kept in the raw map, where the configured role claim is looked up.
type idClaims struct {
	Iss   string   `json:"iss"`
	Sub   string   `json:"sub"`
	Aud   audience `json:"aud"`
	Azp   string   `json:"azp"`
	Exp   int64    `json:"exp"`
	Nbf   int64    `json:"nbf"`
	Iat   int64    `json:"iat"`
	Nonce string   `json:"nonce"`

	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// Verify checks an ID token and returns who it says is logging in.
//
// The order is deliberate: the signature is checked before any claim is read,
// so nothing in an unverified token influences what the gateway does with it.
func (p *Provider) Verify(ctx context.Context, raw, wantNonce string) (*Identity, error) {
	return p.verify(ctx, raw, wantNonce, []string{p.cfg.ClientID})
}

// verify is the shared check behind both the login flow and per-request token
// authentication. The two differ in one claim and one absence: which audiences
// are acceptable, and whether there is a nonce to tie the token to a login the
// gateway started. Everything else — the algorithm allowlist, the signature,
// the issuer, the expiry window — is identical, and is identical because a
// token presented on a request is not a weaker assertion than one presented at
// login.
func (p *Provider) verify(ctx context.Context, raw, wantNonce string, audiences []string) (*Identity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errorf("id token: want three dot-separated segments, got %d", len(parts))
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errorf("id token header: %w", err)
	}
	var h jwtHeader
	if err := json.Unmarshal(headerRaw, &h); err != nil {
		return nil, errorf("id token header: %w", err)
	}
	alg, ok := supportedAlgs[h.Alg]
	if !ok {
		return nil, errorf("id token: algorithm %q is not accepted; the gateway verifies RSA and ECDSA signatures against the provider's published keys", h.Alg)
	}

	key, err := p.publicKey(ctx, h.Kid)
	if err != nil {
		return nil, errorf("id token: %w", err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errorf("id token signature: %w", err)
	}
	signed := []byte(parts[0] + "." + parts[1])
	if err := verifySignature(h.Alg, alg, key, signed, sig); err != nil {
		return nil, errorf("id token: %w", err)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errorf("id token payload: %w", err)
	}
	var c idClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errorf("id token payload: %w", err)
	}

	if strings.TrimSuffix(c.Iss, "/") != strings.TrimSuffix(p.cfg.Issuer, "/") {
		return nil, errorf("id token: issued by %q, want %q", c.Iss, p.cfg.Issuer)
	}
	if !c.Aud.containsAny(audiences) {
		// A token minted for a different client of the same provider is a valid
		// token that says nothing about this gateway. Accepting one would let
		// any other application in the organisation issue gateway keys.
		return nil, errorf("id token: not issued for this client")
	}
	// The authorized-party check turns on which audience this token was
	// actually accepted under, not on how many the operator configured.
	//
	// Keying it on the configured list is a hole: an operator who accepts both
	// an ID-token audience and an access-token one — the natural config for
	// serving logins and services together — would lose the check for the ID
	// tokens as well, and a token another client obtained naming this gateway
	// among its audiences would be accepted. The narrow reason to relax it is
	// an access token, whose "aud" is the API it was minted for and whose "azp"
	// is the client that asked; requiring azp to equal the gateway's client id
	// there would refuse every such token.
	//
	// So it applies when the only thing that made this token acceptable is that
	// it names the gateway's own client id.
	matched := c.Aud.matching(audiences)
	if len(c.Aud) > 1 && c.Azp != "" && c.Azp != p.cfg.ClientID &&
		len(matched) == 1 && matched[0] == p.cfg.ClientID {
		return nil, errorf("id token: authorized party is %q, want %q", c.Azp, p.cfg.ClientID)
	}
	if c.Sub == "" {
		return nil, errorf("id token: no subject claim")
	}

	now := p.now()
	if c.Exp == 0 {
		return nil, errorf("id token: no expiry claim")
	}
	if now.After(time.Unix(c.Exp, 0).Add(clockSkew)) {
		return nil, errorf("id token: expired at %s", time.Unix(c.Exp, 0).UTC().Format(time.RFC3339))
	}
	if c.Nbf != 0 && now.Add(clockSkew).Before(time.Unix(c.Nbf, 0)) {
		return nil, errorf("id token: not valid until %s", time.Unix(c.Nbf, 0).UTC().Format(time.RFC3339))
	}
	if c.Iat != 0 && now.Add(clockSkew).Before(time.Unix(c.Iat, 0)) {
		return nil, errorf("id token: issued in the future, at %s", time.Unix(c.Iat, 0).UTC().Format(time.RFC3339))
	}

	// The nonce ties this token to the login the gateway started. Without it a
	// token captured from another flow could be replayed into this one.
	//
	// A renewal has no nonce to check, because there was no authorization
	// request: the caller passes an empty want, and a provider that echoes one
	// anyway is not a problem.
	if wantNonce != "" {
		if subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(wantNonce)) != 1 {
			return nil, errorf("id token: nonce does not match this login")
		}
	}

	var rawClaims map[string]any
	if err := json.Unmarshal(payload, &rawClaims); err != nil {
		return nil, errorf("id token payload: %w", err)
	}

	name := c.Name
	if name == "" {
		name = c.PreferredUsername
	}
	return &Identity{
		Subject: c.Sub,
		Email:   c.Email,
		Name:    name,
		Roles:   claimValues(rawClaims, p.cfg.RoleClaim),
		Expiry:  time.Unix(c.Exp, 0).UTC(),
	}, nil
}

// verifySignature checks the signature with the algorithm the header named,
// having already established that the gateway accepts that algorithm.
func verifySignature(name string, alg signatureAlg, key any, signed, sig []byte) error {
	sum := alg.newSum()
	sum.Write(signed)
	digest := sum.Sum(nil)

	switch k := key.(type) {
	case *rsa.PublicKey:
		if alg.ecKeyBytes != 0 {
			return errorf("algorithm %s names an ECDSA signature but the key is RSA", name)
		}
		if strings.HasPrefix(name, "PS") {
			return rsa.VerifyPSS(k, alg.hash, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto, Hash: alg.hash})
		}
		return rsa.VerifyPKCS1v15(k, alg.hash, digest, sig)

	case *ecdsa.PublicKey:
		if alg.ecKeyBytes == 0 {
			return errorf("algorithm %s names an RSA signature but the key is ECDSA", name)
		}
		// An ECDSA JWS signature is r and s concatenated, each padded to the
		// curve's byte size — not the ASN.1 form the standard library's other
		// entry points take.
		if len(sig) != 2*alg.ecKeyBytes {
			return errorf("ecdsa signature is %d bytes, want %d", len(sig), 2*alg.ecKeyBytes)
		}
		r := new(big.Int).SetBytes(sig[:alg.ecKeyBytes])
		s := new(big.Int).SetBytes(sig[alg.ecKeyBytes:])
		if !ecdsa.Verify(k, digest, r, s) {
			return errorf("signature does not verify")
		}
		return nil

	default:
		return errorf("unsupported key type %T", key)
	}
}
