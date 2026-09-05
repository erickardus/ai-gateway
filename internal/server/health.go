package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/erickardus/ai-gateway/internal/core"
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
	sort.Strings(names)

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
	return "load balanced across " + itoa(deployments) + " deployments"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
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

// handleHealth reports per-deployment status. It never includes credentials.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	sort.Slice(rows, func(i, j int) bool { return rows[i].Deployment < rows[j].Deployment })

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
		"healthy_deployments": healthy,
		"total_deployments":   len(rows),
		"deployments":         rows,
	})
}

var _ = core.StatusFor
