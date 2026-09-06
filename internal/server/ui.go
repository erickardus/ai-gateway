package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/reqlog"
	"github.com/erickardus/ai-gateway/internal/spend"
	"github.com/erickardus/ai-gateway/internal/ui"
)

// uiPrefix is where the admin UI is mounted. It matches ui.CookiePath, and has
// to: the session cookie is scoped to that path, so moving one without the
// other would either widen the cookie's reach or sign the operator out.
const uiPrefix = ui.CookiePath

// UseUI enables the admin UI and its API.
//
// Like UseSSO, it is wired after construction and a nil argument leaves the
// routes unregistered, so a gateway with no UI serves no browser surface at
// all — not a sign-in page, not a 401, nothing.
func (s *Server) UseUI(sessions *ui.Sessions) {
	if sessions == nil {
		return
	}
	s.uiSessions = sessions
}

// registerUI mounts the UI's routes.
//
// Every route lives under /ui, which is what lets the session cookie be scoped
// there. That scoping is the reason the UI does not simply reuse /key/* and
// /spend/* with a cookie: a cookie those paths could read would be a cookie the
// browser also attaches to /v1/messages.
func (s *Server) registerUI(mux *http.ServeMux) {
	if s.uiSessions == nil {
		return
	}

	// The session routes are the only ones outside the gate, because they are
	// how a request gets through it.
	mux.HandleFunc("POST "+uiPrefix+"/api/session", s.handleUILogin)
	mux.HandleFunc("GET "+uiPrefix+"/api/session", s.handleUISession)
	mux.HandleFunc("POST "+uiPrefix+"/api/session/delete", s.handleUILogout)

	mux.HandleFunc("GET "+uiPrefix+"/api/overview", s.uiGated(s.handleUIOverview))
	mux.HandleFunc("GET "+uiPrefix+"/api/deployments", s.uiGated(s.handleUIDeployments))
	mux.HandleFunc("GET "+uiPrefix+"/api/traffic", s.uiGated(s.handleUITraffic))
	mux.HandleFunc("GET "+uiPrefix+"/api/scopes", s.uiGated(s.handleUIScopes))

	mux.HandleFunc("GET "+uiPrefix+"/api/keys", s.uiGated(s.handleUIKeys))
	mux.HandleFunc("POST "+uiPrefix+"/api/keys/generate", s.uiGated(s.keyGenerate))
	mux.HandleFunc("POST "+uiPrefix+"/api/keys/update", s.uiGated(s.keyUpdate))
	mux.HandleFunc("POST "+uiPrefix+"/api/keys/delete", s.uiGated(s.keyDelete))

	mux.HandleFunc("GET "+uiPrefix+"/api/spend/keys", s.uiGated(func(w http.ResponseWriter, r *http.Request) {
		s.writeSpend(w, r, func() ([]spend.Summary, error) { return s.ledger.Keys(r.Context()) })
	}))
	mux.HandleFunc("GET "+uiPrefix+"/api/spend/scopes", s.uiGated(func(w http.ResponseWriter, r *http.Request) {
		s.writeSpend(w, r, func() ([]spend.Summary, error) { return s.ledger.Scopes(r.Context()) })
	}))
	mux.HandleFunc("GET "+uiPrefix+"/api/spend/deployments", s.uiGated(func(w http.ResponseWriter, r *http.Request) {
		s.writeSpend(w, r, func() ([]spend.Summary, error) { return s.ledger.Deployments(r.Context()) })
	}))

	mux.HandleFunc("POST "+uiPrefix+"/api/cache/purge", s.uiGated(func(w http.ResponseWriter, r *http.Request) {
		if s.cache == nil {
			writeError(w, http.StatusNotFound, "not_found_error", "response caching is not enabled")
			return
		}
		s.cachePurge(w, r)
	}))

	assets, built := ui.Assets(uiPrefix)
	if !built {
		s.log.Warn("admin ui enabled but no assets were built into this binary; run `make ui`")
	}
	// Registered as a subtree, which is what serves every client-side route
	// from the same document. ServeMux redirects the bare /ui to /ui/ on its
	// own, so the trailing slash the relative asset URLs need is not something
	// this has to arrange.
	mux.Handle("GET "+uiPrefix+"/", assets)
}

// uiGated wraps a handler in the browser-session check.
func (s *Server) uiGated(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireUISession(w, r) {
			return
		}
		fn(w, r)
	}
}

// requireUISession authenticates a browser request. It reports whether the
// request may proceed.
func (s *Server) requireUISession(w http.ResponseWriter, r *http.Request) bool {
	if s.uiSessions == nil {
		writeError(w, http.StatusNotFound, "not_found_error", "the admin ui is not enabled")
		return false
	}
	cookie, err := r.Cookie(ui.CookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_error", "sign in to the admin ui")
		return false
	}
	if _, ok := s.uiSessions.Validate(cookie.Value); !ok {
		// Clear the cookie on the way out. A browser holding an expired token
		// would otherwise keep presenting it, and the UI cannot tell "expired"
		// from "never signed in" without being told.
		s.clearUICookie(w, r)
		writeError(w, http.StatusUnauthorized, "authentication_error", "the admin session has expired")
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead &&
		r.Header.Get(ui.CSRFHeader) == "" {
		writeError(w, http.StatusForbidden, "permission_error",
			"the "+ui.CSRFHeader+" header is required on requests that change state")
		return false
	}
	return true
}

// handleUILogin exchanges the master key for a session cookie.
//
// The master key is the most powerful credential the gateway has, and a form
// that asks for it is a form that can be imitated. Three things narrow that:
// the session it issues is short-lived, the cookie is scoped to /ui so it can
// never reach the inference plane, and the key itself is never written
// anywhere the browser can read it back — the response carries a session, not
// the credential that bought it.
func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	if s.uiSessions == nil {
		writeError(w, http.StatusNotFound, "not_found_error", "the admin ui is not enabled")
		return
	}
	var req struct {
		MasterKey string `json:"master_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if !s.auth.IsMasterKey(core.StripScheme(strings.TrimSpace(req.MasterKey))) {
		s.log.Warn("admin ui sign-in refused", "remote", r.RemoteAddr,
			"request_id", RequestIDFrom(r.Context()))
		writeError(w, http.StatusUnauthorized, "authentication_error", "that is not the master key")
		return
	}

	token, expires := s.uiSessions.Issue(uiSubjectMaster)
	http.SetCookie(w, &http.Cookie{
		Name:     ui.CookieName,
		Value:    token,
		Path:     ui.CookiePath,
		Expires:  expires,
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
	s.log.Info("admin ui sign-in", "remote", r.RemoteAddr, "expires", expires,
		"request_id", RequestIDFrom(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{
		"subject": uiSubjectMaster,
		"expires": expires,
	})
}

// uiSubjectMaster labels a session bought with the master key. It is a constant
// rather than a blank so that logs and the UI's own header have something to
// name, and so that a session issued by some future SSO login is visibly a
// different thing.
const uiSubjectMaster = "master"

func (s *Server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	s.clearUICookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// handleUISession reports whether the caller is signed in.
//
// It answers 200 or 401 rather than failing, because it is the first request
// the SPA makes and its only job is to decide between the sign-in page and the
// console.
func (s *Server) handleUISession(w http.ResponseWriter, r *http.Request) {
	if !s.requireUISession(w, r) {
		return
	}
	cookie, _ := r.Cookie(ui.CookieName)
	subject, _ := s.uiSessions.Validate(cookie.Value)
	writeJSON(w, http.StatusOK, map[string]any{"subject": subject})
}

func (s *Server) clearUICookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     ui.CookieName,
		Value:    "",
		Path:     ui.CookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// requestIsSecure reports whether the browser reached the gateway over TLS.
//
// The forwarded header is honoured because the usual deployment terminates TLS
// at an ingress and speaks plaintext to the gateway behind it; without it the
// cookie would lose its Secure attribute in exactly the arrangement that most
// needs it. A client can of course claim https falsely, but the only thing that
// buys is a cookie its own browser then refuses to send over plaintext.
func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// handleUIOverview answers the console's landing page in one request.
//
// It is composed server-side rather than stitched from /health, /spend/* and
// /key/list in the browser because those four round trips would each observe a
// slightly different instant, and a page that reports 12 deployments healthy
// beside a total of 11 is worse than a page that waits.
func (s *Server) handleUIOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, healthy := s.deploymentRows(ctx)

	overview := map[string]any{
		"status":              healthStatus(healthy, len(rows)),
		"strategy":            s.router.Strategy(),
		"healthy_deployments": healthy,
		"total_deployments":   len(rows),
		"deployments":         rows,
		"prompt_cache":        s.promptCacheStatus(),
		"features": map[string]any{
			"metrics":        s.metrics != nil && s.cfg.Observability.Metrics,
			"response_cache": s.cache != nil,
			"spend":          s.ledger != nil,
			"sso":            s.sso != nil,
			"rbac":           s.cfg.RBAC.Enabled(),
			"shared_state":   s.shared != nil,
		},
		"traffic": map[string]any{
			"held":     s.traffic.Len(),
			"capacity": s.traffic.Cap(),
			// Said plainly rather than implied: a fleet behind a load balancer
			// shows each instance only what it served.
			"note": "Recent requests are held in the instance that served them. Behind a load balancer this is one instance's traffic, not the fleet's.",
		},
	}

	if s.shared != nil {
		overview["shared_state"] = map[string]any{
			"degradations": s.shared.Degradations(),
			"reachable":    s.shared.Ping(ctx) == nil,
		}
	}

	if keys, err := s.store.List(ctx); err == nil {
		overview["keys"] = summarizeKeys(keys, time.Now())
	}

	if s.ledger != nil {
		if rows, err := s.ledger.Keys(ctx); err == nil {
			overview["spend"] = totalOf(rows)
		}
	}

	writeJSON(w, http.StatusOK, overview)
}

func healthStatus(healthy, total int) string {
	if total > 0 && healthy == 0 {
		return "unhealthy"
	}
	return "healthy"
}

// summarizeKeys counts the key population by the states an operator acts on.
func summarizeKeys(keys []*core.Key, now time.Time) map[string]int {
	out := map[string]int{"total": len(keys)}
	for _, k := range keys {
		switch {
		case k.Blocked:
			out["blocked"]++
		case k.Expired(now):
			out["expired"]++
		}
		if k.Subject != "" {
			out["sso_issued"]++
		}
		if k.Scope != "" {
			out["scoped"]++
		}
	}
	return out
}

// totalOf sums a spend report into the figures the overview shows.
func totalOf(rows []spend.Summary) map[string]any {
	var total spend.Totals
	for _, row := range rows {
		total.Requests += row.Requests
		total.BillableRequests += row.BillableRequests
		total.InputTokens += row.InputTokens
		total.OutputTokens += row.OutputTokens
		total.CacheReadTokens += row.CacheReadTokens
		total.CacheWriteTokens += row.CacheWriteTokens
		total.Cost += row.Cost
		total.CacheSavings += row.CacheSavings
	}
	return map[string]any{
		"requests":           total.Requests,
		"billable_requests":  total.BillableRequests,
		"input_tokens":       total.InputTokens,
		"output_tokens":      total.OutputTokens,
		"cache_read_tokens":  total.CacheReadTokens,
		"cache_write_tokens": total.CacheWriteTokens,
		"cost":               total.Cost,
		"cache_savings":      total.CacheSavings,
	}
}

// uiDeployment is one row of the deployments view: the routing state /health
// reports, joined to the configuration and the spend that explain it.
type uiDeployment struct {
	deploymentHealth
	Weight int    `json:"weight"`
	RPM    int    `json:"rpm,omitempty"`
	TPM    int    `json:"tpm,omitempty"`
	Model  string `json:"upstream_model,omitempty"`
	// Billable is false for a passthrough deployment. It is reported beside the
	// price rather than left to be inferred from auth_mode, because "this
	// deployment has no pricing" and "this deployment is never billed to us"
	// look identical in a table of zeroes.
	Billable bool         `json:"billable"`
	Cost     uiPricing    `json:"cost"`
	Spend    spend.Totals `json:"spend"`
	Priced   bool         `json:"priced"`
}

// uiPricing is the part of core.Pricing a table shows.
//
// It is restated here rather than encoding core.Pricing directly because that
// type is a YAML shape with no JSON tags, and serializing it would publish Go
// field names as an API. The fields left out — the one-hour write tier, the
// free-writes declaration — are facts about how a price is derived rather than
// the price itself, and belong in the config file the operator already reads.
type uiPricing struct {
	InputPer1M      float64 `json:"input_per_1m"`
	OutputPer1M     float64 `json:"output_per_1m"`
	CacheReadPer1M  float64 `json:"cache_read_per_1m"`
	CacheWritePer1M float64 `json:"cache_write_per_1m"`
}

func (s *Server) handleUIDeployments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, healthy := s.deploymentRows(ctx)

	byID := make(map[string]*config.Deployment, len(s.cfg.ModelList))
	for i := range s.cfg.ModelList {
		d := &s.cfg.ModelList[i]
		byID[d.ID()] = d
	}
	spends := make(map[string]spend.Totals)
	if s.ledger != nil {
		if summaries, err := s.ledger.Deployments(ctx); err == nil {
			for _, sum := range summaries {
				spends[sum.Subject] = sum.Totals
			}
		}
	}

	out := make([]uiDeployment, 0, len(rows))
	for _, row := range rows {
		view := uiDeployment{deploymentHealth: row, Spend: spends[row.Deployment]}
		if d := byID[row.Deployment]; d != nil {
			view.Weight = d.Share()
			view.RPM, view.TPM = d.RPM, d.TPM
			view.Model = d.Params.Model
			view.Cost = uiPricing{
				InputPer1M:      d.Cost.InputPer1M,
				OutputPer1M:     d.Cost.OutputPer1M,
				CacheReadPer1M:  d.Cost.CacheReadPer1M,
				CacheWritePer1M: d.Cost.CacheWritePer1M,
			}
		}
		pricing := s.pricing[row.Deployment]
		view.Billable = pricing.billable
		view.Priced = pricing.billable && pricing.price != (core.Pricing{})
		out = append(out, view)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"deployments":         out,
		"healthy_deployments": healthy,
		"total_deployments":   len(out),
		"strategy":            s.router.Strategy(),
		"prompt_cache":        s.promptCacheStatus(),
	})
}

// handleUIKeys lists keys with the spend and budget each is measured against.
//
// The join happens here because a key's budget lives in the key store and the
// spend it is drawn against lives in the ledger, under a subject that is the
// key's hash for an ordinary key and the person for one issued by SSO. Asking
// the browser to reproduce SpendSubject would be asking it to reimplement the
// one rule that stops a developer resetting their own budget by signing in
// again.
func (s *Server) handleUIKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	keys, err := s.store.List(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	spent := make([]float64, len(keys))
	if s.ledger != nil {
		subjects := make([]spend.Subject, len(keys))
		for i, k := range keys {
			subjects[i] = spend.Subject{Kind: spend.KindKey, ID: k.SpendSubject(), Window: k.BudgetDuration}
		}
		if values, err := s.ledger.Spends(ctx, subjects); err == nil {
			spent = values
		}
	}

	type keyView struct {
		*core.Key
		// SpendSubject is exposed so the UI can group a person's devices: an
		// SSO-issued key shares its subject with every other key that login
		// produced, which is what makes revoking a person a distinct action
		// from revoking a credential.
		SpendSubject string  `json:"spend_subject"`
		Spend        float64 `json:"spend"`
	}
	out := make([]keyView, len(keys))
	for i, k := range keys {
		out[i] = keyView{Key: k, SpendSubject: k.SpendSubject(), Spend: spent[i]}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"keys":   out,
		"count":  len(out),
		"scopes": s.cfg.ScopeIDs(),
		"groups": s.groupNames(),
	})
}

// groupNames lists the configured model groups, so the key form offers what
// exists rather than a free-text field that fails at first use.
func (s *Server) groupNames() []string {
	groups := s.router.Groups()
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// handleUIScopes reports the RBAC hierarchy with each level's pooled spend.
//
// The tree is read-only. It is declared in gateway.yaml and resolved at load,
// and an admin page that edited it would be an admin page that rewrites the
// operator's configuration file behind their back.
func (s *Server) handleUIScopes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scopes := s.cfg.Scopes()

	spent := make(map[string]float64, len(scopes))
	ids := s.cfg.ScopeIDs()
	if s.ledger != nil && len(ids) > 0 {
		subjects := make([]spend.Subject, len(ids))
		for i, id := range ids {
			// SpendSubject, not the bare id. The ledger is written by
			// auth.Context.ScopeSubjects, which qualifies each level by its
			// kind — "team:acme/platform" — so reading by id alone misses every
			// row and reports a team at its cap as having spent nothing.
			subjects[i] = spend.Subject{
				Kind:   spend.KindScope,
				ID:     scopes[id].SpendSubject(),
				Window: scopes[id].BudgetDuration,
			}
		}
		if values, err := s.ledger.Spends(ctx, subjects); err == nil {
			for i, id := range ids {
				spent[id] = values[i]
			}
		}
	}

	type scopeView struct {
		*core.Scope
		Parent string  `json:"parent,omitempty"`
		Spend  float64 `json:"spend"`
		Keys   int     `json:"keys"`
	}

	counts := make(map[string]int)
	if keys, err := s.store.List(ctx); err == nil {
		for _, k := range keys {
			if k.Scope != "" {
				counts[k.Scope]++
			}
		}
	}

	out := make([]scopeView, 0, len(ids))
	for _, id := range ids {
		sc := scopes[id]
		view := scopeView{Scope: sc, Spend: spent[id], Keys: counts[id]}
		if sc.Parent != nil {
			view.Parent = sc.Parent.ID
		}
		out = append(out, view)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scopes":  out,
		"enabled": s.cfg.RBAC.Enabled(),
		"note":    "Declared in gateway.yaml and read-only here. A scope's spend is one pool every key beneath it draws from, not the sum of their individual budgets.",
	})
}

// handleUITraffic serves the recent-request buffer.
func (s *Server) handleUITraffic(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, 500)
		}
	}
	filter := reqlog.Filter{
		SpendSubject: q.Get("spend_subject"),
		ModelGroup:   q.Get("model_group"),
		Deployment:   q.Get("deployment"),
		Outcome:      q.Get("outcome"),
		Errors:       q.Get("errors") == "true",
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"records":  s.traffic.Recent(limit, filter),
		"held":     s.traffic.Len(),
		"capacity": s.traffic.Cap(),
		"note":     "Metadata only — no request or response bodies are retained. Held in the instance that served the traffic.",
	})
}

// writeJSON encodes a UI response. The management endpoints hand-roll this
// because they answer JSON on success and the Anthropic error envelope on
// failure, and only the success half is uniform.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
