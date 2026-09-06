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

### 🟡 Service tiers are not priced

`cost.long_context` prices the one tier dimension that Claude Code traffic
actually crosses. It is not the only one a provider has: a request served at a
priority or a flex service tier is charged at its own rates for every class of
token, cache reads and writes included, and the gateway prices it at the
standard ones.

A caller asks for a tier in the request body, which the gateway forwards
untouched, so the misprice is silent in the same way the long-context one was.
LiteLLM carries suffixed rate keys for each tier and picks between them on the
request. The same shape would work here — `cost.service_tiers.<name>` resolved
the way `long_context` is — and it wants the tier read out of the body during
the walk the router already performs, rather than a second parse.

### 🟡 The injection minimum is measured in bytes, not tokens

A provider's minimum cacheable prefix is counted in tokens and differs by model:
1024 for the larger Claude models, 2048 for the smaller ones.
`prompt_cache.inject_min_bytes` is a byte count, defaulting to 4096 — roughly
1000 tokens, which is right for the larger models and half of what the smaller
ones need.

Counting tokens instead means running a tokenizer over every request, which is
what LiteLLM does: it carries a per-model `prompt_cache_min_tokens` and tokenizes
to compare against it, twice per request on the routing path. That is a real
per-request cost for a threshold that only decides whether an optimization is
attempted, and being under it costs nothing — the provider ignores a breakpoint
below its minimum rather than rejecting it.

The gap is an operator on a small model who leaves the default and gets
breakpoints the provider ignores. [prompt-caching.md](prompt-caching.md#cache-breakpoints)
says to raise it; a per-deployment minimum would say it for them. The
per-deployment body transform it wanted now exists, so the remaining work is a
`prompt_cache_min_bytes` on `DeploymentParams` read by the annotator.

### 🟡 An OpenAI-format breakpoint caches the prefix, not the conversation

Injection on an `openai` deployment declaring `supports_cache_control` marks the
leading system message and nothing else. Anthropic's third breakpoint is the
top-level `cache_control` field — the API's own automatic caching, which walks a
marker forward as turns accumulate — and the OpenAI format has no equivalent.

So a caller with a large system prompt and short turns gets the whole benefit,
and one whose history grows re-reads that history at full price behind the prefix
marker. Qwen documents up to four markers per request, so the shape of the fix is
a second marker on a trailing message; the reason not to have taken it is the one
that applies under Anthropic too — the gateway would have to move that marker
itself every turn, and a marker it moves is a marker it must be sure the upstream
accepts on whatever block ends the newest turn.

### 🟡 Injected breakpoints are always ephemeral, and always in the same three places

Injection marks the end of the tools, the end of the system prompt and the
conversation, each at the default five-minute lifetime. There is no way to ask
for the one-hour tier, and no way to move or add a breakpoint.

LiteLLM takes configurable injection points — by role or by message index,
negative indexes included, each with its own TTL. The reason not to follow it on
the TTL is that a one-hour write costs twice base input against 1.25x: injecting
one is spending a caller's money on a lifetime it did not ask for, and the
caller that wants it can say so, at which point injection stands down anyway
because the request now carries breakpoints of its own. The reason not to follow
it on the positions is that the three the gateway marks are the ones it can
identify structurally; an index into a message array is a shape only the caller
knows.

Both would become reasonable alongside a per-deployment or per-key injection
policy. The per-deployment half of that seam now exists; a per-key one would
have to reach the annotator from the auth context.

### 🟡 Savings do not say which breakpoints earned them

`cache_savings` is one figure per request. Where `prompt_cache.inject` is on it
mixes savings the caller's own breakpoints earned with savings the gateway's
injected ones did, so it cannot answer whether injection is paying for itself —
which is the question an operator turning it on actually has.

LiteLLM stamps the injecting deployment into request metadata and splits the two
in its spend reporting. Doing the same here means carrying a flag from injection
through to the ledger entry and the persisted totals, which changes the on-disk
ledger format; it was not worth that before the feature had users.

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

### 🟡 Provider cache shapes are read from docs, not from live traffic

The OpenAI-compatible ecosystem does not agree on where the cache counters go.
OpenAI, Kimi and GLM report a read as `prompt_tokens_details.cached_tokens` and
no write; DeepSeek reports the read at the top level as
`prompt_cache_hit_tokens`; Qwen and MiniMax charge for a write and report it
nested inside `prompt_tokens_details`, where Qwen also nests the TTL breakdown.
All of those are read, and the fixtures in `usage_test.go` are the shapes each
provider's own documentation gives.

None of it has been seen on the wire from this gateway. The shapes came from
provider docs cross-checked against LiteLLM's regression fixtures, which is a
good deal better than inference but is not the same as a response from the
provider. It shares the caveat at the top of this file: nothing here has talked
to a real upstream.

The failure mode if a shape is wrong is quiet in the usual way. A counter under
an unread name lands in the input bucket and is billed at the input rate, which
is roughly right for the providers that write for free and understates the two
that charge. Adding a provider means adding a fixture, not a code path.

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

Narrower than it first looks: the field is OpenAI's own, and none of Kimi, GLM,
DeepSeek or Qwen accepts it. It biases routing inside one provider's fleet for a
caller sending one prefix at high rates.

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

The plumbing is now in place. `provider.Request` carries an `Annotator` rather
than a finished body, and `Client.Do` derives what one deployment receives at
dispatch, so a field worth varying per upstream has somewhere to be decided —
and an upstream that refuses it is remembered rather than re-asked. Prefix
affinity already gives the gateway the fingerprint such a key would be derived
from, so what remains is choosing the key, not carrying it.

The same seam covers OpenAI's explicit `prompt_cache_breakpoint` marker, which
LiteLLM does send on `gpt-5.6` and later. It is narrower: it applies to a
handful of models, and on the rest of an OpenAI-compatible fleet caching is
automatic and needs no marker at all. Sending it wants a per-model capability
check of the kind the cost map already holds.

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
| Provider prompt cache | routed for, by default | LiteLLM has the same idea behind an opt-in `optional_pre_call_checks` entry, so the default arrangement pays a cache write on every hop |
| OpenAI cached tokens | carved out of `prompt_tokens` | they are reported *inside* the input count, so adding them beside it bills every cached token twice. LiteLLM reaches the same answer by recomputing the input figure when the parts exceed the total |
| A nested cache-write counter | read at either level | Qwen and MiniMax nest `cache_creation_input_tokens` inside `prompt_tokens_details` and charge for it. LiteLLM reads the nesting too, after a regression that billed those tokens as input |
| A breakpoint on an OpenAI-format upstream | per-deployment capability | LiteLLM decides from a static per-provider table, which is right for the providers in it and silent about a self-hosted or unlisted one |
| An unset cache-write price | falls back to input | a missing price means the provider names no separate one, not that the write was free. LiteLLM reaches the same conclusion for its savings figures |
| Streamed OpenAI usage | asked for | a streamed reply reports none unless the request opted in, so the traffic is billed at zero |
| A moving cache breakpoint | ignored when pinning | every Anthropic client walks one forward each turn, which would re-pin the conversation every time. LiteLLM's affinity key is the messages up to and including the last breakpoint, marker bytes and all, so its own injected breakpoint moves the key every turn |
| Affinity key | prefix, tools and model group | LiteLLM hashes neither the tools nor the model, so two conversations sharing a history but not a toolset share a pin |
| A pin | written on evidence the provider cached | LiteLLM pins any successful call above a token count, so a prompt nothing cached still concentrates its traffic |
| Long-context pricing | a configurable tier | the largest requests are the ones a cache serves, and the small-request rates understate them by about half |
| Anthropic cache-write tiers | priced apart | the one-hour cache costs 2x base input against the five-minute tier's 1.25x |
| Pin lifetime | follows the declared cache TTL | a five-minute pin on a one-hour entry pays the long tier's premium a second time |
| `cache_savings` | net of the write premium | a gross figure cannot report that caching is costing money, which is what scattering looks like |
| Injected breakpoints | tools, system **and** the conversation | marking only the static prefix re-reads a growing history at full price every turn |
| An upstream that refuses an annotation | retried without it, then not annotated again | LiteLLM strips `cache_control` ahead of time per provider from a static rule and never retries, so an upstream that refuses for a reason not in that rule costs the caller their request |
| Passthrough cost | not billed to the operator | it is billed to the caller's subscription |

---

## Recording new work

Add an entry with a status marker, say what breaks without it, and note the
seam it would attach to. If something is a deliberate non-goal, mark it ⚪ and
say why — the reasoning is the part that gets lost.
