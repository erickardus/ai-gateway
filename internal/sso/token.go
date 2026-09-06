package sso

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// tokenResponse is a token endpoint's reply.
type tokenResponse struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// AuthCodeURL builds the provider's authorization URL for one login.
//
// The state and nonce are the caller's, generated per login and remembered
// alongside the rest of that login's state: state ties the callback back to a
// login this gateway started, nonce ties the resulting token to it.
func (p *Provider) AuthCodeURL(ctx context.Context, state, nonce string) (string, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		return "", errorf("authorization endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", p.cfg.RedirectURL)
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange trades an authorization code for tokens.
func (p *Provider) Exchange(ctx context.Context, code string) (*Tokens, error) {
	return p.token(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {p.cfg.RedirectURL},
	})
}

// Refresh trades a refresh token for fresh ones.
//
// This is what makes renewal an identity check rather than a rubber stamp. The
// gateway could mint a new key from state it already holds, but then a key
// would keep renewing after its owner had been disabled at the provider, and
// offboarding would once again be something an operator has to remember to do
// here as well as there.
func (p *Provider) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	return p.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {strings.Join(p.cfg.Scopes, " ")},
	})
}

// token posts to the token endpoint and reads the reply.
func (p *Provider) token(ctx context.Context, form url.Values) (*Tokens, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}

	form.Set("client_id", p.cfg.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// A confidential client authenticates with HTTP Basic, which every provider
	// accepts and which keeps the secret out of the form body and so out of any
	// log that records one. A public client sends no secret at all.
	if p.cfg.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, errorf("token endpoint: %w", err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, errorf("token endpoint: %s: response is not JSON", resp.Status)
	}
	if tr.Error != "" {
		// The provider's own wording is the most useful thing available when a
		// grant is refused — "invalid_grant" on a renewal usually means the
		// account is gone — so it is relayed rather than flattened.
		if tr.ErrorDescription != "" {
			return nil, errorf("token endpoint: %s: %s", tr.Error, tr.ErrorDescription)
		}
		return nil, errorf("token endpoint: %s", tr.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errorf("token endpoint: %s", resp.Status)
	}
	if tr.IDToken == "" {
		return nil, errorf("token endpoint returned no id_token; check that the openid scope is granted to this client")
	}
	return &Tokens{IDToken: tr.IDToken, RefreshToken: tr.RefreshToken}, nil
}
