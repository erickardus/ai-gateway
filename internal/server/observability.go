package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// handleMetrics serves the Prometheus text exposition format.
//
// It is unauthenticated, like the liveness probe, because a scrape target
// usually cannot carry a credential — but unlike /health it exposes no upstream
// hostnames or auth modes, only counters labelled by model group and deployment
// ID. Bind the gateway on a private interface, or put the scrape endpoint behind
// your own ingress, if even those labels are sensitive.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	if s.metrics == nil {
		writeError(w, http.StatusNotFound, "not_found_error", "metrics are not enabled")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := s.metrics.WriteTo(w); err != nil {
		s.log.Warn("write metrics", "error", err)
	}
}

// handleSpendKeys reports consumption per virtual key. Master-key only: it
// discloses what every developer spent.
func (s *Server) handleSpendKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.writeSpend(w, r, func() ([]spend.Summary, error) { return s.ledger.Keys(r.Context()) })
}

// handleSpendDeployments reports consumption per deployment.
func (s *Server) handleSpendDeployments(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.writeSpend(w, r, func() ([]spend.Summary, error) { return s.ledger.Deployments(r.Context()) })
}

func (s *Server) writeSpend(w http.ResponseWriter, r *http.Request, fetch func() ([]spend.Summary, error)) {
	if s.ledger == nil {
		writeError(w, http.StatusNotFound, "not_found_error", "spend accounting is not enabled")
		return
	}
	rows, err := fetch()
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var totalCost float64
	for _, row := range rows {
		totalCost += row.Cost
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries":    rows,
		"count":      len(rows),
		"total_cost": totalCost,
		"note":       "Cost covers only deployments the operator pays for. Passthrough traffic bills the caller's own subscription and is reported as usage with no cost.",
	})
}

// record accounts for a completed request in both the ledger and the metrics
// registry. It is the gateway's single instrumentation point: everything either
// wants to know is available here, and nothing else has to be measured twice.
func (s *Server) record(r *http.Request, obs observation) {
	// Accounting runs on a context detached from the client's.
	//
	// By the time a request is recorded the response has been relayed, and a
	// client that has hung up leaves r.Context() already cancelled — which would
	// abandon the write to shared storage and let anyone dodge a budget simply
	// by disconnecting. The work is bounded and local, so completing it is both
	// cheap and necessary.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), recordTimeout)
	defer cancel()

	pricing := s.pricing[obs.deployment]
	cost := 0.0
	if pricing.billable {
		cost = pricing.price.Cost(obs.usage)
	}

	if s.ledger != nil && obs.deployment != "" {
		entry := spend.Entry{
			KeyHash:      obs.keyHash,
			KeyAlias:     obs.keyAlias,
			ModelGroup:   obs.model,
			DeploymentID: obs.deployment,
			Usage:        obs.usage,
			Cost:         cost,
			Billable:     pricing.billable,
		}
		if err := s.ledger.Record(ctx, entry); err != nil {
			s.log.Warn("record spend", "error", err, "request_id", RequestIDFrom(r.Context()))
		}
	}

	s.metrics.Observe(obs.toResult(cost))
}

// recordTimeout bounds accounting so a wedged store cannot pin a handler open
// after the response has already been delivered.
const recordTimeout = 5 * time.Second

// observation is what one request produced, gathered at the point it completes.
type observation struct {
	model, deployment  string
	keyHash, keyAlias  string
	usage              core.Usage
	outcome            string
	rejectReason       string
	retries, fallbacks int
	latency            time.Duration
}

// toResult renders an observation for the metrics registry.
func (o observation) toResult(cost float64) metrics.Result {
	return metrics.Result{
		Model:        o.model,
		Deployment:   o.deployment,
		Outcome:      o.outcome,
		Latency:      o.latency,
		Tokens:       o.usage.Total(),
		Cost:         cost,
		Retries:      o.retries,
		Fallbacks:    o.fallbacks,
		RejectReason: o.rejectReason,
	}
}
