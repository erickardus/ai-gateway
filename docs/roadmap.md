# Roadmap and known gaps

What is not built, what is built but unverified, and what is deliberately
limited. Ordered by what would hurt most if left alone.

Status: 🔴 not started · 🟡 partial · ⚪ deliberate non-goal

---

## Blocking real use

### 🔴 Verify against a live provider

**Nothing in this repository has ever talked to a real LLM provider.** Every
test runs against `httptest` fakes; the end-to-end runs used local fake
upstreams and placeholder credentials.

The whole design turns on Anthropic accepting a relayed OAuth token alongside an
intact `anthropic-beta`, and that specific assertion is untested against the real
endpoint. Everything else is downstream of it.

The check is about five minutes on a machine with a Claude subscription:

1. `make build`
2. `cp config/gateway.example.yaml config/gateway.yaml`, keep the passthrough
   deployment, set `allow_passthrough: true` on your key
3. `export ANTHROPIC_BASE_URL=http://localhost:4000`,
   `ANTHROPIC_MODEL=anthropic-claude`,
   `ANTHROPIC_CUSTOM_HEADERS="x-gateway-key: sk-vk-…"`
4. **Do not** set `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` — either
   replaces the subscription
5. `claude` → `/login` → "Claude account with subscription"
6. `/status` should show the base URL **and** a claude.ai login line, and **no**
   auth-token or API-key line

Failure signatures worth recognising:

| Symptom | Likely cause |
|---|---|
| `401` on every request | `anthropic-beta` lost the OAuth capability, or a credential variable displaced the subscription |
| Works, but billed per token | `ANTHROPIC_API_KEY` is set somewhere, or an `apiKeyHelper` is configured |
| Long completions truncate | a proxy between client and gateway is buffering |
| `400` naming an unknown field | something is reshaping the request body |

Until this is done, treat "functional" as *correct against a faithful fake*.

---

## Correctness gaps

### 🟡 In-flight and latency are per-instance

Deliberate — latency measures one instance's own path to an upstream, and a
shared in-flight counter leaks when an instance dies mid-request — but it means
`least-busy` and `latency-based` balance within an instance, not across a fleet.
With a load balancer in front this is usually fine; with uneven instance sizing
it is not.

A self-healing shared design is possible: per-instance keys with a TTL, summed
by readers. It costs a round trip per candidate and was not obviously worth it.

### 🟡 The cache's error-status guard is untested

`cacheable()` refuses non-2xx responses, but the transport already converts any
status ≥400 into an error, so the branch is unreachable today. It stays as
defence in depth. A mutation test cannot currently reach it, and the code says
so rather than looking covered.

### 🟡 Breakpoint injection is refused alongside passthrough

`prompt_cache.inject` rewrites the request body to mark a cacheable prefix. The
passthrough path must forward a body unchanged, so configuration refuses the two
together rather than letting subscription traffic be quietly rewritten. A gateway
fronting both Claude Code and plain API callers therefore cannot use injection
at all today, even for the callers that would benefit.

The seam is a per-deployment body rewrite: `provider.Request` would carry the
transform rather than a finished body, and `Client.Do` would apply it only for
deployments whose auth mode permits it. That means the router can no longer
assume one set of bytes per request, which touches retry, fallback and the
response-cache key, so it was not worth doing before the feature had users.

Claude Code places its own breakpoints, so nothing is lost on the traffic this
gateway primarily carries.

### 🟡 A deployment that refuses an annotation is retried, not remembered

Injection marks three positions, one of which — the top-level `cache_control`
field — is not accepted everywhere; the legacy Bedrock integration rejects it.
An upstream answering `400` to an annotated body is retried once with the
request as it arrived, so the caller never loses a request, and the deployment
is named in the log.

The round trip is paid on **every** request until an operator acts on that line.
Remembering the rejection per deployment would avoid it, but injection is decided
before the router picks one, so the body would have to be annotated per attempt
rather than per request — the same per-deployment body transform the entry above
wants. Until then the log line is the mechanism, which is why it names the
setting to unset rather than just reporting the status.

### 🟡 A declared one-hour cache is read from the prefix only

A pin lives as long as the cache entry it points at, so a request declaring the
one-hour breakpoint is pinned for an hour rather than for `affinity_ttl`. The
declaration is read from the tools and the system prompt, which are small and
already walked. The API requires longer-lived entries to precede shorter-lived
ones, so a one-hour breakpoint sharing a request with any five-minute one is in
those; a request whose *only* breakpoint is a one-hour marker further into the
message history is pinned for the default instead.

Catching that case means walking the whole message array on every request, which
is the cost the fingerprint is deliberately shaped to avoid. The consequence is a
pin that lapses early on an unusual request shape, not a wrong one.

### 🟡 Prefix affinity concentrates load, bounded by in-flight rather than share

A fingerprint covers a request's prefix, not a conversation, so all traffic
sharing a system prompt, its tools and its opening two turns shares one pin.
For a conversational caller that is exactly right. For a templated single-turn
caller it means the whole workload has one pin, and before
`affinity_max_in_flight_lead` existed every request of it landed on one
deployment while the rest of the group sat idle.

The bound is a load comparison, so it fires on contention rather than on share.
That is deliberate — concentration without contention costs nothing — but it
means a workload that concentrates *and* keeps in-flight low, many small fast
requests against a fast upstream, is not redistributed. Throughput is fine there
and cache hits are maximal, so the cost is paid capacity going unused rather than
latency. An operator who wants that capacity used needs `rpm` on the deployment,
or `affinity: false`.

A share-based bound would catch it, but wants a per-deployment request rate
compared against weight share, which under Redis is a round trip per candidate on
every request — the same objection that keeps in-flight and latency local.

`gateway_prompt_affinity_total{outcome="miss"}` alongside `gateway_in_flight`
shows the bound firing.

### 🟡 Cache-write tiers are priced, not verified

The gateway reads Anthropic's `cache_creation` breakdown and prices the
one-hour tier separately from the five-minute one. Where a deployment has no
`cache_write_1h_per_1m` it falls back to the five-minute price and logs a
warning naming the deployment, once, on the first response that reports a long
write.

That is the right default and an honest signal, but it is still a fallback: the
window between the first long write and an operator reading that line is billed
at the lower rate. Refusing the request instead would be worse — the traffic is
already served — and refusing the config would demand a price from every
operator whose callers never use the longer TTL.

### 🟡 Budget windows reset lazily

A key's budget window resets on its **first request after** the window elapses,
not on a timer. A key that goes quiet for a month and returns sees its window
reset then. Correct for enforcement, slightly surprising in reports.

### 🔴 No spend history

The ledger holds current-window totals only. There is no per-request log and no
way to answer "what did this key spend last Tuesday". That needs a real
datastore behind `spend.Store`.

---

## Not built

### ⚪ Cross-format translation

An Anthropic ingress reaches only `anthropic` deployments; OpenAI only `openai`.
Enforced in routing and validation.

This is a **non-goal for now, not an oversight**. Anthropic's gateway rules
require forwarding request bodies unchanged, and translation is precisely the
opposite. Building it means mapping content blocks, `tool_use`/`tool_result`, and
the streaming event grammar in both directions — large, and in tension with the
guarantee that makes the passthrough path work.

If it is wanted, the shape is a `Transformer` between ingress and `provider`,
applied only to deployments whose format differs from the ingress, never on the
passthrough path.

### 🔴 OpenAI's `prompt_cache_key` is not sent

An OpenAI-compatible provider caches automatically, keyed on the prefix itself.
`prompt_cache_key` is an optional field that biases that provider's own internal
routing, and it earns its keep for a caller sending one prefix at high rates —
which is what a company standardizing on one system prompt looks like.

The gateway does not send it. It does now edit an OpenAI-format body — a
streamed request gets `stream_options.include_usage` so the deployment can be
billed at all — so the objection is no longer that such an edit is impossible.
It is that this one is optional where that one is the difference between a bill
and a zero: an OpenAI-*compatible* server strict about unknown fields would 400
rather than ignore it, and the same risk buys much less.

Both edits happen before routing, so the response-cache key is taken from the
body as it arrived and every retry and fallback attempt sends the same bytes.
A per-deployment transform carried on `provider.Request` and applied by
`Client.Do` is still what a field worth varying per upstream would want. Prefix
affinity already gives the gateway the fingerprint such a key would be derived
from, so the work is the plumbing rather than the value.

### 🔴 No cache-hit-rate signal per prefix

Metrics report prompt-cache hit rate per model and deployment, which answers
"is caching working" but not "which prompt shape is missing". Answering the
second means keying counters by fingerprint, which is unbounded cardinality on a
Prometheus endpoint. It needs a sampled top-N, not another label.

### 🔴 Guardrails and content filtering

No PII detection, no prompt-injection screening, no content policy hooks. The
natural seam is a pre-dispatch and post-relay hook alongside the existing budget
check.

Note the tension: a guardrail that **modifies** a request body breaks the
byte-preservation the passthrough path depends on. Inspection-only guardrails
compose; rewriting ones need a deliberate exception.

### 🔴 Teams and organisations

Keys are flat. No team-level budgets, no inherited limits, no hierarchy.
`core.Key` would gain a parent reference and budget checks would walk it.

### 🔴 Persistent key store

`memory` and `file` only. `file` is single-node: two instances with the same path
will clobber each other. `auth.KeyStore` is ready for Postgres.

### 🔴 Admin UI

Everything is config plus the `/key/*` and `/spend/*` endpoints.

### 🔴 MCP gateway, batches, embeddings, audio

`/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions` and
`/v1/models` only.

### 🔴 Health-check probes

`/health` reports state the gateway already knows — cooldowns, in-flight — but
never probes upstreams. A deployment that has gone bad while idle is discovered
by the first request to fail on it.

---

## Operational

### 🔴 No Helm chart or compose file

A `Dockerfile` exists. No deployment manifests, no readiness/liveness wiring
beyond the endpoints themselves.

### 🔴 No structured audit log

Access logs carry the key alias, model and outcome, but there is no separate
tamper-evident audit stream.

### 🟡 Metrics have no build info

No `gateway_build_info` series, so a dashboard cannot distinguish versions
during a rollout.

---

## Deliberate divergences from LiteLLM

Not gaps — decisions, recorded so they are not "fixed" by accident.
[routing.md](routing.md#differences-from-litellm) has the full table.

| Behaviour | Here | Why |
|---|---|---|
| `rpm`/`tpm` | enforced limits | LiteLLM treats them as weights under its default strategy and never enforces them |
| Explicit config zeros | honoured | LiteLLM conflates `0` with unset, silently voiding configured values |
| Retry target | excludes failed deployments | LiteLLM may re-pick the one that just failed |
| Rate-limit windows | monotonic | LiteLLM's wall-clock buckets wrap daily |
| `anthropic-beta` | forwarded verbatim | LiteLLM validates against a pinned list, which breaks on new Claude Code releases |
| Cache scope | per-key by default | shared-by-default leaks completions across tenants |
| Provider prompt cache | routed for | LiteLLM balances without regard to it, so every hop pays a cache write instead of a read |
| OpenAI cached tokens | carved out of `prompt_tokens` | they are reported *inside* the input count, so adding them beside it bills every cached token twice |
| Streamed OpenAI usage | asked for | a streamed reply reports none unless the request opted in, so the traffic is billed at zero |
| A moving cache breakpoint | ignored when pinning | every Anthropic client walks one forward each turn, which would re-pin the conversation every time |
| Anthropic cache-write tiers | priced apart | the one-hour cache costs 2x base input against the five-minute tier's 1.25x |
| Pin lifetime | follows the declared cache TTL | a five-minute pin on a one-hour entry pays the long tier's premium a second time |
| `cache_savings` | net of the write premium | a gross figure cannot report that caching is costing money, which is what scattering looks like |
| Injected breakpoints | tools, system **and** the conversation | marking only the static prefix re-reads a growing history at full price every turn |
| Passthrough cost | not billed to the operator | it is billed to the caller's subscription |

---

## Recording new work

Add an entry with a status marker, say what breaks without it, and note the
seam it would attach to. If something is a deliberate non-goal, mark it ⚪ and
say why — the reasoning is the part that gets lost.
