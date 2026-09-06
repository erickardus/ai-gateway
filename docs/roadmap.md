# Roadmap and known gaps

What is not built, what is built but unverified, and what is deliberately
limited. Ordered by what would hurt most if left alone.

Status: 🔴 not started · 🟡 partial · ⚪ deliberate non-goal

---

## Blocking real use

### 🟡 Verify against a live provider

The gateway has now served real traffic, but not from the provider the design
turns on.

What has been exercised live: a third-party upstream reached in both wire
formats — an `openai` deployment and an `anthropic`-format one on the same
account — under an `api_key` deployment. That run paid for itself immediately by
finding two faults no fake had: `/key/generate` silently discarding
`max_budget`, and `validateRates` refusing to price any `anthropic`-format
deployment whose upstream writes its cache for free. Both are fixed, and the
second is why `cost.cache_writes_free` exists.

What has **not**: `api.anthropic.com`, and therefore the assertion everything
else is downstream of — that Anthropic accepts a relayed subscription OAuth
token alongside an intact `anthropic-beta`. Every test of that path still runs
against an `httptest` fake, and a fake agrees with whatever it was written to
agree with. Until this is done, treat the passthrough path as *correct against a
faithful fake*, and the rest as correct against one real upstream that is not
Anthropic.

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

### 🟡 A budget is checked before the call, so concurrency overshoots it

`CheckBudget` reads what a key has spent and compares it against the cap. It
reserves nothing, and the cost of a request is not known until its response has
been read — so every request already in flight when the boundary is crossed is
admitted, and the cap is exceeded by roughly their combined cost. A key at its
limit is refused from the next request onward, which is the behaviour that
matters; the overshoot is bounded by that key's concurrency, not by time.

Closing it properly means reserving an estimate at admission and reconciling
against the real cost at completion, which puts a guess into the ledger and a
second Redis round trip on the request path. The cheaper half — refusing once
spend plus the cost of what is already in flight would cross the cap — needs a
per-key in-flight cost estimate the gateway does not currently keep. Both want
the same seam: a reservation alongside `Record` in `spend.Store`.

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
A model group may not mix formats, and a fallback may not cross them. Enforced in
`router.candidates` and twice in `config.validate`.

This is a **non-goal for now, not an oversight**.
[architecture.md](architecture.md#there-is-no-cross-format-translation) records
why. What follows is the size of the thing, so that a later decision to build it
is taken against the real number rather than against "map the fields".

| Surface | The work |
|---|---|
| Envelope | `system` is a top-level field one side and a message the other. `max_tokens` is required one side, optional the other. `temperature` is 0–1 against 0–2, so a faithful mapping rescales rather than copies. Each side has parameters the other cannot express — `n` and `logprobs` one way, a `thinking` budget and `cache_control` the other |
| Content blocks | Anthropic's typed block list against OpenAI's string-or-parts plus a sibling `tool_calls` array. Tool results move from blocks inside a user message to separate `role: "tool"` messages — 1→N, so the arrays do not correspond by index and cannot be walked in step. Images move between `source.data` and a `data:` URL. `thinking` signatures have nowhere to go |
| Tools | `input_schema` against `function.parameters`. `tool_choice` vocabularies that overlap without either containing the other: `any` and `required` are the same thing under different names |
| Streaming | Anthropic's stateful event grammar against OpenAI's flat chunk sequence. Index-addressed tool-call fragments must be reassembled into `content_block_start`/`delta`/`stop` lifecycles with block indices the gateway invents, and `message_start` must carry an input token count that an OpenAI-compatible upstream does not report until its final chunk. Terminal reasons map unevenly too — OpenAI's `content_filter` has no `stop_reason` counterpart |

It is also not a feature that finishes. Every content block type either provider
adds afterwards is a new mapping in both directions, and a missing one degrades a
call rather than failing it — so the cost is a permanent one, carried against
providers that ship on their own schedule.

If it is wanted, the shape is a `Transformer` between ingress and `provider`,
applied only to deployments whose format differs from the ingress, never on the
passthrough path. The gain that would justify it is a fallback that crosses
vendors, which nothing else here can provide: today an Anthropic outage has no
escape hatch, because `validate.go` refuses a fallback leaving the format.

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

### ✅ Teams and organisations

Resolved. Keys were flat: `sso.roles` mapped an identity to entitlements, but
every identity matching a role received its own independent budget, so a role
was not a pool and a cap written once was multiplied by the number of people it
applied to.

`rbac` declares organisations, teams and projects; `core.Scope` is the resolved
node with a parent link; and a scope is **its own subject in the ledger**, so
every key beneath one records against the same entry and the cap is a ceiling
the members share. Budgets, `rpm` and `tpm` all pool, and model allowlists
intersect down the chain. `/spend/scopes` reports each level, and
`spend.Entry.Scopes` is how one request reaches all of them.

The whole chain is read in one call and reserved in one atomic operation, so
depth costs no extra round trips and a request refused by an outer scope leaves
no increment on an inner one. The budget overshoot below does apply per level
rather than once, since `CheckBudget` still reserves nothing.

### 🟡 A scope budget is checked before the call, like a key's

The concurrency overshoot described under
[budgets](#-a-budget-is-checked-before-the-call-so-concurrency-overshoots-it)
applies to a pool as well, and is larger there: the requests in flight when the
boundary is crossed belong to every member of the team, not to one key. The
bound is the team's combined concurrency rather than one caller's.

It wants the same fix and the same seam — a reservation alongside `Record` in
`spend.Store` — so it is one piece of work covering both, not two.

### 🟡 SSO covers a browser on the same machine, and nothing else

`gateway login` runs an authorization-code flow with a loopback redirect, which
needs a browser on the machine the CLI is on. A developer on a headless box or
over SSH has no way to complete it and falls back to a key from
`/key/generate`.

The fix is the device-authorization flow: the CLI prints a short code, the
developer completes it in a browser anywhere, and the CLI polls. It is an extra
endpoint pair and a polling loop against the provider's device endpoint, and it
reuses everything else — the role mapping, the key issuing, the settings
writer. It was left out because the stated audience was laptops.

### 🟡 The SSO endpoints are not rate limited

`/sso/login` takes no credential — it cannot, since issuing one is the point.
The pending-login map is capped at 10,000 entries so it cannot grow without
bound, and a one-time code is 256 bits and single-use, so guessing is not the
exposure. What remains is that a caller can occupy that cap, and make the
gateway generate tokens and read a cached discovery document, at whatever rate
they like.

The gateway has no rate limiting on unauthenticated endpoints generally, so this
is not a gap peculiar to SSO — but it is the only unauthenticated endpoint that
allocates. A per-IP limiter in front, or the reverse proxy most deployments
already have, covers it.

### 🟡 The refresh token is a file, not a keychain entry

`~/.claude/.gateway-sso.json` holds the provider's refresh token at `0600`.
Whoever can read it can obtain a gateway key for that identity until the
provider revokes it.

It sits beside the virtual key it renews, at the same permissions, in the same
directory — so it is not a new class of secret on the machine, and an attacker
who can read one can read the other. But a key that must be re-obtained is
weaker than one already in hand, and the OS keychain is where it belongs. That
means three platform paths (Keychain, libsecret, DPAPI), which is why it is not
here yet.

### ✅ A scope chain cost a round trip per level, and reserved non-atomically

Resolved, and the two halves had one fix. `CheckBudget` issued one read per
budgeted scope and `Admit` one reservation per limited scope, each sequential —
so a full organisation → team → project hierarchy added up to six round trips to
every request. Worse, the reservations were separate operations, so a request
refused by an outer scope kept the increment it had already made to an inner
one: a key's own window ran ahead of the requests it actually served, and a
caller sitting against a team limit burned their personal allowance doing
nothing.

Both walks are over a set known before the request starts, so neither had to be
sequential. `spend.Store.Spends` takes a set and the Redis ledger pipelines it;
`auth.KeyLimiter.ReserveAll` takes a set and `reserveAllScript` checks every
limit before moving any counter. The chain is now one round trip for the read
and one for the reservation, whatever its depth, and the reservation is
all-or-nothing.

Two things worth keeping: a chain declaring no limits anywhere short-circuits
without touching Redis, so the common case costs nothing; and degrading to the
local limiter keeps the all-or-nothing property rather than dropping to
per-subject reservation while Redis is away.

### 🟡 JWT auth is bounded by the token's lifetime, not by a revocation list

`sso.jwt_auth` verifies the provider's own token on every request, which is the
point: a suspended account stops working as soon as its current token does,
where a virtual key issued at login is trusted until it expires.

"As soon as its current token does" is the limit. The gateway reads no
revocation list and calls no introspection endpoint, so a token already in hand
stays good for its remaining life — typically under an hour, and set by the
provider rather than by anything here. Closing that means an introspection call
per request, which is a network round trip on the request path for a window most
operators consider acceptable. The verified-token cache is a smaller version of
the same trade and is already bounded by the token's own expiry, never past it.

### ⚪ No SAML

OIDC only. SAML would mean verifying XML digital signatures, which is a large
hand-rolled surface and the classic source of authentication bypasses, or a
dependency this project does not otherwise need. An organisation with only SAML
provisioned can usually front it with an OIDC-speaking broker.

### 🔴 Persistent key store

`memory` and `file` only. `file` is single-node: two instances with the same path
will clobber each other. `auth.KeyStore` is ready for Postgres.

### ✅ Admin UI

There was no way to see who was signed in, or to revoke someone, without curl.

Built as a Vite + React console embedded into the binary and served at `/ui`,
off by default. Seven pages: health, per-request traffic, keys, the three spend
ledgers, deployments, the RBAC tree and an ops page. `POST /key/update` came
with it, so blocking or re-budgeting a key is an edit rather than a
revoke-and-reissue that would reset the window its spend accumulates in.

The load-bearing decision is that the browser session is a third credential
plane, not a reuse of the first. Its cookie is scoped to `Path=/ui` so the
browser never attaches it to `/v1/messages`, and the console has its own API
under `/ui/api` rather than reusing the master-key endpoints — an endpoint
outside `/ui` is one the cookie cannot reach. See
[admin-ui.md](admin-ui.md#why-a-third-credential-plane).

Two gaps found while building it, both fixed: the scope ledger is keyed by
`Scope.SpendSubject()` rather than the bare id, so the first version of the
organisations page reported every pool as having spent nothing; and throughput
was measured on unstreamed replies, where the interval after the "first chunk"
is the time to write one buffer, reporting millions of tokens per second into a
histogram whose largest bucket is 1000.

### 🔴 The console signs in with the master key, not SSO

`ui.enabled` accepts one credential: the master key, typed into a form. That is
the most powerful credential the gateway has, and a page that asks for it is a
page that can be imitated.

Three things narrow it — the session is short-lived, the cookie cannot reach the
inference plane, and the key is never written anywhere the browser can read back
— but the real fix is to sign the operator in through the identity provider that
already exists. The OIDC flow is built; what is missing is a browser variant of
it that sets a session cookie instead of returning a key to a CLI, and a role
that grants console access. Until then the form should stay behind an operator's
deliberate `enabled: true`.

### 🔴 The console has no history, so it has no charts

The ledger holds current-window totals only, so every figure the console shows
is a number rather than a line. "Spend is $40" cannot be read as rising or
falling, which is the question an operator actually has.

The traffic ring is not the answer: it is a fixed thousand requests in one
process, sized for reading rather than archiving. Charting needs the same thing
[No spend history](#-no-spend-history) needs — time-bucketed rollups behind
`spend.Store` — after which the console is a rendering problem rather than a
data one.

### 🟡 Sign-in is not rate limited

`POST /ui/api/session` compares the presented master key in constant time and
logs a refusal, but nothing throttles attempts. A master key is 32 bytes of
entropy, so this is not a guessing risk; it is an unbounded log-writing and
hashing endpoint reachable by anyone who can open a socket, which is the same
gap [the SSO endpoints](#-the-sso-endpoints-are-not-rate-limited) have and wants
the same fix.

### 🔴 MCP gateway, batches, embeddings, audio

`/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions` and
`/v1/models` only.

### 🔴 Health-check probes

`/health` reports state the gateway already knows — cooldowns, in-flight — but
never probes upstreams. A deployment that has gone bad while idle is discovered
by the first request to fail on it.

---

## Operational

### 🟡 No Helm chart

`deploy/docker-compose.yml` runs two gateway instances against one Redis, which
is the arrangement every shared limit is written for and the one a single
container cannot exercise. There are still no Kubernetes manifests, and no
readiness/liveness wiring beyond the endpoints themselves — the compose file
deliberately declares no container healthcheck, because the runtime image is
distroless and the only command available to it proves the binary runs rather
than that it serves.

### 🔴 No structured audit log

Access logs carry the key alias, model and outcome, but there is no separate
tamper-evident audit stream. The admin console widens this: minting, blocking
and deleting a key are now things that happen from a browser, and the only
record is an ordinary log line saying a key changed — not who was signed in when
it did. Every console session is the master key, so there is nobody to name yet;
that changes the day it signs in through the identity provider.

### ✅ Per-key rate limits were enforced per instance

Resolved. `keys[].rpm_limit` and `tpm_limit` went through a process-local
counter while the deployment limits beside them were shared, so a fleet handed
every key its allowance once per replica — the exact arithmetic
[observability.md](observability.md#what-is-shared-and-what-is-not) promised
Redis prevented. `auth.KeyLimiter` is now the seam, `rstate.KeyLimiter` the
shared implementation, and the two counters live in separate keyspaces so a key
hash and a deployment id cannot draw on one window.

### ✅ Metrics have no build info

Resolved. `gateway_build_info` carries the version and the active routing
strategy as labels.

### 🟡 Only the metrics signal is exported

`observability.otlp` pushes metrics over OTLP/HTTP. Traces and logs are not
exported: each would be another schema and another exporter, and the gateway
already emits structured logs carrying a request ID that a collector can
correlate on. A span per request, with the retry and fallback hops as children,
is the piece that would actually add something metrics cannot say.

### 🟡 A permanently unreachable deployment is never ejected

Connection errors and timeouts deliberately do not count toward cooldown — see
[routing.md](routing.md#cooldowns). The cost is that a deployment whose host has
gone away stays in the rotation indefinitely, and every request to its group
keeps paying a wasted attempt plus backoff. `gateway_deployment_available` makes
the state visible; it does not act on it. A separate active health check, rather
than a change to what counts as a failure, is the shape of the fix.

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
| A scope's budget | a subject in the ledger | LiteLLM derives a team's spend from its members, which changes underneath history when someone leaves the team |
| A dangling scope reference | refuses the key | the key store outlives the config that produced it, so treating a renamed team as "unscoped" would silently drop the cap its members were issued under |
| Model allowlists down a hierarchy | intersected, and a child widening its parent is refused at load | a grant that can never take effect is a statement the operator believes they made |
| A pooled budget refusal | names the level that bound, not the figures | "this key has exhausted its budget" is false when a team's pool ran out, and sends the developer to ask the wrong person |
| A model refused by a scope | names the level that withheld it | a caller whose own key lists the model has nowhere to go otherwise; widening the key's allowlist changes nothing |
| Reserving a chain of limits | one atomic operation | reserving level by level charges the inner windows for requests an outer one refused |

---

## Recording new work

Add an entry with a status marker, say what breaks without it, and note the
seam it would attach to. If something is a deliberate non-goal, mark it ⚪ and
say why — the reasoning is the part that gets lost.
