package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/erickardus/ai-gateway/internal/audit"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/reqlog"
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
	s.cachePurge(w, r)
}

func (s *Server) cachePurge(w http.ResponseWriter, r *http.Request) {
	// Audited like a key mutation, and for the same reason: a purge is an
	// administrative action with a cost — every caller's next request pays a
	// full upstream call — and afterwards nothing in the cache says it happened.
	ev := s.adminEvent(r, audit.Event{
		Action:     audit.ActionCachePurge,
		TargetKind: audit.TargetCache,
	})
	if !s.recordAudit(w, r, ev) {
		return
	}
	if err := s.cache.Purge(r.Context()); err != nil {
		s.recordAuditFailure(r, ev, err)
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

// handleSpendScopes reports consumption per organisation, team and project.
//
// It answers the question a per-key report cannot: what a team has spent
// between them. Summing the key rows would give a different number and a wrong
// one — a key can leave a team, and its historical spend does not leave with
// it — so the pool is its own subject in the ledger and is read as one.
func (s *Server) handleSpendScopes(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.writeSpend(w, r, func() ([]spend.Summary, error) { return s.ledger.Scopes(r.Context()) })
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
		"cache_savings_note":  "What the provider's prompt cache took off the bill, against the same tokens charged as ordinary input. Net of the write premium, so it goes negative where caches were written and never read back — which is what a conversation scattered across deployments costs. It is not included in cost, which is what was actually charged.",
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
	discount, premium := 0.0, 0.0
	if pricing.billable {
		cost = pricing.price.Cost(obs.usage)
		savings = pricing.price.CacheSavings(obs.usage)
		// The ledger reports savings as one signed figure, which is the right
		// shape for a report a human reads. Metrics need the two halves apart:
		// their difference is signed, and a counter that can fall is not a
		// counter — every rate() over it would break the moment caching started
		// costing more than it saved, which is exactly when it matters.
		discount, premium = pricing.price.CacheBreakdown(obs.usage)
		s.warnUnpricedCacheTier(obs, pricing.price)
		s.warnUnpricedCacheWrite(obs, pricing.price)
	}
	s.warnMissingStreamUsage(obs)

	if s.ledger != nil && obs.deployment != "" {
		entry := spend.Entry{
			At:           time.Now().UTC(),
			RequestID:    RequestIDFrom(r.Context()),
			KeyHash:      obs.keyHash,
			KeyAlias:     obs.keyAlias,
			ModelGroup:   obs.model,
			DeploymentID: obs.deployment,
			Usage:        obs.usage,
			Cost:         cost,
			Billable:     pricing.billable,
			CacheSavings: savings,
			Scopes:       obs.scopes,
			ScopeAliases: obs.scopeAliases,
		}
		if err := s.ledger.Record(ctx, entry); err != nil {
			s.log.Warn("record spend", "error", err, "request_id", RequestIDFrom(r.Context()))
		}
	}

	s.recordTraffic(r, obs, cost, savings, pricing.billable)
	s.metrics.Observe(obs.toResult(cost, discount, premium))
}

// recordTraffic keeps one completed request for the UI's traffic view.
//
// It sits beside the ledger write rather than anywhere else on the request path
// because this is the only point where the routing decision, the usage and the
// resulting cost are all known at once. Everything it stores is metadata the
// gateway already holds; nothing is read from a body.
func (s *Server) recordTraffic(r *http.Request, obs observation, cost, savings float64, billable bool) {
	if !s.traffic.Enabled() {
		return
	}
	scope := ""
	if len(obs.scopes) > 0 {
		// Innermost first, so the first entry is the project or team the key
		// actually sits in rather than the organisation above it.
		scope = obs.scopes[0]
	}
	s.traffic.Add(reqlog.Record{
		ID:              RequestIDFrom(r.Context()),
		At:              time.Now().UTC(),
		KeyAlias:        obs.keyAlias,
		SpendSubject:    obs.keyHash,
		Scope:           scope,
		ModelGroup:      obs.model,
		Deployment:      obs.deployment,
		Format:          string(obs.format),
		Streaming:       obs.streaming,
		Outcome:         obs.outcome,
		RejectReason:    obs.rejectReason,
		StatusClass:     obs.statusClass,
		Retries:         obs.retries,
		Fallbacks:       obs.fallbacks,
		PromptAffinity:  obs.promptAffinity,
		Usage:           reqlog.UsageOf(obs.usage),
		Cost:            cost,
		Billable:        billable,
		CacheSavings:    savings,
		LatencyMS:       float64(obs.latency.Microseconds()) / 1000,
		TTFTMS:          float64(obs.timeToFirstToken.Microseconds()) / 1000,
		StreamCompleted: obs.streamCompleted,
		ThroughputTPS:   obs.throughput,
		RequestBytes:    obs.requestBytes,
		ResponseBytes:   obs.responseBytes,
	})
}

// recordTimeout bounds accounting so a wedged store cannot pin a handler open
// after the response has already been delivered.
const recordTimeout = 5 * time.Second

// observation is what one request produced, gathered at the point it completes.
type observation struct {
	model, deployment string
	keyHash, keyAlias string
	// scopes are the ledger subjects of the hierarchy this request's key sits
	// in, innermost first, with scopeAliases their labels. Empty for a key
	// outside any hierarchy, which is what makes the pooled accounting cost
	// nothing for a gateway that declares none.
	scopes, scopeAliases []string
	usage                core.Usage
	outcome              string
	rejectReason         string
	retries, fallbacks   int
	latency              time.Duration
	// promptAffinity records whether the request went to the deployment already
	// holding its prompt prefix. Empty when no pin was consulted.
	promptAffinity string
	// format and streaming describe the shape of the request, which is what
	// says whether an upstream reporting no usage at all is expected or a
	// deployment quietly billing nothing.
	format    core.Format
	streaming bool
	// upstreamFormat is the format the serving deployment answered in, which
	// differs from format exactly when the request was translated. It is kept
	// apart from format because the two answer different questions: the
	// caller's format decides what the reply must look like, and the
	// upstream's decides how its usage counters read — and after translation
	// only the upstream's says whether a silent reply is expected.
	upstreamFormat core.Format

	// statusClass is the class of the upstream response the caller received,
	// as "2xx", "4xx", "5xx". Empty where no upstream answered.
	statusClass string
	// requestBytes and responseBytes are body sizes, headers excluded.
	requestBytes, responseBytes int64
	// timeToFirstToken is measured from the caller's request arriving, so it
	// includes the routing and retrying they waited through. streamCompleted
	// says whether the stream then ran to the end; throughput is output tokens
	// per second measured after the first chunk.
	timeToFirstToken time.Duration
	streamCompleted  bool
	throughput       float64
}

// toResult renders an observation for the metrics registry.
//
// discount and premium are the two halves of what prompt caching did to this
// request's bill. They are passed in rather than derived here because pricing
// belongs to the deployment, which record has already looked up.
func (o observation) toResult(cost, discount, premium float64) metrics.Result {
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

		InputTokens:  o.usage.InputTokens,
		OutputTokens: o.usage.OutputTokens,
		PromptTokens: o.usage.PromptTokens(),

		CacheDiscount:     discount,
		CacheWritePremium: premium,

		Streaming:        o.streaming,
		StreamCompleted:  o.streamCompleted,
		TimeToFirstToken: o.timeToFirstToken,
		Throughput:       o.throughput,

		UpstreamStatusClass: o.statusClass,
		RequestBytes:        o.requestBytes,
		ResponseBytes:       o.responseBytes,
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
	if obs.usage.CacheWrite1hTokens == 0 {
		return
	}
	// The rate that fell back is the one belonging to the tier this request was
	// billed at, so a long-context deployment is told to price the long-context
	// block rather than the base one it may already have priced correctly.
	// A deployment whose writes are free has no premium to under-report: both
	// tiers are zero, and the fallback is the correct figure rather than an
	// approximation of one.
	if price.CacheWritesFree {
		return
	}
	key, priced, fallback := "cost.cache_write_1h_per_1m", price.CacheWrite1hPer1M > 0, price.CacheWritePer1M
	if tier := price.TierFor(obs.usage); tier != nil {
		key, priced, fallback = "cost.long_context.cache_write_1h_per_1m", tier.CacheWrite1hPer1M > 0, tier.CacheWritePer1M
	}
	if priced {
		return
	}
	if _, seen := s.mispriced.LoadOrStore(obs.deployment+"\x00"+key, true); seen {
		return
	}
	s.log.Warn("upstream reported one-hour cache writes at a deployment priced only for five-minute ones; cost is understated until the one-hour write price is set",
		"deployment", obs.deployment,
		"model", obs.model,
		"remedy", key,
		"cache_write_1h_tokens", obs.usage.CacheWrite1hTokens,
		"cache_write_per_1m", fallback)
}

// warnUnpricedCacheWrite reports, once per deployment, that an upstream charged
// for a cache write the cost model names no price for.
//
// cost.cache_write_per_1m is optional on an `openai` deployment because most of
// that ecosystem caches automatically and writes for free — OpenAI, Kimi, GLM
// and DeepSeek all do. Alibaba's Qwen and MiniMax do not: both have an explicit
// cache and both charge for the write, reporting it nested inside
// prompt_tokens_details.
//
// Those writes are billed at the input rate where no write price is set, which
// is a defensible approximation and much closer than the zero it would otherwise
// be — but it is still an approximation, and on a provider charging a premium it
// understates every write. Nothing else says so: the request succeeds and only
// the total is wrong.
func (s *Server) warnUnpricedCacheWrite(obs observation, price core.Pricing) {
	if obs.usage.CacheWriteTokens == 0 {
		return
	}
	// Nothing fell back: the operator declared the write price to be zero, so
	// zero is what was charged.
	if price.CacheWritesFree {
		return
	}
	key, priced := "cost.cache_write_per_1m", price.CacheWritePer1M > 0
	if tier := price.TierFor(obs.usage); tier != nil {
		key, priced = "cost.long_context.cache_write_per_1m", tier.CacheWritePer1M > 0
	}
	if priced {
		return
	}
	if _, seen := s.mispriced.LoadOrStore(obs.deployment+"\x00"+key, true); seen {
		return
	}
	s.log.Warn("upstream charged for a cache write at a deployment with no write price; those tokens are billed at the input rate, which understates a provider that charges a premium",
		"deployment", obs.deployment,
		"model", obs.model,
		"remedy", key,
		"cache_write_tokens", obs.usage.CacheWriteTokens,
		"input_per_1m", price.InputPer1M)
}

// warnMissingStreamUsage reports, once per deployment, that a streamed reply
// from an OpenAI-compatible upstream carried no usage at all.
//
// Such a request is recorded as zero of everything: no cost, nothing charged
// against a budget or a token limit, and a prompt cache whose reads cannot be
// seen. observability.stream_usage exists to prevent it, so reaching here means
// either that it is switched off, that the caller set stream_options itself and
// asked for no usage, or that this upstream ignores the field — and none of the
// three announces itself anywhere else.
func (s *Server) warnMissingStreamUsage(obs observation) {
	// The serving deployment's format is what decides this, not the caller's.
	// A translated request arrives in the Anthropic format and is answered by
	// an OpenAI-compatible upstream, and it is that upstream that reports
	// nothing unless asked — so keying on the caller's format would leave the
	// translated half of a fleet billing zero with no warning anywhere.
	upstream := obs.upstreamFormat
	if upstream == "" {
		upstream = obs.format
	}
	if upstream != core.FormatOpenAI || !obs.streaming || obs.deployment == "" {
		return
	}
	if obs.outcome != metrics.OutcomeSuccess || !obs.usage.Empty() {
		return
	}
	if _, seen := s.unmeasured.LoadOrStore(obs.deployment, true); seen {
		return
	}
	s.log.Warn("streamed reply reported no usage, so it is billed as nothing",
		"deployment", obs.deployment,
		"model", obs.model,
		"remedy", "set observability.stream_usage, or have the caller send stream_options.include_usage")
}

// UseSpendHistory attaches the durable spend store.
//
// Wired after construction like UseAudit and for the same reason: whether
// history is kept, and where, is a deployment decision that the process
// assembling the gateway is the only thing that knows.
func (s *Server) UseSpendHistory(h spend.History) {
	if h == nil {
		return
	}
	s.history = h
}

// handleSpendHistory reports consumption bucketed over time. Master-key only,
// like the rest of /spend: it discloses what every team and developer spent.
//
// It is a separate endpoint from /spend/keys rather than a parameter on it
// because the two read different systems and answer different questions.
// /spend/keys reads the enforcing ledger and answers "how close is this key to
// its cap right now"; this reads the history and answers "what did they spend
// in August". A key whose window rolled over this morning is at zero in the
// first and unchanged in the second, and that is not a discrepancy.
func (s *Server) handleSpendHistory(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.writeSpendHistory(w, r)
}

func (s *Server) writeSpendHistory(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		writeError(w, http.StatusNotFound, "not_found_error",
			"spend history is not enabled; set observability.spend_history.dsn to keep one")
		return
	}
	q, err := spendQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	buckets, err := s.history.Series(r.Context(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var totalCost, totalSavings float64
	for _, b := range buckets {
		totalCost += b.Cost
		totalSavings += b.CacheSavings
	}

	// Every kind but one partitions the traffic: a request has one key, one
	// deployment and one model group, so summing those buckets is summing the
	// money once. A request has a *chain* of scopes and is charged to every
	// level of it, so a scope report over every subject returns the same money
	// at each depth — an organisation's total and its teams' totals, added
	// together. That is correct data and a wrong sum, and the difference is
	// invisible in a number, so the response says which one it handed back
	// rather than leaving a reader to notice their spend has doubled.
	overlapping := q.Kind == spend.KindScope && q.Subject == ""
	note := "Buckets are UTC. Each request is counted once here, but a request is also charged to its key, its scopes, its deployment and its model group, so totals from different kinds cover the same money and must not be added together."
	if overlapping {
		note = "Buckets are UTC. This report covers every scope at every level, so an organisation and the teams inside it each appear: the buckets are right and their sum counts the same money once per level. Name a subject to total one scope."
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind":                string(q.Kind),
		"subject":             q.Subject,
		"interval":            string(q.Interval),
		"from":                q.From,
		"to":                  q.To,
		"buckets":             buckets,
		"count":               len(buckets),
		"total_cost":          totalCost,
		"total_cache_savings": totalSavings,
		"overlapping":         overlapping,
		"note":                note,
	})
}

// handleSpendExport streams the per-request rows a query selects as CSV.
//
// CSV rather than JSON because of who asks for it: this is the artefact that
// goes to whoever does chargeback, and it opens in the tool they already use.
// It streams as it reads rather than building a document, because a month of a
// fleet's traffic is millions of rows and materializing them would be a way to
// ask the gateway to exhaust its own memory on request.
func (s *Server) handleSpendExport(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.writeSpendExport(w, r)
}

// writeSpendExport is the export without its authentication, split off the way
// writeSpendHistory is and for the same reason: the console reaches the same
// report through a session cookie rather than the master key, and a second copy
// of the streaming CSV logic would be a second place for the column list to
// drift from the one whoever does chargeback already has a spreadsheet for.
func (s *Server) writeSpendExport(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		writeError(w, http.StatusNotFound, "not_found_error",
			"spend history is not enabled; set observability.spend_history.dsn to keep one")
		return
	}
	q, err := spendQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="spend-%s-%s.csv"`,
		q.From.UTC().Format("20060102"), q.To.UTC().Format("20060102")))

	cw := csv.NewWriter(w)
	// The header is written before the first row rather than after the query
	// succeeds, because the status line has already gone out: an error now
	// truncates the file, and a truncated CSV with a header reads as an
	// interrupted download rather than as a successful empty one.
	if err := cw.Write([]string{
		"at", "request_id", "key_hash", "key_alias", "scope", "model_group", "deployment",
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
		"cost", "billable", "cache_savings",
	}); err != nil {
		s.log.Warn("write spend export", "error", err)
		return
	}
	err = s.history.EachRow(r.Context(), q, func(row spend.Row) error {
		return cw.Write([]string{
			row.At.Format(time.RFC3339), row.RequestID, row.KeyHash, row.KeyAlias,
			row.Scope, row.ModelGroup, row.Deployment,
			strconv.Itoa(row.Usage.InputTokens), strconv.Itoa(row.Usage.OutputTokens),
			strconv.Itoa(row.Usage.CacheReadTokens), strconv.Itoa(row.Usage.CacheWriteTokens),
			strconv.FormatFloat(row.Cost, 'f', -1, 64),
			strconv.FormatBool(row.Billable),
			strconv.FormatFloat(row.CacheSavings, 'f', -1, 64),
		})
	})
	cw.Flush()
	if err == nil {
		err = cw.Error()
	}
	if err != nil {
		// Nothing can be said to the caller: the body is already going out with
		// a 200. The truncated file is the signal, and this line is what says
		// why.
		s.log.Warn("spend export ended early", "error", err, "request_id", RequestIDFrom(r.Context()))
	}
}

// spendQuery reads a history query out of the URL.
//
// Defaults are chosen so a bare call answers something useful: the last thirty
// days by day, over every subject of the kind asked for. `kind` has no default
// because the four dimensions cover the same money from different angles, and
// picking one for the caller would be picking which number they meant.
func spendQuery(r *http.Request) (spend.Query, error) {
	v := r.URL.Query()
	q := spend.Query{
		Kind:     spend.Kind(v.Get("kind")),
		Subject:  v.Get("subject"),
		Interval: spend.Interval(v.Get("interval")),
	}
	if q.Kind == "" {
		q.Kind = spend.KindScope
	}
	if q.Interval == "" {
		q.Interval = spend.IntervalDay
	}

	now := time.Now().UTC()
	q.To = now
	q.From = now.AddDate(0, 0, -30)
	for _, f := range []struct {
		name string
		dst  *time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		raw := v.Get(f.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			// Also accepted as a bare date, because that is what somebody
			// typing a month boundary by hand writes.
			t, err = time.Parse("2006-01-02", raw)
			if err != nil {
				return q, fmt.Errorf("%s: must be RFC 3339 or YYYY-MM-DD, got %q", f.name, raw)
			}
		}
		*f.dst = t.UTC()
	}
	if raw := v.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return q, fmt.Errorf("limit: must be a number, got %q", raw)
		}
		q.Limit = n
	}
	if err := q.Validate(); err != nil {
		return q, err
	}
	return q, nil
}
