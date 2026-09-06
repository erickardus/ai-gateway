package sso

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// discoveryTTL is how long a discovery document is reused. Endpoints move
// rarely, and re-reading one on every login would put a provider round trip in
// front of a developer for information that almost never changes.
const discoveryTTL = time.Hour

// discovery is the subset of the OpenID Connect discovery document the gateway
// uses. Unknown fields are ignored rather than refused: providers add their own,
// and a gateway that rejected a document for carrying an extra key would break
// on a provider's next release.
type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discover returns the provider's discovery document, cached.
func (p *Provider) discover(ctx context.Context) (*discovery, error) {
	p.mu.Lock()
	if p.disc != nil && p.now().Sub(p.discAt) < discoveryTTL {
		d := p.disc
		p.mu.Unlock()
		return d, nil
	}
	p.mu.Unlock()

	d, err := p.fetchDiscovery(ctx)
	if err != nil {
		// A cached document that has merely gone stale is far better than
		// failing a login because the provider was briefly unreachable. The
		// endpoints in it are the same ones that worked an hour ago.
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.disc != nil {
			p.log.Warn("sso: discovery refresh failed, using the cached document", "error", err)
			return p.disc, nil
		}
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.disc, p.discAt = d, p.now()
	return d, nil
}

func (p *Provider) fetchDiscovery(ctx context.Context) (*discovery, error) {
	endpoint := strings.TrimSuffix(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	var d discovery
	if err := p.getJSON(ctx, endpoint, &d); err != nil {
		return nil, errorf("fetch openid configuration: %w", err)
	}

	// The issuer in the document must be the issuer that was asked. Without
	// this check a redirect, a typo or a hijacked well-known path could point
	// the gateway at a provider willing to assert any identity at all, and
	// every token it signed would verify.
	if strings.TrimSuffix(d.Issuer, "/") != strings.TrimSuffix(p.cfg.Issuer, "/") {
		return nil, errorf("openid configuration at %s declares issuer %q, want %q", endpoint, d.Issuer, p.cfg.Issuer)
	}
	for _, f := range []struct {
		name, value string
	}{
		{"authorization_endpoint", d.AuthorizationEndpoint},
		{"token_endpoint", d.TokenEndpoint},
		{"jwks_uri", d.JWKSURI},
	} {
		u, err := url.Parse(f.value)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, errorf("openid configuration at %s: %s is not an absolute URL (%q)", endpoint, f.name, f.value)
		}
	}
	return &d, nil
}

// getJSON performs a bounded GET and decodes the body.
func (p *Provider) getJSON(ctx context.Context, endpoint string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errorf("GET %s: %s", endpoint, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return errorf("decode %s: %w", endpoint, err)
	}
	return nil
}
