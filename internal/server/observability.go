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

// handleCachePurge empties the response cache. Master-key only: it affects
// every caller.
func (s *Server) handleCachePurge(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	if err := s.cache.Purge(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("response cache purged", "request_id", RequestIDFrom(r.Context()))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "purged"})
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

	var totalCost, totalSavings float64
	for _, row := range rows {
		totalCost += row.Cost
		totalSavings += row.CacheSavings
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries":             rows,
		"count":               len(rows),
		"total_cost":          totalCost,
		"total_cache_savings": totalSavings,
		"note":                "Cost covers only deployments the operator pays for. Passthrough traffic bills the caller's own subscription and is reported as usage with no cost.",
		"cache_savings_note":  "What the provider's prompt cache took off the bill, against the same tokens charged as ordinary input. It is not included in cost, which is what was actually charged.",
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
	cost, savings := 0.0, 0.0
	if pricing.billable {
		cost = pricing.price.Cost(obs.usage)
		savings = pricing.price.CacheSavings(obs.usage)
		s.warnUnpricedCacheTier(obs, pricing.price)
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
			CacheSavings: savings,
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
	// promptAffinity records whether the request went to the deployment already
	// holding its prompt prefix. Empty when no pin was consulted.
	promptAffinity string
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

		CacheReadTokens:    o.usage.CacheReadTokens,
		CacheWriteTokens:   o.usage.CacheWriteTokens,
		CacheWrite1hTokens: o.usage.CacheWrite1hTokens,
		PromptAffinity:     o.promptAffinity,
	}
}

// warnUnpricedCacheTier reports, once per deployment, that an upstream billed a
// tier this deployment has no price for.
//
// A one-hour cache write costs twice base input where the default five-minute
// write costs 1.25x. A cost model that omits the longer price falls back to the
// shorter one, which is right until a caller asks for the longer TTL and then
// under-reports every write it makes by more than a third. Nothing else would
// say so: the request succeeds, the tokens are counted, and only the total is
// wrong.
//
// It is a log line rather than a refusal because the traffic is already served
// and the fallback is a defensible default — but it is a log line the operator
// can act on, naming the deployment and the key to add.
func (s *Server) warnUnpricedCacheTier(obs observation, price core.Pricing) {
	if obs.usage.CacheWrite1hTokens == 0 || price.CacheWrite1hPer1M > 0 {
		return
	}
	if _, seen := s.mispriced.LoadOrStore(obs.deployment, true); seen {
		return
	}
	s.log.Warn("upstream reported one-hour cache writes at a deployment priced only for five-minute ones; cost is understated until cost.cache_write_1h_per_1m is set",
		"deployment", obs.deployment,
		"model", obs.model,
		"cache_write_1h_tokens", obs.usage.CacheWrite1hTokens,
		"cache_write_per_1m", price.CacheWritePer1M)
}
