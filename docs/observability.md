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

## Spend endpoints

Both are **master-key only** — they disclose what every developer spent.

| Endpoint | Reports |
|---|---|
| `GET /spend/keys` | Consumption per virtual key, with alias. |
| `GET /spend/deployments` | Consumption per deployment. |

Both carry `cache_savings` beside `cost`: what the provider's prompt cache took
off the bill, against the same tokens charged as ordinary input. It sits beside
cost rather than inside it — cost is what was charged, and this is what was not.
See [prompt-caching.md](prompt-caching.md#seeing-whether-it-works).

Revoking a key drops its ledger record and its rate-limit counter.

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
