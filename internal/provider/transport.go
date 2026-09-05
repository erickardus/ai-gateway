package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/jsonx"
)

// maxErrorBodyBytes bounds how much of an upstream error body is buffered for
// relay, so a misbehaving upstream cannot exhaust memory on the error path.
const maxErrorBodyBytes = 1 << 20

// Request is one attempt at an upstream call, independent of which deployment
// eventually serves it.
type Request struct {
	// Path is the upstream path, e.g. "/v1/messages". Query strings are carried
	// separately so routing can match on path alone.
	Path   string
	Query  string
	Body   []byte
	Header http.Header
	Creds  auth.Credentials
	Stream bool
}

// Response is an upstream response whose body has not yet been consumed. The
// caller owns Body and must close it.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	Deployment string
}

// Client executes upstream requests.
type Client struct {
	http           *http.Client
	keyHeaderNames []string
	allowedHosts   map[string]bool
}

// NewClient builds a Client. allowedHosts bounds where a passthrough deployment
// may relay a caller credential.
func NewClient(keyHeaderNames, allowedHosts []string, timeout time.Duration) *Client {
	allowed := make(map[string]bool, len(allowedHosts))
	for _, h := range allowedHosts {
		allowed[config.NormalizeHost(h)] = true
	}
	return &Client{
		http: &http.Client{
			// No client-level timeout: streaming responses run long, and the
			// per-attempt deadline is carried on the request context instead.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				// Responses must reach the client as they arrive, so no
				// response buffering is configured here.
			},
		},
		keyHeaderNames: keyHeaderNames,
		allowedHosts:   allowed,
	}
}

// Do executes req against one deployment and returns as soon as the response
// headers arrive, leaving the body unread. Separating "headers received" from
// "body relayed" is what lets the router retry a failed attempt before any bytes
// have reached the client, and never after.
func (c *Client) Do(ctx context.Context, dep *config.Deployment, req *Request) (*Response, error) {
	params := dep.Params

	if params.AuthMode == core.AuthModePassthrough {
		host := config.HostOf(params.APIBase)
		if host == "" || !c.allowedHosts[host] {
			return nil, fmt.Errorf("deployment %s targets %q: %w", dep.ID(), host, core.ErrUpstreamHostNotAllowed)
		}
	}

	body := req.Body
	// Rewrite the model only when the upstream's identifier differs from the
	// public name. When they match the body is forwarded byte for byte.
	if params.Model != "" && params.Model != dep.ModelName {
		rewritten, err := jsonx.SetTopLevelString(body, "model", params.Model)
		if err != nil {
			return nil, fmt.Errorf("rewrite model for deployment %s: %w", dep.ID(), err)
		}
		body = rewritten
	}

	target, err := buildURL(params.APIBase, req.Path, req.Query)
	if err != nil {
		return nil, fmt.Errorf("deployment %s: %w", dep.ID(), err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request for %s: %w", dep.ID(), err)
	}
	httpReq.Header = BuildUpstreamHeaders(req.Header, params, req.Creds, c.keyHeaderNames)
	httpReq.ContentLength = int64(len(body))

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call deployment %s: %w", dep.ID(), err)
	}

	if resp.StatusCode >= 400 {
		// Buffer the error body so it can be relayed verbatim and so the
		// connection is released for reuse. Claude Code matches on the
		// upstream's own error wording, so it must not be rewrapped.
		defer resp.Body.Close()
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if readErr != nil {
			errBody = nil
		}
		return nil, &core.UpstreamError{
			StatusCode: resp.StatusCode,
			Body:       errBody,
			Header:     resp.Header.Clone(),
			Deployment: dep.ID(),
		}
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		Deployment: dep.ID(),
	}, nil
}

// buildURL joins a deployment's base URL with the request path, preserving any
// path prefix on the base (so an api_base of https://host/anthropic works).
func buildURL(apiBase, path, query string) (string, error) {
	u, err := url.Parse(apiBase)
	if err != nil {
		return "", fmt.Errorf("parse api_base %q: %w", apiBase, err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = query
	return u.String(), nil
}
