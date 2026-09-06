package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// modelEntry is one row of the /v1/models response. The field names match what
// Claude Code reads for gateway model discovery.
type modelEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Object      string `json:"object"`
}

// handleModels lists the configured model groups.
//
// Claude Code fetches this with a short timeout and treats any redirect as
// failure, so it is answered directly and immediately. Note that Claude Code
// skips discovery entirely when its only credential comes from
// ANTHROPIC_CUSTOM_HEADERS, which is the subscription-passthrough setup; the
// endpoint is still served for other clients.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, err := s.auth.Authenticate(r.Context(), r.Header); err != nil {
		s.fail(w, r, err)
		return
	}

	groups := s.router.Groups()
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	slices.Sort(names)

	entries := make([]modelEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, modelEntry{
			ID:          name,
			DisplayName: name,
			Description: describeGroup(len(groups[name])),
			Object:      "model",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   entries,
	})
}

func describeGroup(deployments int) string {
	if deployments == 1 {
		return "1 deployment behind the gateway"
	}
	return "load balanced across " + strconv.Itoa(deployments) + " deployments"
}

// handleLiveliness answers whether the process is running. It is unauthenticated
// so an orchestrator can probe it without a credential.
func (s *Server) handleLiveliness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
}

// handleReadiness answers whether the gateway can serve traffic, which here
// means the key store is reachable.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{"status": "ready", "key_store": "ok"}
	code := http.StatusOK

	if _, err := s.store.List(r.Context()); err != nil {
		status["status"] = "not_ready"
		status["key_store"] = "unavailable"
		code = http.StatusServiceUnavailable
		s.log.Error("readiness: key store unavailable", "error", err)
	}

	// Shared state being down is reported but does not make the instance
	// unready: it still serves correctly, just with per-instance limits, and
	// removing it from the load balancer would make the outage worse.
	if s.shared != nil {
		if err := s.shared.Ping(r.Context()); err != nil {
			status["shared_state"] = "degraded"
			status["shared_state_note"] = "limits and budgets are enforced per instance until Redis recovers"
		} else {
			status["shared_state"] = "ok"
		}
		status["shared_state_degradations"] = s.shared.Degradations()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(status)
}

// deploymentHealth is one row of the /health response.
type deploymentHealth struct {
	Deployment string `json:"deployment"`
	ModelName  string `json:"model_name"`
	Format     string `json:"format"`
	AuthMode   string `json:"auth_mode"`
	APIBase    string `json:"api_base"`
	Cooling    bool   `json:"cooling_down"`
	InFlight   int    `json:"in_flight"`
}

// handleHealth reports per-deployment status. It never includes credentials,
// but it does disclose the upstream topology — hostnames, auth modes and which
// deployments are currently ejected — so it requires a valid key. Orchestrator
// probes should use /health/liveliness, which is unauthenticated by design.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := s.auth.Authenticate(ctx, r.Header); err != nil {
		s.fail(w, r, err)
		return
	}
	groups := s.router.Groups()
	rows := make([]deploymentHealth, 0)
	healthy := 0

	for name, deployments := range groups {
		for _, d := range deployments {
			row := deploymentHealth{
				Deployment: d.ID(),
				ModelName:  name,
				Format:     string(d.Params.Format),
				AuthMode:   string(d.Params.AuthMode),
				APIBase:    d.Params.APIBase,
			}
			row.Cooling, row.InFlight = s.router.DeploymentStatus(ctx, d.ID())
			if !row.Cooling {
				healthy++
			}
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b deploymentHealth) int { return strings.Compare(a.Deployment, b.Deployment) })

	code := http.StatusOK
	overall := "healthy"
	if healthy == 0 && len(rows) > 0 {
		code = http.StatusServiceUnavailable
		overall = "unhealthy"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":              overall,
		"strategy":            s.router.Strategy(),
		"healthy_deployments": healthy,
		"total_deployments":   len(rows),
		"deployments":         rows,
		"prompt_cache":        s.promptCacheStatus(),
	})
}

// promptCacheStatus reports how the gateway is treating the provider's prompt
// cache. It sits on /health beside the strategy because the two decide together
// where a request lands, and because "affinity is on" and "affinity is doing
// anything" are different claims: a fleet of single-deployment groups reports
// the first without the second.
func (s *Server) promptCacheStatus() map[string]any {
	active := s.cfg.PromptCache.AffinityEnabled() && s.anyPinnable()
	status := map[string]any{
		"affinity":                    active,
		"affinity_ttl":                s.cfg.PromptCache.AffinityTTL.String(),
		"affinity_max_in_flight_lead": s.cfg.PromptCache.MaxInFlightLead(),
		"inject":                      s.cfg.PromptCache.Inject,
	}
	if s.cfg.PromptCache.AffinityEnabled() && !active {
		status["affinity_note"] = "configured, but inactive: no model group holds two deployments with separate prompt caches, so there is nothing to pin against"
	}
	return status
}
