package provider

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/erickardus/ai-gateway/internal/translate"
)

// maxErrorBodyBytes bounds how much of an upstream error body is buffered for
// relay, so a misbehaving upstream cannot exhaust memory on the error path.
const maxErrorBodyBytes = 1 << 20

// Annotator supplies the body one deployment should receive.
//
// The gateway annotates a request for its own benefit — a cache breakpoint so
// there is something to cache, a usage option so the reply can be billed — and
// the caller asked for none of it. Which of those an upstream will take is a
// fact about that upstream rather than about the request: a passthrough
// deployment must receive the caller's bytes untouched, breakpoints are an
// Anthropic construct, the usage option belongs only to an OpenAI-compatible
// server, and any upstream may simply refuse a field it does not know.
//
// So the decision belongs here, after routing has chosen a deployment, rather
// than to one body fixed before anything knew where the request was going. One
// body for every attempt is what forces the choice between annotating a
// passthrough deployment and annotating nothing at all.
type Annotator interface {
	// Annotate returns the body to send to dep, and whether it differs from the
	// one that arrived.
	//
	// It returns base unchanged rather than an error when it cannot annotate:
	// an optimization that could not be applied is never a reason to refuse a
	// request.
	Annotate(dep *config.Deployment, base []byte) (body []byte, annotated bool)
	// Refused reports that dep rejected an annotation this Annotator added.
	//
	// It is called only once that is established — the annotated body was
	// refused and the very same request as it arrived was accepted — so a 400
	// the caller earned, which fails both bodies, is never reported through it.
	Refused(dep *config.Deployment)
}

// Request is one attempt at an upstream call, independent of which deployment
// eventually serves it.
type Request struct {
	// Path is the upstream path, e.g. "/v1/messages". Query strings are carried
	// separately so routing can match on path alone.
	Path  string
	Query string
	// Body is the request as the caller sent it, less any gateway directive
	// that must not reach an upstream. It is never mutated: every attempt,
	// retry and fallback hop starts from these same bytes, and whatever one
	// deployment is sent is derived from them by Annotate.
	Body []byte
	// Annotate, when set, produces the body for the deployment an attempt
	// actually reaches. Nil means every deployment is sent Body verbatim.
	Annotate Annotator
	// Format is the wire protocol the caller spoke. The router only dispatches
	// to deployments declaring the same format, since the gateway does not
	// translate between them.
	Format core.Format
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
	// Format is the wire format the bytes in Body are written in, which is the
	// serving deployment's rather than the caller's whenever the request was
	// translated on the way out. The caller needs it to know whether the reply
	// has to be translated back, and to read the reply's usage counters under
	// the right convention.
	Format core.Format
}

// Client executes upstream requests.
type Client struct {
	http           *http.Client
	keyHeaderNames []string
	allowedHosts   map[string]bool
	translation    config.TranslationConfig
}

// NewClient builds a Client. allowedHosts bounds where a passthrough deployment
// may relay a caller credential.
//
// There is deliberately no timeout parameter: a client-level timeout would apply
// to the whole exchange including a streaming body, so deadlines are carried on
// the per-attempt request context instead.
func NewClient(keyHeaderNames, allowedHosts []string, translation config.TranslationConfig) *Client {
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
		translation:    translation,
	}
}

// Do executes req against one deployment and returns as soon as the response
// headers arrive, leaving the body unread. Separating "headers received" from
// "body relayed" is what lets the router retry a failed attempt before any bytes
// have reached the client, and never after.
//
// The body this deployment receives is derived here rather than handed down
// ready-made, so an annotation the gateway adds for its own benefit can be
// applied to the deployments that take one and withheld from those that do not.
func (c *Client) Do(ctx context.Context, dep *config.Deployment, req *Request) (*Response, error) {
	if dep.Params.AuthMode == core.AuthModePassthrough {
		host := config.HostOf(dep.Params.APIBase)
		if host == "" || !c.allowedHosts[host] {
			return nil, fmt.Errorf("deployment %s targets %q: %w", dep.ID(), host, core.ErrUpstreamHostNotAllowed)
		}
	}

	// Translation comes first, where this deployment speaks another format, so
	// that everything after it works on the body the upstream will actually
	// receive. Annotating before translating would add a member in the caller's
	// format and then throw it away rebuilding the document — which is how a
	// translated streamed reply ends up with no usage on it at all, and so
	// costs nothing, charges nothing, and shows up in no budget.
	base, path, err := c.prepare(dep, req)
	if err != nil {
		return nil, err
	}

	body, annotated := base, false
	if req.Annotate != nil {
		body, annotated = req.Annotate.Annotate(dep, base)
	}

	resp, err := c.dispatch(ctx, dep, req, body, path)
	if err == nil || !annotated || !refusedAsBadRequest(err) {
		return resp, err
	}

	// The upstream refused something the caller never asked for, so it gets one
	// more attempt with the request exactly as it arrived. That second answer
	// settles it either way: accepted, and the annotation was the cause, so this
	// deployment is not annotated again and the round trip is paid once rather
	// than on every request; refused again, and the 400 was the caller's own and
	// is relayed as they wrote it, wording intact.
	//
	// Refused is called only on the accepting branch. A 400 the caller earned
	// fails both bodies, and reading that as "this upstream will not take an
	// annotation" would let one malformed request switch the optimization off
	// for every other caller of the deployment.
	plain, plainErr := c.dispatch(ctx, dep, req, base, path)
	if plainErr != nil {
		return nil, plainErr
	}
	req.Annotate.Refused(dep)
	return plain, nil
}

// refusedAsBadRequest reports whether an upstream rejected the request outright,
// which is the answer an annotation it does not accept produces.
func refusedAsBadRequest(err error) bool {
	var upstream *core.UpstreamError
	return errors.As(err, &upstream) && upstream.StatusCode == http.StatusBadRequest
}

// prepare returns the body and upstream path for one deployment, rewriting the
// request where that deployment speaks a wire format other than the one it
// arrived in.
//
// A passthrough deployment is refused rather than translated. Its body has to
// reach the upstream exactly as the caller wrote it, and its credential is the
// caller's own subscription, so a rewritten body sent there would spend a
// person's personal quota on a document they never wrote. The router already
// declines to select one for a cross-format request; this is the second lock,
// on the path that actually puts bytes on the wire.
func (c *Client) prepare(dep *config.Deployment, req *Request) ([]byte, string, error) {
	if req.Format == "" || dep.Params.Format == req.Format {
		return req.Body, req.Path, nil
	}
	if dep.Params.AuthMode == core.AuthModePassthrough {
		return nil, "", fmt.Errorf("deployment %s speaks %s and the request is %s, and a passthrough deployment is never translated: %w",
			dep.ID(), dep.Params.Format, req.Format, core.ErrNoHealthyDeployment)
	}
	path, ok := translate.UpstreamPath(req.Path, dep.Params.Format)
	if !ok {
		return nil, "", fmt.Errorf("deployment %s: %s has no counterpart in the %s format: %w",
			dep.ID(), req.Path, dep.Params.Format, translate.ErrUnsupported)
	}
	body, err := translate.Request(req.Format, dep.Params.Format, req.Body, translate.Options{
		MaxCompletionTokens: dep.Params.MaxCompletionTokens,
		// The marker is carried only to an upstream whose operator said its
		// cache reads one. Everywhere else it is an unknown member, and an
		// unknown member is a 400 on the whole request.
		KeepCacheControl: dep.Params.SupportsCacheControl,
		DefaultMaxTokens: c.translation.DefaultMaxTokens,
	})
	if err != nil {
		return nil, "", fmt.Errorf("translate request for deployment %s: %w", dep.ID(), err)
	}
	return body, path, nil
}

// dispatch sends one body to one deployment. It is separate from Do because the
// same attempt may send two: the annotated body, and — where that is refused —
// the request as it arrived.
func (c *Client) dispatch(ctx context.Context, dep *config.Deployment, req *Request, body []byte, path string) (*Response, error) {
	params := dep.Params
	// Rewrite the model only when the upstream's identifier differs from the one
	// the body actually carries. Comparing against dep.ModelName instead would
	// skip the rewrite on a fallback hop, where the body still names the
	// original group and the upstream would reject it.
	if params.Model != "" {
		current, err := jsonx.Peek(body)
		if err != nil {
			return nil, fmt.Errorf("inspect body for deployment %s: %w", dep.ID(), err)
		}
		if current.Model != params.Model {
			rewritten, err := jsonx.SetTopLevelString(body, "model", params.Model)
			if err != nil {
				return nil, fmt.Errorf("rewrite model for deployment %s: %w", dep.ID(), err)
			}
			body = rewritten
		}
	}

	target, err := buildURL(params.APIBase, path, req.Query)
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
			// The format the error envelope is written in, which is the
			// deployment's rather than the caller's on a translated request.
			// A client handed an error envelope it cannot parse reads it as a
			// corrupt reply rather than as the refusal it is.
			Format: params.Format,
		}
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		Deployment: dep.ID(),
		Format:     params.Format,
	}, nil
}

// buildURL joins a deployment's base URL with the request path.
//
// Two details matter. A base that already ends in the path's leading segment
// (api_base https://api.openai.com/v1 with path /v1/chat/completions) must not
// double it, and any query string on the base — Azure's api-version, for
// instance — must survive alongside the caller's own.
func buildURL(apiBase, path, query string) (string, error) {
	u, err := url.Parse(apiBase)
	if err != nil {
		return "", fmt.Errorf("parse api_base %q: %w", apiBase, err)
	}

	base := strings.TrimSuffix(u.Path, "/")
	// Drop any leading path segments the base already supplies.
	for _, seg := range strings.Split(strings.Trim(base, "/"), "/") {
		if seg == "" {
			continue
		}
		if after, found := strings.CutPrefix(path, "/"+seg+"/"); found {
			base = strings.TrimSuffix(base, "/"+seg)
			path = "/" + seg + "/" + after
			break
		}
	}
	u.Path = base + path

	switch {
	case u.RawQuery == "":
		u.RawQuery = query
	case query != "":
		u.RawQuery += "&" + query
	}
	return u.String(), nil
}
