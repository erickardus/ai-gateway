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
| `cost.cache_write_per_1m` | `input_per_1m` | Required on an `anthropic` deployment once `input_per_1m` is set. Optional on an `openai` one, where most providers write for free — but set it for Qwen or MiniMax, which charge. |
| `params.supports_cache_control` | `false` | Declares that an `openai` upstream reads Anthropic's `cache_control` marker, which Qwen's explicit cache does and most of the ecosystem does not. Refused on an `anthropic` deployment, which always reads it, and on a passthrough one, which is never annotated. |
| `cost.cache_write_1h_per_1m` | `cache_write_per_1m` | Anthropic's one-hour cache write, priced at 2x base input against the five-minute tier's 1.25x. |
| `cost.long_context` | — | The higher rates a provider charges above a prompt size. Optional; see below. |

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

An `openai` deployment usually needs no write price: OpenAI, Kimi, GLM and
DeepSeek all cache automatically and charge nothing to write. Alibaba's Qwen and
MiniMax do charge, so a deployment pointing at either wants
`cache_write_per_1m`. Left unset it falls back to `input_per_1m` rather than to
nothing — a free cache write does not exist, and an omitted price means the
provider names no separate one — and the first response reporting a charged
write on an unpriced deployment says so in the log.

### The long-context tier

A provider may charge more for every token of a request once its prompt crosses
a size — Anthropic's threshold is 200k tokens, above which input, output, cache
reads and cache writes all cost more. That is the largest traffic a gateway
carries, and it is exactly the traffic a prompt cache exists for, so pricing it
at the small-request rates understates the bill on the requests that dominate
it.

```yaml
cost:
  input_per_1m: 3.00
  output_per_1m: 15.00
  cache_read_per_1m: 0.30
  cache_write_per_1m: 3.75
  long_context:
    above_prompt_tokens: 200000
    input_per_1m: 6.00
    output_per_1m: 22.50
    cache_read_per_1m: 0.60
    cache_write_per_1m: 7.50
```

The threshold is written out rather than assumed, because where a provider draws
the line is a fact about its price list rather than about caching. It is
compared against every prompt token the response reported — ordinary input plus
cache reads plus cache writes — since that is the figure a provider measures its
own threshold against. A conversation reading 190k tokens out of the cache is a
large request and is charged as one.

The block is optional and refused when incomplete, for the same reason a partial
cost model is:

| Rule | Why |
|---|---|
| `above_prompt_tokens` above zero | without a threshold nothing decides which tier a request falls into |
| the same completeness rules as the base block | a rate the tier omits falls back to the base rate, so a tier naming only its input price charges the premium on input and the small-request price on everything else |
| `output_per_1m` required where the base block prices output | otherwise long-context output is billed at the small-request rate |
| every rate at least its base counterpart | the tier is what a provider charges *extra*, so a cheaper rate here is the two blocks written the wrong way round |

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
| `keys[].max_budget` | unlimited | Only billable traffic counts. This key's alone; see `rbac` for a cap several keys share. |
| `keys[].budget_duration` | lifetime | Window the budget applies over. |
| `keys[].scope` | — | A project, team or organisation declared under `rbac`. Its limits bind this key, and its budget is a pool this key draws from. |
| `keys[].blocked`, `expires_at` | — | |

## `rbac`

Absent means keys are flat and every check below is skipped. Declaring it gives
keys somewhere to belong, and gives budgets and rate limits somewhere to be
shared.

```yaml
rbac:
  organizations:
    - id: acme
      max_budget: 10000
      budget_duration: 720h
      models: ["anthropic-*"]
      teams:
        - id: platform
          alias: Platform Engineering
          max_budget: 2000
          budget_duration: 720h
          rpm_limit: 600
          projects:
            - id: gateway
              max_budget: 500
              budget_duration: 720h
```

| Key | Default | Notes |
|---|---|---|
| `organizations[].id` | required | One path segment. Must not contain `/`. |
| `…teams[].id`, `…projects[].id` | required | Likewise. Nesting builds the qualified id: `acme/platform/gateway`. |
| `alias` | — | Label for `/spend/scopes`. The id is what everything keys on. |
| `models` | — | Restricts what may be called beneath this scope. |
| `rpm_limit`, `tpm_limit` | unlimited | One allowance shared by every key beneath the scope, not one each. |
| `max_budget` | unlimited | A pool. Every key beneath the scope draws from it. |
| `budget_duration` | forever | Window `max_budget` applies over. |
| `blocked` | `false` | Disables every key beneath the scope at once. |

The hierarchy is expressed by nesting rather than by parent references, so a
team cannot be declared under an organisation that does not exist and a
membership is stated once. The qualified id — the path — is what a key, a role
or `/key/generate` names.

### Entitlements narrow going down

A scope's limits bind every key beneath it **in addition to** the key's own, and
never instead of them. Three consequences worth stating:

- **Models intersect.** A key listing a model its team withholds may not call
  it, and the refusal names the level that withheld it rather than blaming the
  key. A scope naming no models abstains rather than granting everything, so a
  parent's list still applies. A child listing a concrete model its parent
  withholds is refused at load: it can never work, and nothing else would say so.
- **Budgets are pools, and every level is charged.** One request records against
  the key, its project, its team and its organisation — four subjects. A refusal
  names the innermost cap that bound, because that is the level whose owner can
  act on it.
- **Rate limits are pools too**, and the chain is reserved in one all-or-nothing
  operation — so a request a team refuses charges nothing to the key's own
  window, and depth costs no extra round trips.

A key naming a scope that is not configured is **refused**, not treated as
unscoped. The key store outlives the config that produced it, so a renamed team
would otherwise silently drop the cap its members were issued under.

`/spend/scopes` reports each level's pooled totals. Summing the `/spend/keys`
rows gives a different and wrong number: a key can leave a team, and its
historical spend does not leave with it.

## `sso`

Issues virtual keys from an OpenID Connect login, so a developer runs
`gateway login` instead of an operator minting a key by hand. Absent disables
the `/sso/*` endpoints entirely. See **[sso.md](sso.md)**.

| Key | Default | Notes |
|---|---|---|
| `issuer` | — | Provider base URL. Setting it enables SSO. Must be `https` unless it is loopback. |
| `client_id` | required | The gateway is the only OIDC client; the CLI registers nothing. |
| `client_secret` | — | Omit for a public client. |
| `scopes` | `[openid, email, profile, groups, offline_access]` | `offline_access` is what buys the refresh token renewal re-checks the identity with. |
| `redirect_url` | required | The gateway's own `/sso/callback`, registered at the provider. |
| `key_duration` | `720h` | How long an issued key lives. |
| `renew_within` | `168h` | How far ahead of expiry the client renews. Must be below `key_duration`. |
| `role_claim` | `groups` | Claim matched against `roles[].match`. A string or a list of them. |
| `roles[].match` | required | First match wins; `*` matches anything, so a catch-all belongs last. No match is refused. |
| `roles[].models`, `rpm_limit`, `tpm_limit`, `allow_passthrough`, `max_budget`, `budget_duration` | — | The same fields as `virtual_keys.keys[]`. |
| `roles[].scope` | — | Places everyone matching the role in a scope declared under `rbac`. This is what makes a role a pool rather than a template — see below. |
| `base_url` | origin of `redirect_url` | Handed to clients as `ANTHROPIC_BASE_URL`. |
| `model` | the only group, if there is one | Handed to clients as `ANTHROPIC_MODEL`. Required when several groups exist. |
| `jwt_auth.enabled` | `false` | Accept the provider's own tokens on inference requests, as well as issued keys. |
| `jwt_auth.audiences` | `[client_id]` | The `aud` values a token may carry. An access token issued for an API names that API rather than the gateway's client id. No wildcard: `"*"` is refused at load rather than read as a literal audience. |
| `jwt_auth.cache_ttl` | `60s` | How long a verified token's result is reused. Never past the token's own expiry. An explicit `0` is honoured and means "verify every request". |

Entitlements are read from this file and never from the token. A claim that
could grant a model or raise a budget would make any claim-mapping mistake at
the provider a privilege escalation here.

### A role without a scope is not a pool

`roles[].max_budget` is granted to **each** identity the role matches, so a role
covering ten developers with a `max_budget` of 200 permits 2000 of spend. That
is right for a per-person allowance and wrong for a team's.

Naming a `scope` fixes it without changing what the role means: every identity
matching the role now records its spend against that scope as well as its own,
so the scope's `max_budget` is one cap they share and the role's own becomes a
per-person cap *within* it. Both are enforced, innermost first.

### `jwt_auth`

With it on, a caller may present the provider's own token in the same header a
virtual key travels in. The gateway verifies it against the provider's published
keys on every request, maps it through the same `roles`, and authenticates it as
an ephemeral key that is never stored.

The trade against an issued key is revocation for moving parts:

| | Issued key | `jwt_auth` |
|---|---|---|
| Verified | once, at login | every request |
| Revocation reaches the gateway | when the key expires (`key_duration`, default 30 days) | when the caller's current token does, typically an hour |
| Caller must | hold one static string | hold a live token and refresh it |
| Works with Claude Code + subscription | yes | no — the credential header is static |

So this is for callers a key does not suit: CI, a service, a script running under
a workload identity. It sits **beside** the key path rather than replacing it,
and the two share a spend subject, so one person moving between them draws on
one budget.

Where a token names several audiences and the only one the gateway accepts is
its own `client_id`, the `azp` claim must also be the `client_id` — otherwise a
token another application in the organisation obtained, which happens to name
this gateway among its audiences, would authenticate here. The check stands down
only where the audience that matched is *not* the client id, which is the access
token case: there `aud` is the API and `azp` is legitimately the calling client.

A credential is routed by shape. A JWT is three base64url segments separated by
dots whose first segment decodes to a JSON object naming an algorithm; a virtual
key generated here is `sk-vk-` followed by base64url, which contains no dot. The
header check is what makes this safe for a key you wrote by hand: `team.gateway.2024`
is three base64url runs separated by dots, and without it, enabling `jwt_auth`
would start rejecting that key. A token presented to a gateway with `jwt_auth`
off is refused as an unknown key.

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
| `affinity_ttl` | `5m` | How long a pin survives without use. Matches the lifetime of an ephemeral prompt-cache entry. A request declaring the one-hour cache is pinned for an hour regardless, so the pin does not expire before the entry it points at. |
| `affinity_max_in_flight_lead` | `4` | A pin is passed over once the pinned deployment carries this many more in-flight requests than the idlest deployment that could serve it instead. `0` yields to any idler peer. |
| `inject` | `false` | Place cache breakpoints on a request that carries none of its own. On an `anthropic` deployment: the tools, the system prompt, and the top-level field that caches the conversation. On an `openai` one declaring `supports_cache_control`: the leading system message. |
| `inject_min_bytes` | `4096` | Prompts smaller than this are left unmarked; a provider would ignore the breakpoint anyway. Measured over tools, system **and** messages, since the conversation is cached too. |

Which deployments a breakpoint reaches is decided per deployment, at dispatch. A
passthrough deployment is never annotated — it must forward the caller's body
unchanged — so a mixed fleet serves both halves correctly: the API deployments
get breakpoints and the subscription traffic is forwarded verbatim.

An `openai` deployment gets one only where `params.supports_cache_control` says
its upstream reads the marker. Most of that ecosystem caches automatically and
would ignore or refuse one; Alibaba's Qwen is the provider this is for. See
[prompt-caching.md](prompt-caching.md#breakpoints-on-an-openai-format-upstream).

`inject: true` is **refused at load** only where *no* deployment could carry a
breakpoint, which is a setting that says one thing and does nothing.

An upstream that answers `400` to an annotated body is retried once with the
request as it arrived. Where that succeeds the deployment is named in the log and
is not annotated again, so the round trip is paid once rather than per request.
See [prompt-caching.md](prompt-caching.md#when-an-upstream-refuses-an-annotation).

## `observability`

| Key | Default | Notes |
|---|---|---|
| `log_level` | `info` | |
| `log_format` | `text` | `text` or `json`. |
| `metrics` | `false` | Serves `GET /metrics`. |
| `spend_store_path` | — | Persists budgets across restarts. |
| `spend_flush_interval` | `30s` | |
| `stream_usage` | `true` | Ask OpenAI-compatible upstreams to report usage on streamed replies. See [prompt-caching.md](prompt-caching.md#streamed-replies-report-nothing-unless-asked). |
| `otlp.endpoint` | — | Collector base URL. Empty disables the exporter unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set. |
| `otlp.protocol` | `http/protobuf` | Or `http/json`. |
| `otlp.interval` | `60s` | |
| `otlp.timeout` | `10s` | Must be below `interval`. |
| `otlp.compress` | `true` | gzip the payload. |
| `otlp.service_name` | `ai-gateway` | Becomes the `service.name` resource attribute. |
| `otlp.headers` | — | Sent on every export. |
| `otlp.resource_attributes` | — | Added to every export. |

`stream_usage` adds `stream_options.include_usage` to a streamed request that did
not set `stream_options` itself. Without it such a reply carries no usage at all
and the request is billed as zero — no cost, no budget, no cached tokens. The
cost of asking is one extra chunk at the end of the stream; turn it off for a
client or an upstream that cannot take one.

`otlp` pushes the same metrics `/metrics` serves to an OpenTelemetry collector.
The two are independent — either, both or neither may be on — and both render
one snapshot, so they cannot disagree about a number. See
[observability.md](observability.md#opentelemetry). The standard
`OTEL_EXPORTER_OTLP_*` environment variables are read where this block is
silent, so a collector sidecar that configures everything else in a fleet
configures this too.

## `ui`

The admin console, served from the gateway's own binary at `/ui`. Off by
default.

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | Mounts `/ui` and `/ui/api`. Requires `virtual_keys.master_key`. |
| `session_ttl` | `12h` | How long a browser session lives. |
| `request_log_size` | `1000` | Recent requests kept for the traffic view. `0` keeps none. |

Enabling it without a master key is refused at load: signing in means presenting
one, so a console without it is a sign-in page nobody can pass.

The session is a signed token carrying its own expiry, held in a cookie scoped
to `Path=/ui`. That scope is load-bearing rather than tidy — a cookie the
browser also attached to `POST /v1/messages` would be a session riding along on
every inference request a page could be tricked into making, which is the
credential collision this gateway exists to avoid wearing a different hat.
`auth.ExtractGatewayKey` reads headers only, so nothing on the inference path
can read a cookie even if one arrived.

The signing key is derived from the master key, so every instance verifies every
other's sessions without sharing any state — the arrangement
`deploy/docker-compose.yml` runs, where a session held in one process's memory
would sign the operator out on every second request. The cost of statelessness
is that a session cannot be revoked before it expires, which is why the TTL is
short; rotating the master key invalidates all of them at once.

Mutating requests must carry an `x-gateway-ui` header. `SameSite=Strict` is the
first defence and this is the second: SameSite is a browser policy, while a
header a cross-site form post cannot set is a property of the request itself.

`request_log_size` bounds an in-memory ring of completed requests — metadata
only, never a request or response body. It lives in the process that served the
traffic, so a fleet behind a load balancer shows each instance its own slice
rather than the whole, and the console says so rather than implying otherwise.
Unauthenticated refusals are counted in the metrics but deliberately not kept:
they are decided before any credential is verified, so recording them would let
anyone able to open a socket evict the buffer.

The console is built separately from the binary:

```bash
make ui && make build
```

A binary built without it serves a page saying so; the gateway proxies inference
normally either way.

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
| `GET /sso/login`, `GET /sso/callback` | none — the identity provider, plus a single-use state. Registered only when `sso.issuer` is set |
| `POST /sso/exchange` | none — a single-use code and the client's PKCE verifier |
| `POST /sso/renew` | the provider's refresh token |
| `POST /key/generate`, `GET /key/info`, `GET /key/list`, `POST /key/update`, `POST /key/delete` | master key |
| `GET /spend/keys`, `GET /spend/scopes`, `GET /spend/deployments` | master key |
| `POST /cache/purge` | master key |
| `GET /ui/`, `POST /ui/api/session` | none — the sign-in page and the exchange itself. Registered only when `ui.enabled` |
| `GET|POST /ui/api/*` | the browser session, plus `x-gateway-ui` on anything that changes state |

`POST /key/update` edits a stored key in place. Every field is optional and an
omitted one is left alone, so a partial update cannot silently reset what it did
not mention; an empty `duration` or `budget_duration` clears that window. It
exists because revoke-and-reissue is not equivalent: a key's hash is what its
spend, its budget window and its rate-limit counters are addressed by, so
replacing a key to raise its budget resets the window it was spending against
and hands its holder a new secret to install everywhere.

Blocking is therefore distinct from deleting. A blocked key stops spending but
keeps answering "what did this cost"; a deleted one takes its ledger entry with
it.

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
