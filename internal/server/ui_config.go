package server

import (
	"net/http"
	"slices"

	"github.com/erickardus/ai-gateway/internal/audit"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// Notes the config view carries where a field would otherwise be read as
// meaning more than it does.
const (
	configTranslationNote = "Off by default. With it on, a request may be routed to a deployment whose wire format differs from the ingress it arrived at, which is what lets one endpoint and one set of virtual keys serve both halves of the ecosystem. A passthrough deployment is never translated whatever this says."
	configNote            = "Read-only. This is what gateway.yaml resolved to in this process, joined to live router state; nothing here can be edited from the console, and no credential is reported — a secret appears only as a boolean saying one is set, and a connection string only as the host and database it names."
)

// uiConfigGroup is one model group as the routing page needs it: the
// deployments behind the name, and the groups a failure moves the traffic to.
//
// The fallback lists are resolved per group rather than handed over as the
// configured rules, because a rule is written from the group's point of view —
// "from kimi-k3, to these" — and a page rendering a group would otherwise have
// to search the whole rule set for every row it draws.
type uiConfigGroup struct {
	Name string `json:"name"`
	// Format is the wire format this group's deployments speak, or "mixed"
	// where translation has put both halves of the ecosystem in one group. A
	// single value would be a lie about exactly the arrangement translation
	// exists to create.
	Format                 string   `json:"format"`
	Deployments            []string `json:"deployments"`
	Fallbacks              []string `json:"fallbacks"`
	ContextWindowFallbacks []string `json:"context_window_fallbacks"`
	ContentPolicyFallbacks []string `json:"content_policy_fallbacks"`
}

// handleUIConfig reports what this instance is configured to do.
//
// It is hand-built field by field rather than encoded from config.Config, and
// that is the whole point of the endpoint existing at all: the configuration
// holds the master key, every upstream API key, the SSO client secret and three
// connection strings with passwords in them, and a projection that started from
// the struct would leak whichever of those somebody forgot to strip. Building
// the answer from named fields makes exposure the deliberate act and omission
// the default.
func (s *Server) handleUIConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg

	fallbacks := fallbackIndex(cfg.Router.Fallbacks)
	contextWindow := fallbackIndex(cfg.Router.ContextWindowFallbacks)
	contentPolicy := fallbackIndex(cfg.Router.ContentPolicyFallbacks)

	routed := s.router.Groups()
	names := make([]string, 0, len(routed))
	for name := range routed {
		names = append(names, name)
	}
	slices.Sort(names)

	groups := make([]uiConfigGroup, 0, len(names))
	for _, name := range names {
		deployments := routed[name]
		ids := make([]string, 0, len(deployments))
		for _, d := range deployments {
			ids = append(ids, d.ID())
		}
		groups = append(groups, uiConfigGroup{
			Name:                   name,
			Format:                 groupFormat(deployments),
			Deployments:            ids,
			Fallbacks:              orEmpty(fallbacks[name]),
			ContextWindowFallbacks: orEmpty(contextWindow[name]),
			ContentPolicyFallbacks: orEmpty(contentPolicy[name]),
		})
	}

	orgs, teams, projects := 0, 0, 0
	for _, scope := range cfg.Scopes() {
		switch scope.Kind {
		case core.ScopeOrganization:
			orgs++
		case core.ScopeTeam:
			teams++
		case core.ScopeProject:
			projects++
		}
	}

	responseCache := map[string]any{
		"enabled":     s.cache != nil,
		"ttl_seconds": int(cfg.Cache.TTL.Seconds()),
		"scope":       string(cfg.Cache.Scope),
		"max_entries": cfg.Cache.MaxEntries,
		"shared":      cfg.Cache.Shared,
	}
	// How full the cache is, for the implementations that can say. A Redis-backed
	// cache is shared with every other instance and holds keys this gateway did
	// not write, so it has no size of its own to report and the field is left
	// out rather than reported as zero.
	if sized, ok := s.cache.(interface{ Len() int }); ok {
		responseCache["entries"] = sized.Len()
	}

	spendView := map[string]any{
		"ledger":     s.ledger != nil,
		"history":    s.history != nil,
		"store_path": cfg.Observability.SpendStorePath,
	}
	if dsn := cfg.Observability.SpendHistory.DSN; dsn != "" {
		// The host and database, never the credential in front of them.
		spendView["history_target"] = spend.PostgresTarget(dsn)
	}

	keys := map[string]any{
		"store_kind": cfg.VirtualKeys.Store.Kind,
		// The master key itself is never reported, here or anywhere else the
		// console can reach. Whether one is set is what an operator needs — it
		// decides whether the management endpoints and this console exist at all
		// — and it is the whole of what can be said without handing the browser
		// the credential its session stands in for.
		"master_key_set": cfg.VirtualKeys.MasterKey != "",
		"header_names":   cfg.VirtualKeys.HeaderNames,
	}
	if dsn := cfg.VirtualKeys.Store.DSN; dsn != "" {
		keys["store_target"] = spend.PostgresTarget(dsn)
	}

	auditView := map[string]any{
		"enabled":  s.auditor != nil,
		"sink":     s.auditSinkKind(),
		"readable": auditIsReadable(s.auditor),
	}
	if dsn := cfg.Audit.DSN; dsn != "" {
		auditView["target"] = audit.PostgresTarget(dsn)
	}
	if cfg.Audit.SinkKind() == config.AuditSinkFile {
		// A path, not a credential, and the one thing an operator reading this
		// page needs in order to go and fetch the chain themselves.
		auditView["path"] = cfg.Audit.Path
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"strategy": s.router.Strategy(),
		"groups":   groups,
		"router": map[string]any{
			"num_retries":             cfg.Router.Retries(),
			"timeout_seconds":         int(cfg.Router.Timeout.Seconds()),
			"stream_timeout_seconds":  int(cfg.Router.StreamTimeout.Seconds()),
			"max_fallback_hops":       cfg.Router.MaxFallbackHops,
			"cooldown_allowed_fails":  cfg.Router.Cooldown.Fails(),
			"cooldown_period_seconds": int(cfg.Router.Cooldown.Period.Seconds()),
			"backoff_initial_ms":      cfg.Router.Backoff.Initial.Milliseconds(),
			"backoff_max_ms":          cfg.Router.Backoff.Max.Milliseconds(),
			"backoff_jitter":          cfg.Router.Backoff.JitterFactor(),
			"lowest_latency_buffer":   cfg.Router.LowestLatencyBuffer,
		},
		"translation": map[string]any{
			"enabled":            cfg.Router.Translation.Enabled,
			"default_max_tokens": cfg.Router.Translation.DefaultMaxTokens,
			"note":               configTranslationNote,
		},
		"response_cache": responseCache,
		"prompt_cache":   s.promptCacheStatus(),
		"spend":          spendView,
		"audit":          auditView,
		"limits": map[string]any{
			"max_body_bytes": cfg.Server.MaxBodyBytes,
			// What the traffic buffer actually holds, which is the ring's own
			// capacity rather than the configured number: a gateway with the
			// console disabled keeps none whatever the file says.
			"request_log_size": s.traffic.Cap(),
		},
		"keys": keys,
		"sso": map[string]any{
			"enabled": s.sso != nil,
			// The issuer is a public URL — the one a browser is redirected to —
			// where the client secret beside it in the configuration is not, and
			// is not reported.
			"issuer": cfg.SSO.Issuer,
		},
		"rbac": map[string]any{
			"enabled":       cfg.RBAC.Enabled(),
			"organizations": orgs,
			"teams":         teams,
			"projects":      projects,
		},
		"note": configNote,
	})
}

// auditIsReadable reports whether the console's audit view can show this sink's
// records, which is the same question handleUIAudit answers and is asked here
// so the routing page can say so without a second request.
func auditIsReadable(sink audit.Sink) bool {
	_, ok := sink.(audit.Reader)
	return ok
}

// fallbackIndex turns the configured rules into a lookup by originating group.
func fallbackIndex(rules []config.FallbackRule) map[string][]string {
	out := make(map[string][]string, len(rules))
	for _, rule := range rules {
		out[rule.From] = append(out[rule.From], rule.To...)
	}
	return out
}

// orEmpty renders an absent list as an empty one, so a group with no fallbacks
// encodes as [] rather than null. The console maps over these, and a null is a
// branch it would otherwise have to carry on three fields.
func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// groupFormat names the wire format a group speaks, or "mixed" where it holds
// both. A group spanning formats is only reachable with translation on, and it
// is exactly the arrangement worth seeing on this page.
func groupFormat(deployments []*config.Deployment) string {
	if len(deployments) == 0 {
		return ""
	}
	format := deployments[0].Params.Format
	for _, d := range deployments[1:] {
		if d.Params.Format != format {
			return "mixed"
		}
	}
	return string(format)
}
