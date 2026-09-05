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
wrong by a wide margin on exactly the traffic this gateway exists to carry. The
gateway reads Anthropic's `cache_read_input_tokens` and
`cache_creation_input_tokens` from the response, including from the
`message_start` event on a streamed reply.

A deployment with no `cost` block accrues usage but no cost.

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

Revoking a key drops its ledger record and its rate-limit counter.

## Metrics

```yaml
observability:
  metrics: true
```

Serves `GET /metrics` in the Prometheus text exposition format, written by hand
rather than pulling in a client library — the metric set is small and fixed, and
the project's only dependency is a YAML parser.

| Metric | Type | Labels |
|---|---|---|
| `gateway_requests_total` | counter | model, deployment, outcome |
| `gateway_tokens_total` | counter | model, deployment |
| `gateway_cost_total` | counter | model, deployment |
| `gateway_retries_total` | counter | model, deployment |
| `gateway_fallbacks_total` | counter | model |
| `gateway_cooldowns_total` | counter | model, deployment |
| `gateway_rejections_total` | counter | model, outcome (reason) |
| `gateway_in_flight` | gauge | model, deployment |
| `gateway_request_duration_seconds` | histogram | model, deployment |
| `gateway_uptime_seconds` | gauge | — |

`outcome` is `success`, `upstream_error`, `gateway_error` or `rejected`.
Rejections carry the reason instead: `unauthenticated`, `model_unknown`,
`model_forbidden`, `budget_exceeded`, `rate_limited`, `body_too_large`,
`malformed_body`, `missing_model`.

`/metrics` is unauthenticated, like the liveness probe, because scrapers
generally cannot carry a credential. Unlike `/health` it exposes no hostnames or
auth modes — only counters labelled by model group and deployment ID. If even
those labels are sensitive, bind the gateway on a private interface or put the
endpoint behind your own ingress.

No credential ever appears in a scrape; a test asserts it.

## One instrumentation point

Spend and metrics are recorded together at a single place in the request path,
because both want the same facts: which key, which deployment, what usage, what
outcome, how long. Measuring twice would let the two drift apart.
