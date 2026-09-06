# Usage, cost and metrics

The gateway records what each virtual key and deployment consumed, enforces
per-key budgets, and exposes Prometheus metrics.

## What "cost" means here

Cost is attributed **only where the operator actually pays it**.

A `passthrough` deployment relays the caller's own credential, so the upstream
bills their claude.ai subscription rather than your account. Those requests are
recorded as usage — tokens, request counts, latency — with **no cost**, and they
never consume a budget you set. Pricing a passthrough deployment is a
configuration error and is rejected at load, because a figure there would invent
a charge nobody receives.

This distinction is why `Totals` reports `requests` and `billable_requests`
separately: a key serving only subscription traffic is visibly busy but free,
which a bare cost of `0` could not distinguish from an idle key.

`cache_savings`, reported beside cost, is **net of the prompt cache's write
premium** and therefore goes negative where caches were written and never read
back. That is not a fault to filter out: it is the report that caching is
currently costing more than it saves, which is what a conversation scattered
across deployments looks like in money. See
[prompt-caching.md](prompt-caching.md#seeing-whether-it-works).

There is one way a billable request can nonetheless record nothing, and it is
worth knowing before reading a zero as good news: a **streamed**
OpenAI-compatible reply carries no usage unless the request asked for it. The
gateway asks by default — see
[prompt-caching.md](prompt-caching.md#streamed-replies-report-nothing-unless-asked)
— and says so in the log, once per deployment, whenever a streamed reply arrives
without any.

## Pricing

Per million tokens, as providers publish it:

```yaml
model_list:
  - model_name: anthropic-claude
    params:
      auth_mode: api_key
      api_key: ${ANTHROPIC_API_KEY}
    cost:
      input_per_1m: 3.00
      output_per_1m: 15.00
      cache_read_per_1m: 0.30
      cache_write_per_1m: 3.75
```

**The cache figures are not optional detail.** Claude Code leans heavily on
prompt caching, and a cache read costs roughly a tenth of ordinary input while a
cache write costs a premium. A cost model using only input and output would be
wrong by a wide margin on exactly the traffic this gateway exists to carry, so
one that prices input without pricing the cache is
[refused at load](configuration.md#a-partial-cost-model-is-refused-at-load)
rather than quietly serving traffic it cannot cost.

The gateway reads Anthropic's `cache_read_input_tokens` and
`cache_creation_input_tokens` from the response, including from the
`message_start` event on a streamed reply, and the OpenAI-compatible
`prompt_tokens_details.cached_tokens` — which, unlike Anthropic's counters, is
part of the input count it is reported beside. See
[prompt-caching.md](prompt-caching.md#openai-compatible-deployments) for why that
difference is the expensive one to get wrong.

A deployment with no `cost` block at all accrues usage but no cost.

## Budgets

```yaml
virtual_keys:
  keys:
    - key: ${DEV_KEY}
      max_budget: 50.0
      budget_duration: 720h    # omit for a lifetime budget
```

Checked **before** dispatch, so an over-budget key is refused before incurring
further cost. Exceeding it returns **402 Payment Required** rather than 429: the
caller is not going too fast, they are out of budget, and retrying sooner will
not help until the window rolls over.

Only billable traffic counts. The window resets on first use after it elapses.

Budgets survive restarts only when the ledger is persisted:

```yaml
observability:
  spend_store_path: ./data/spend.json
  spend_flush_interval: 30s
```

Without it, every restart hands each key a fresh allowance. The ledger is
flushed on the interval and once more on graceful shutdown, written atomically
with `0600` permissions.

### A shared budget

`keys[].max_budget` is that key's alone. Ten developers issued the same cap can
spend it ten times over, which is the right behaviour for a per-person allowance
and the wrong one for a team's.

A cap several callers share is a **scope** — an organisation, a team or a
project declared under [`rbac`](configuration.md#rbac) — and keys join one by
naming it:

```yaml
rbac:
  organizations:
    - id: acme
      max_budget: 10000
      budget_duration: 720h
      teams:
        - id: platform
          max_budget: 2000
          budget_duration: 720h

virtual_keys:
  keys:
    - key: ${DEV_KEY}
      max_budget: 200        # this developer's own cap
      budget_duration: 720h
      scope: acme/platform   # …inside the team's shared 2000
```

The difference is where the spend lands. A scope is its own subject in the
ledger, so **every key beneath it records against the same entry**: what one
member spends is subtracted from what the others may. One request writes four
entries here — the key, the project, the team and the organisation — and each is
checked, innermost first, before dispatch.

A refusal names the level that bound:

```
402 the shared budget for team acme/platform is exhausted for the current window
```

which is the difference between an actionable message and a misleading one. A
developer told their *own* budget is exhausted will ask for their own cap to be
raised, and that request goes to the wrong person.

The figures stay out of the response: what a team has spent belongs to whoever
owns the team. They are in the log line and in `/spend/scopes`.

The same pooling applies to `rpm_limit` and `tpm_limit` on a scope — one
allowance the members share rather than one each. The key's allowance and every
scope's are reserved in a single all-or-nothing operation, so a request the team
refuses leaves no increment on the key's own window: a caller sitting against a
shared limit does not burn their personal allowance doing nothing.

Depth is free. The whole chain is one read and one reservation however many
levels it has, and a chain declaring no limits anywhere costs no round trip at
all.

## Spend endpoints

All of them are **master-key only** — they disclose what every developer spent.

| Endpoint | Reports |
|---|---|
| `GET /spend/keys` | Consumption per virtual key, with alias. Current budget window. |
| `GET /spend/scopes` | Consumption per organisation, team and project. Current budget window. Empty where no hierarchy is configured. |
| `GET /spend/deployments` | Consumption per deployment. Current budget window. |
| `GET /spend/history` | Consumption bucketed over a date range, from the durable history. |
| `GET /spend/export` | The per-request rows a range selects, as CSV. |

The first three and the last two read different systems, and the difference is
the point rather than an implementation detail. The first three read the ledger
that **enforces** budgets and report the window it is enforcing over, so a key
whose window rolled over this morning reads as zero. The last two read the
**history**, and report what was spent whether or not that window has since
rolled over. Neither is wrong; they answer different questions, and a gateway
with no history configured can only answer the first.

`/spend/scopes` is read rather than derived. Summing the `/spend/keys` rows for
a team gives a different number and a wrong one: a key can leave a team, and its
historical spend does not leave with it — so a pool is its own subject rather
than a query over its current members.

Both carry `cache_savings` beside `cost`: what the provider's prompt cache took
off the bill, against the same tokens charged as ordinary input. It sits beside
cost rather than inside it — cost is what was charged, and this is what was not.
See [prompt-caching.md](prompt-caching.md#seeing-whether-it-works).

Revoking a key drops its ledger record and its rate-limit counter. It does
**not** drop the key's history: that record is what a chargeback is built from,
and erasing it would make revoking a key a way to erase a month of somebody's
costs.

## Spend history

```yaml
observability:
  spend_history:
    dsn: ${GATEWAY_SPEND_DSN}
```

Off unless a dsn is set. With one, every completed request also becomes a row in
`spend_requests` and is added to a day rollup in `spend_daily`, and the gateway
can answer what a team spent last month, chart a trend, and export the rows
behind a figure.

### Why this is not the ledger

The ledger answers *may this request proceed*: on the inference path, for every
request, across a fleet. That is a current-window number, it wants to be one
fast shared read, and Redis is the right shape for it. History answers *what did
the Payments team spend in August*: off the request path, for a person, as a
range scan and an aggregation — which is the shape Redis is worst at and a
relational database is best at.

Serving both from one store makes each worse. A Postgres ledger would put an
`INSERT` and a `SUM` on every inference request; a Redis history would need a
key per bucket per subject and still could not answer a range query without
scanning them. So the two run side by side and every entry is recorded to both.

### What it costs

**Writes are buffered and batched.** Entries go onto a channel, one goroutine
drains it, and each batch is one transaction that copies the rows in and upserts
the day rollups they belong to. Accounting runs inline on the request path, so a
synchronous `INSERT` per request would put a database round trip between a
response being relayed and the handler returning.

**A full buffer drops rows and counts them.** That is the deliberate end of the
trade: a row here is a report, and blocking until the database caught up would
make an inference request wait on the system that answers monthly questions. No
budget is escaped by a dropped row — enforcement never reads this table.
`gateway_spend_history_dropped_total` is how that becomes visible, and it is
worth an alert: it is the difference between a chargeback that adds up and one
that quietly does not. `gateway_spend_history_pending` is the warning that comes
before it.

**A failed write is retried, not discarded.** Batches that could not commit are
carried forward and written with the next flush, so a database restart costs
latency rather than rows. Five batches deep, the oldest is dropped and counted.

**A database that cannot be reached at startup refuses to start**, like the key
store and the audit chain. A gateway that came up regardless would serve traffic
whose cost nothing was recording, which is discovered a month later, when the
report is asked for.

### Rollups, and why they are written with the rows

`spend_daily` holds one row per subject per UTC day, maintained **in the same
transaction as the rows it summarizes** rather than by a scheduled job. A job
would be a second thing to run and to alert on, would leave today's figures
missing until it ran, and would have to be idempotent against rows it might see
twice. Doing it in the write transaction makes double counting impossible rather
than merely unlikely: either both halves commit or neither does.

It also means rollups outlive the rows, which is what makes `retention` safe to
set — a pruned month still has its totals.

### Reading it

```bash
# what each team spent last month, by day
curl -s "localhost:4000/spend/history?kind=scope&from=2026-08-01&to=2026-09-01" \
  -H "x-gateway-key: $GATEWAY_MASTER_KEY"

# one team, by hour, to find a spike
curl -s "localhost:4000/spend/history?kind=scope&subject=acme/payments&interval=hour&from=2026-08-14&to=2026-08-15" \
  -H "x-gateway-key: $GATEWAY_MASTER_KEY"

# the rows behind the figure, for whoever does chargeback
curl -s "localhost:4000/spend/export?kind=scope&subject=acme/payments&from=2026-08-01&to=2026-09-01" \
  -H "x-gateway-key: $GATEWAY_MASTER_KEY" -o august.csv
```

| Parameter | Meaning |
|---|---|
| `kind` | `key`, `scope`, `deployment` or `model`. Defaults to `scope`. |
| `subject` | One key hash, scope id, deployment id or model group. Omitted, every subject of that kind. |
| `from`, `to` | RFC 3339 or `YYYY-MM-DD`. `from` is inclusive and `to` exclusive, so consecutive months tile without counting a boundary request twice. Default: the last thirty days. |
| `interval` | `day` (from the rollups) or `hour` (aggregated from the rows, so bounded by their retention). |
| `limit` | Caps rows or buckets returned. Exports default to 10,000. |

Two things to know before adding a total up.

**Buckets are UTC days.** A fleet spans time zones, and a rollup keyed on the
operator's local day would move under a gateway deployed in another region. A
finance team that needs local months converts at the edge.

**A scope report over every subject returns the same money more than once.** A
request is charged to its key, to *every scope above it*, to its deployment and
to its model group — the same deliberate duplication `/spend/scopes` makes, and
for the same reason. So an organisation and the teams inside it each appear, and
adding those buckets together counts the money once per level. The response says
`"overlapping": true` when that is what it handed back. `key`, `deployment` and
`model` each count a request exactly once, as does `scope` with a named subject.

### Retention

```yaml
observability:
  spend_history:
    retention: 2160h    # 90 days of per-request rows
```

Zero, the default, keeps every row: deleting a financial record should be
something an operator asked for rather than something that happens quietly. When
set, rows older than the window are dropped hourly and **the day rollups are
kept**, so a pruned month still reports its totals and only its per-request
export is gone.

The arithmetic worth doing first: a few hundred engineers running an agent all
day produce on the order of 100,000 rows a day, or tens of millions a year at a
few hundred bytes each. That is unremarkable for Postgres and not nothing, and
it is the number that should decide the setting.

## Metrics

```yaml
observability:
  metrics: true
```

Serves `GET /metrics` in the Prometheus text exposition format, written by hand
rather than pulling in a client library — the metric set is fixed and known at
compile time, and the project's dependencies are a YAML parser and a Redis
client.

### Traffic

| Metric | Type | Labels |
|---|---|---|
| `gateway_requests_total` | counter | model, deployment, outcome |
| `gateway_request_duration_seconds` | histogram | model, deployment |
| `gateway_upstream_responses_total` | counter | model, deployment, status (`2xx`…`5xx`) |
| `gateway_in_flight` | gauge | model, deployment |
| `gateway_request_body_bytes` | histogram | model |
| `gateway_response_body_bytes` | histogram | model |

`gateway_upstream_responses_total` counts **every attempt**, not only the
response the caller received, so a group that is quietly failing over on half
its requests is visible before anyone complains. The status is reduced to its
class deliberately: a provider is free to invent a code, and an unbounded label
value is an unbounded metric.

### Tokens and cost

| Metric | Type | Labels |
|---|---|---|
| `gateway_tokens_total` | counter | model, deployment |
| `gateway_input_tokens_total` | counter | model, deployment |
| `gateway_output_tokens_total` | counter | model, deployment |
| `gateway_prompt_tokens` | histogram | model, deployment |
| `gateway_completion_tokens` | histogram | model, deployment |
| `gateway_cost_total` | counter | model, deployment |
| `gateway_prompt_cache_discount_total` | counter | model, deployment |
| `gateway_prompt_cache_write_premium_total` | counter | model, deployment |

Input and output are separate counters because they answer different questions:
output is what a model generates and what dominates a bill, input is what a
prompt cache acts on. Summed into one total, neither is visible.

`gateway_prompt_tokens` is a **distribution**, not a total, and it counts cached
input too — it is the figure a context window is measured against. A mean prompt
size hides exactly the long-context requests that dominate a bill, which is what
the histogram exists to surface.

Net cache savings are **signed**: caching costs more than it saves whenever
prefixes are written and never read back. A counter that can fall is not a
counter, so the two halves are published apart and both only ever rise. Net
savings is one subtraction:

```promql
rate(gateway_prompt_cache_discount_total[5m])
  - rate(gateway_prompt_cache_write_premium_total[5m])
```

A negative result is caching costing more than it saves — a conversation
scattered across deployments. See
[prompt-caching.md](prompt-caching.md#seeing-whether-it-works).

### The provider's prompt cache

| Metric | Type | Labels |
|---|---|---|
| `gateway_prompt_cache_tokens_total` | counter | model, deployment, outcome (`read`, `write`, `write_1h`) |
| `gateway_prompt_cache_requests_total` | counter | model, deployment, outcome (`hit`, `miss`) |
| `gateway_prompt_affinity_total` | counter | model, deployment, outcome (`hit`, `miss`, `new`) |

`write_1h` is the long-TTL **subset** of `write`, not an addition to it. It is
reported separately because it is priced separately: twice base input against
the five-minute tier's 1.25x, so a shift in the mix moves the bill without
moving the token count.

Affinity's `new` is kept out of `miss` on purpose. It lets a miss mean "routing
passed over a warm upstream" rather than also counting every conversation's
opening turn.

### Streaming

| Metric | Type | Labels |
|---|---|---|
| `gateway_time_to_first_token_seconds` | histogram | model, deployment |
| `gateway_output_tokens_per_second` | histogram | model, deployment |
| `gateway_streams_total` | counter | model, deployment, outcome (`completed`, `interrupted`) |

These are the numbers a user actually feels, and total duration describes none
of them: a fast first token followed by a long generation and a slow first token
followed by a short one produce the same total. Time to first token is measured
from the caller's request arriving, so it includes the routing and retrying they
waited through. Throughput is measured *after* the first chunk, so it describes
generation rather than queueing — the two move independently, and averaging them
together hides both.

`gateway_streams_total` exists because an interrupted stream is a **200 with a
truncated body**. The request counter, the status class and the latency
histogram all record a perfectly ordinary success; only this says otherwise.

### Routing and admission

| Metric | Type | Labels |
|---|---|---|
| `gateway_retries_total` | counter | model, deployment |
| `gateway_fallbacks_total` | counter | model |
| `gateway_cooldowns_total` | counter | model, deployment |
| `gateway_deployment_available` | gauge | model, deployment |
| `gateway_deployment_throttled_total` | counter | model |
| `gateway_rejections_total` | counter | model, outcome (reason) |

`gateway_cooldowns_total` counts ejections; `gateway_deployment_available` says
whether the outage is still going on. Note that **connection errors and
timeouts do not eject** a deployment — see
[routing.md](routing.md#cooldowns) for why, and for what that costs: a
permanently unreachable deployment stays in the rotation, and every request to
the group keeps paying a wasted attempt.

`gateway_deployment_throttled_total` is a deployment at *its own* rpm or tpm
limit, which is a capacity signal. A caller's key hitting *its* limit is a
rejection, which is the caller's problem rather than the fleet's.

### Response cache and process

| Metric | Type | Labels |
|---|---|---|
| `gateway_response_cache_requests_total` | counter | model, outcome (`hit`, `miss`) |
| `gateway_audit_write_failures_total` | counter | action |
| `gateway_shared_state_degradations_total` | gauge | — |
| `gateway_virtual_keys` | gauge | — |
| `gateway_build_info` | gauge | version, strategy |
| `gateway_uptime_seconds` | gauge | — |

Cache lookups are counted at the lookup rather than inferred from the request
outcome, because a miss has no distinguishing outcome of its own and the hit
rate would have no denominator.

Virtual keys are counted but never **labelled** individually. A runtime-minted
key is unbounded in number, and one series per key would be an unbounded metric.
Per-key spend lives on `/spend/keys`, which is master-key only for the same
reason it is not on a public scrape endpoint.

`gateway_build_info` is always 1; the labels are the point, and a change in them
is a deploy.

`gateway_audit_write_failures_total` counts administrative actions refused
because the audit log would not take the record — a key that could not be
minted, a console sign-in that could not be granted. It is worth an alert rather
than a dashboard panel: from the outside those refusals look like any other 500,
and a non-zero value means the gateway currently cannot be administered at all.
See [audit.md](audit.md#failure-posture).

### Outcomes and reasons

`outcome` on `gateway_requests_total` is `success`, `upstream_error`,
`gateway_error`, `rejected` or `cache_hit`. Rejections carry the reason instead:
`unauthenticated`, `model_unknown`, `model_forbidden`, `budget_exceeded`,
`rate_limited`, `body_too_large`, `malformed_body`, `missing_model`.

### Access

`/metrics` is unauthenticated, like the liveness probe, because scrapers
generally cannot carry a credential. Unlike `/health` it exposes no hostnames or
auth modes — only counters labelled by model group and deployment ID. If even
those labels are sensitive, bind the gateway on a private interface or put the
endpoint behind your own ingress.

No credential ever appears in a scrape; a test asserts it.

## OpenTelemetry

```yaml
observability:
  otlp:
    endpoint: http://localhost:4318
```

Pushes the metrics above to an OpenTelemetry collector over OTLP/HTTP.
Independent of `metrics`: either, both or neither may be on. Both render the
same snapshot, so they cannot disagree about a number.

**Why both exist.** OTLP is a push where Prometheus is a pull, and that is the
whole reason to use it. A gateway that cannot be scraped — behind NAT, in a
serverless runtime, one instance of an autoscaled group whose short-lived
members are gone before the next scrape — has no way to publish a pull-based
metric at all.

### Configuration

| Key | Default |
|---|---|
| `endpoint` | — (also `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_ENDPOINT`) |
| `protocol` | `http/protobuf`, or `http/json` |
| `interval` | `60s` |
| `timeout` | `10s`, must be below `interval` |
| `compress` | `true` |
| `service_name` | `ai-gateway` (also `OTEL_SERVICE_NAME`) |
| `headers` | — (merged over `OTEL_EXPORTER_OTLP_HEADERS`) |
| `resource_attributes` | — (merged over `OTEL_RESOURCE_ATTRIBUTES`) |

The standard `OTEL_*` variables are read wherever the config file is silent.
That matters more than it looks: those variables are how a collector sidecar or
a hosted vendor configures every other component in a fleet, and a gateway that
read only its own YAML would be the one process needing to be told separately —
and would silently export nothing in an environment where everything else
worked. The config file wins wherever it speaks, because it is the more specific
statement.

The exporter is written directly rather than through the OpenTelemetry SDK,
which would pull in gRPC and the protobuf runtime for a process whose entire
published surface is a few dozen fixed metric families. What the SDK buys —
instrumentation libraries, dynamic views — this gateway does not use.

### What arrives

Counters become **monotonic cumulative Sums**, gauges become Gauges, histograms
become Histograms. Cumulative rather than delta because that is what the
registry holds: counters run from process start and are never reset, so every
export carries the same start timestamp and a running total, and a dropped
export is made good by the next one rather than permanently lost.

Every metric carries a UCUM unit — `s`, `By`, `{token}`, `USD` — which OTLP
consumers use to label axes. Resource attributes (`service.name`,
`service.version`, plus anything configured) are sent once per export rather
than repeated on every data point.

A final export runs **on shutdown**. Without it everything since the last tick
is lost, which for a short-lived process — a job, a canary, a container that
failed its first health check — is the whole of its telemetry.

### When the collector is down

Exports fail and the gateway keeps serving. `/metrics` is unaffected, so a
Prometheus scrape still sees everything. The failure is logged on the first
occurrence and then sparsely, so an hour-long collector outage does not write
one error line per interval for an hour.

Only the **metrics** signal is implemented. Traces and logs would each be
another schema and another exporter, and the gateway already emits structured
logs carrying a request ID that a collector can correlate on.

## One instrumentation point

Spend and metrics are recorded together at a single place in the request path,
because both want the same facts: which key, which deployment, what usage, what
outcome, how long. Measuring twice would let the two drift apart.

## Recent requests

Metrics and the spend ledger both aggregate, and most questions an operator asks
about a gateway are about one request: why did this call fall back, did that
conversation hit the prompt cache it warmed, which deployment served the request
that took nine seconds.

The admin console keeps the last `ui.request_log_size` completed requests in
memory and answers those. It holds metadata only — never a request or response
body — and lives in the process that served the traffic, so it is a debugging
view rather than an archive. Anything that must outlive the process belongs in
the access log. See [admin-ui.md](admin-ui.md#the-traffic-buffer).

None of this describes what was done *to* the gateway. Minting a key, blocking
one, signing in to the console: those are recorded separately, in a hash chain
that makes an edited or missing record detectable. See
**[audit.md](audit.md)**.

## Running more than one instance

By default every replica keeps its own counters, so a `rpm: 100` limit across
three replicas admits up to 300 requests a minute, a key with `rpm_limit: 60`
gets 180, and a key with a $50 budget can spend $50 on each instance. Point them
at one Redis and the limits become what they say:

```yaml
redis:
  addr: localhost:6379
  key_prefix: ai-gateway      # namespaces this gateway; several can share one Redis
  timeout: 250ms
```

`deploy/docker-compose.yml` runs two instances against one Redis, which is the
smallest arrangement in which any of the rows below can be observed to differ:

```bash
docker compose -f deploy/docker-compose.yml up --build   # gateways on :4000 and :4001
```

### What is shared, and what is not

| State | Where | Why |
|---|---|---|
| Deployment rate limits (`model_list[].rpm`/`tpm`) | Redis | A per-process limit is silently multiplied by the replica count. |
| Key rate limits (`keys[].rpm_limit`/`tpm_limit`) | Redis | The same arithmetic, applied to the caller's allowance rather than the upstream's capacity. Counted in a separate keyspace from the deployment limits, so a key hash and a deployment id can never draw on one window. |
| Cooldowns | Redis | An upstream ejected by one instance should be ejected everywhere. |
| Spend and budgets | Redis | Otherwise a key spends its whole allowance once per instance. |
| Scope pools (`rbac` budgets and rate limits) | Redis | The same arithmetic again, and worse here: a pool exists precisely to be one cap several callers share, so multiplying it by the replica count defeats the whole feature. Kept in a separate keyspace from the key subjects, so a scope named after a key hash cannot draw on that key's window. |
| Prompt-prefix pins | Redis | Behind a load balancer the next turn of a conversation arrives at a different replica; a per-instance pin would send it to a different upstream, which is the thing the pin exists to prevent. |
| Latency samples | **local** | Latency measures *this instance's* network path to the upstream. Blending measurements from different network positions makes the signal worse, not better. |
| In-flight counts | **local** | It describes the load this instance is carrying, and a shared counter would leak permanently whenever an instance died mid-request. |

### Atomicity

Rate-limit reservation, token accumulation, failure counting and spend recording
each run as a Lua script rather than a sequence of commands. `INCR` followed by
`EXPIRE` is two round trips, and an instance dying in between leaves a counter
with no TTL — a deployment permanently at its limit, recoverable only by hand.

### When Redis is down

The gateway **degrades to per-instance state and keeps serving**. Refusing
requests because a dependency blipped is worse than briefly enforcing limits per
replica.

Degradation is never silent:

- an error log on the first failure, and an info log on recovery
- a counter, surfaced as `shared_state_degradations` on `/health/readiness`
- `shared_state: degraded` on the same endpoint

Readiness still reports **ready** while degraded: the instance serves correctly,
just with local limits, and pulling it from the load balancer would deepen the
outage. An unreachable Redis at startup is logged, not fatal.

The local ledger is kept current even when Redis is healthy, so a fallback has
something to fall back to and `/spend` keeps answering.

## Response caching

Off by default. When enabled, an identical request is served from a stored
response instead of calling the provider.

```yaml
cache:
  enabled: true
  ttl: 5m
  scope: key              # key | shared
  max_entries: 1000
  max_entry_bytes: 1048576
  shared: false           # store in Redis so a hit on one instance serves all
```

### Who may see a cached response

This is the decision that matters, and the default is deliberate.

| Scope | Behaviour | When |
|---|---|---|
| `key` (default) | A virtual key only ever sees its own responses. | Any setup with more than one tenant. |
| `shared` | Every caller reuses any cached response. | Only when all callers are equally trusted. |

Prompts and completions are the most sensitive thing this gateway handles, so
sharing them across tenants is opt-in rather than something reached by omission.
The key hash is part of the cache key under `key` scope, so isolation is
structural rather than a filter that could be bypassed.

**`shared` is refused while any passthrough deployment is configured**, and
validation fails at load rather than leaving it as a footgun: a passthrough
response was generated under one person's own claude.ai subscription, and
serving it to a different key would hand them output someone else paid for.

### What is and is not cached

- Only successful responses. An error describes one moment — a rate limit, an
  overloaded upstream — and replaying it for the whole TTL would turn a blip
  into a sticky outage.
- Only complete streams. A response truncated mid-relay is discarded rather
  than replayed to every later caller.
- Nothing larger than `max_entry_bytes`, so one outsized completion cannot
  evict everything else.

Streaming works: the relayed bytes are stored and replayed through the same
path, so the client parses an identical event sequence. The original timing is
not reproduced — the point is that it arrives at once.

### Cost

A cache hit calls no upstream and costs nothing, so it records **no spend** and
does not consume a rate limit or budget. It appears in metrics with
`outcome="cache_hit"`. Charging for it would be billing twice for one answer.

Responses carry `x-gateway-cache: hit` or `miss`.

`POST /cache/purge` empties the cache; master-key only, since it affects every
caller.

## The provider's prompt cache

Separate from all of the above, and covered in
**[prompt-caching.md](prompt-caching.md)**: Anthropic's own cache of a request's
leading tokens, which load balancing quietly destroys unless conversations are
pinned to the upstream holding their prefix. The symptom is a bill, not an
error, so the metrics above exist to make it visible first.
