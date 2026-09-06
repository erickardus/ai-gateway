# Configuration reference

Every key the parser accepts. Unknown keys are **rejected**, so a typo fails at
startup rather than being silently ignored.

`${VAR}` and `${VAR:-default}` are expanded from the environment at load time. A
reference with no value and no default expands to empty, which validation then
reports as a missing required field rather than passing an empty credential
upstream.

Validation reports **every** problem at once, each naming the field at fault.

## `server`

| Key | Default | Notes |
|---|---|---|
| `addr` | `0.0.0.0:4000` | Listen address. |
| `read_header_timeout` | `30s` | |
| `idle_timeout` | `120s` | |
| `shutdown_grace` | `30s` | In-flight requests drain within this. |
| `max_body_bytes` | `33554432` | Larger bodies get 413. |

There is deliberately **no write timeout**: a write deadline covers the whole
response, so any value large enough for a long streaming completion protects
nothing, and any smaller value severs streams.

## `model_list`

One entry per deployment. Entries sharing a `model_name` form a model group that
the router load-balances across. A group must be format-homogeneous.

| Key | Default | Notes |
|---|---|---|
| `model_name` | required | The public name callers request. |
| `weight` | `1` | Selection share. An explicit `0` drains the deployment. |
| `rpm`, `tpm` | unlimited | Enforced limits, not weights. |
| `params.format` | `anthropic` | `anthropic` or `openai`. |
| `params.api_base` | required | Must be http/https, and must not embed userinfo. |
| `params.model` | `model_name` | Upstream identifier. Equal to `model_name` means the body is forwarded byte for byte. |
| `params.auth_mode` | `api_key` | `api_key` or `passthrough`. |
| `params.auth_header` | `x-api-key` / `authorization` | Where the credential goes, in `api_key` mode. |
| `params.auth_scheme` | — | e.g. `Bearer`. |
| `params.api_key` | required for `api_key` | Must be **empty** for `passthrough`. |
| `cost.input_per_1m` | — | Per million tokens. |
| `cost.output_per_1m` | — | |
| `cost.cache_read_per_1m` | — | Required once `input_per_1m` is set. Not optional detail: Claude Code leans on prompt caching. |
| `cost.cache_write_per_1m` | — | Required on an `anthropic` deployment once `input_per_1m` is set. |
| `cost.cache_write_1h_per_1m` | `cache_write_per_1m` | Anthropic's one-hour cache write, priced at 2x base input against the five-minute tier's 1.25x. |

A `passthrough` deployment must have its host listed in
`virtual_keys.allowed_upstream_hosts`, must not carry an `api_key`, and must not
be priced — the upstream bills the caller's own subscription.

### A partial cost model is refused at load

A deployment with no `cost` block accrues usage and no cost, which is a coherent
state. A deployment priced for input and output but not for its cache is not:
it charges every cached token at the full input rate, reports savings of exactly
zero on the traffic prompt caching exists for, and says nothing about either
until the provider's invoice disagrees.

So the combination fails validation rather than serving traffic:

| Rule | Why |
|---|---|
| `cache_read_per_1m` required once `input_per_1m` is set | zero prices cache reads as free, overstating savings and understating cost by the whole of the cache traffic |
| `cache_read_per_1m` below `input_per_1m` | a read costing as much as fresh input means caching saves nothing, which no provider charges |
| `cache_write_per_1m` required and above `input_per_1m`, on `anthropic` | a write is charged at a premium; pricing it at or below input makes a cache miss look free |
| `cache_write_1h_per_1m`, if set, at least `cache_write_per_1m` | the longer-lived cache is the more expensive one to write |

An `openai` deployment needs no write price: OpenAI-compatible providers cache
automatically and charge nothing to write.

## `router`

| Key | Default | Notes |
|---|---|---|
| `strategy` | `weighted-shuffle` | Also `least-busy`, `usage-based`, `latency-based`. |
| `num_retries` | `2` | Total attempts is `1 + num_retries`. An explicit `0` means never retry. |
| `timeout` | `600s` | Whole exchange, non-streaming. |
| `stream_timeout` | `60s` | Time to response headers only. |
| `cooldown.allowed_fails` | `3` | An explicit `0` ejects on the first failure. |
| `cooldown.period` | `30s` | |
| `backoff.initial` | `500ms` | Applies only when nothing untried remains. |
| `backoff.max` | `8s` | |
| `backoff.jitter` | `0.75` | An explicit `0` makes backoff deterministic. |
| `max_fallback_hops` | `5` | |
| `lowest_latency_buffer` | `0` | Widens the latency-based candidate band. |
| `fallbacks` | — | `[{from: a, to: [b, c]}]` |
| `context_window_fallbacks` | — | Used when the upstream reports a context overflow. |
| `content_policy_fallbacks` | — | Used on a content refusal. |

`num_retries`, `weight`, `backoff.jitter` and `cooldown.allowed_fails` are
pointers internally, so an explicit `0` is never mistaken for an omitted field.

Fallbacks may not target the source group, a group that does not exist, or a
group of a different wire format.

## `virtual_keys`

| Key | Default | Notes |
|---|---|---|
| `master_key` | — | Enables `/key/*`, `/spend/*`, `/cache/purge`. Absent disables them. |
| `header_names` | `[x-gateway-key, x-litellm-api-key]` | Headers that may carry a key, in precedence order. |
| `allowed_upstream_hosts` | — | Where a passthrough deployment may relay a credential. |
| `store.kind` | `memory` | `memory` or `file`. |
| `store.path` | required for `file` | Written atomically at `0600`, hashes only. |
| `keys[].key` | required | Hashed at load; the plaintext is not retained. |
| `keys[].alias` | — | Appears in logs and spend reports. |
| `keys[].models` | any | Exact names or a trailing `*`. |
| `keys[].rpm_limit`, `tpm_limit` | unlimited | |
| `keys[].allow_passthrough` | `false` | Required to reach a passthrough deployment. |
| `keys[].max_budget` | unlimited | Only billable traffic counts. |
| `keys[].budget_duration` | lifetime | Window the budget applies over. |
| `keys[].blocked`, `expires_at` | — | |

## `redis`

Absent means every replica keeps its own counters. See
[observability.md](observability.md#running-more-than-one-instance).

| Key | Default |
|---|---|
| `addr` | — (enables sharing when set) |
| `username`, `password`, `db` | — |
| `key_prefix` | `ai-gateway` |
| `timeout` | `250ms` |

## `cache`

Off by default. See [observability.md](observability.md#response-caching).

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | |
| `ttl` | `5m` | |
| `scope` | `key` | `key` isolates per caller; `shared` does not. |
| `max_entries` | `1000` | |
| `max_entry_bytes` | `1048576` | |
| `shared` | `false` | Store in Redis. Requires `redis.addr`. |

`scope: shared` is **refused at load** while any passthrough deployment exists.

## `prompt_cache`

The **provider's** cache of a request's leading tokens, as distinct from
`cache`, which stores whole responses here. See
[prompt-caching.md](prompt-caching.md).

| Key | Default | Notes |
|---|---|---|
| `affinity` | `true` | Pin requests sharing a cacheable prefix to the deployment that last served one. A preference, never a constraint. Inert in a group with one deployment. |
| `affinity_ttl` | `5m` | How long a pin survives without use. Matches the lifetime of an ephemeral prompt-cache entry. |
| `affinity_max_in_flight_lead` | `4` | A pin is passed over once the pinned deployment carries this many more in-flight requests than the idlest deployment that could serve it instead. `0` yields to any idler peer. |
| `inject` | `false` | Place cache breakpoints on the tools and system prompt of an Anthropic request that carries none of its own. |
| `inject_min_bytes` | `4096` | Prefixes smaller than this are left unmarked; a provider would ignore the breakpoint anyway. |

`inject: true` is **refused at load** while any passthrough deployment exists,
and while any deployment speaks a format other than `anthropic`. Injection edits
the request body, and passthrough exists to forward one unchanged.

## `observability`

| Key | Default | Notes |
|---|---|---|
| `log_level` | `info` | |
| `log_format` | `text` | `text` or `json`. |
| `metrics` | `false` | Serves `GET /metrics`. |
| `spend_store_path` | — | Persists budgets across restarts. |
| `spend_flush_interval` | `30s` | |
| `stream_usage` | `true` | Ask OpenAI-compatible upstreams to report usage on streamed replies. See [prompt-caching.md](prompt-caching.md#streamed-replies-report-nothing-unless-asked). |

`stream_usage` adds `stream_options.include_usage` to a streamed request that did
not set `stream_options` itself. Without it such a reply carries no usage at all
and the request is billed as zero — no cost, no budget, no cached tokens. The
cost of asking is one extra chunk at the end of the stream; turn it off for a
client or an upstream that cannot take one.

## Endpoints

| Endpoint | Auth |
|---|---|
| `POST /v1/messages`, `/v1/messages/count_tokens` | virtual key |
| `POST /v1/chat/completions` | virtual key |
| `GET /v1/models` | virtual key |
| `GET /health` | virtual key — it discloses upstream hosts |
| `GET /health/liveliness`, `/health/readiness` | none |
| `HEAD /api/hello` | none |
| `GET /metrics` | none |
| `POST /key/generate`, `GET /key/info`, `GET /key/list`, `POST /key/delete` | master key |
| `GET /spend/keys`, `GET /spend/deployments` | master key |
| `POST /cache/purge` | master key |

## Error responses

| Condition | Status |
|---|---|
| Missing or unknown key | 401 |
| Blocked or expired key | 403 |
| Model not allowed, passthrough not permitted, host not allowlisted | 403 |
| Unknown model | 404 |
| Body too large | 413 |
| Rate limit exceeded | 429 |
| Budget exhausted | **402** — retrying sooner will not help |
| No healthy deployment | 503 |

Upstream errors are relayed **verbatim**, status and body, because Claude Code
matches on the upstream's own wording to decide whether to retry with a
capability disabled.

## Response headers

| Header | Meaning |
|---|---|
| `x-gateway-request-id` | Correlates with the access log. |
| `x-gateway-model-id` | Model group that served the request. |
| `x-gateway-deployment` | Deployment that served it. |
| `x-gateway-attempted-retries` | Retries used within the final group. |
| `x-gateway-attempted-fallbacks` | Fallback hops taken. |
| `x-gateway-cache` | `hit` or `miss`, when response caching is on. |
| `x-gateway-prompt-affinity` | `hit`, `miss`, or `new`, when a prompt-prefix pin was consulted. Absent otherwise. |

## Per-request overrides

| Header | Effect |
|---|---|
| `x-litellm-num-retries` | Override `num_retries` (0–10). |
| `x-litellm-timeout` | Request timeout, seconds. Capped at 30 minutes. |
| `x-litellm-stream-timeout` | Time-to-first-chunk timeout. Independent of the above. |
| `x-litellm-tags` | Comma-separated labels for attribution. |

A `"disable_fallbacks": true` body field skips fallbacks. It is stripped before
the body is forwarded, since an upstream rejecting unknown fields would 400 it.
