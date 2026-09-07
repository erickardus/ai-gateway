# Architecture

What exists today, how a request flows through it, and why the load-bearing
decisions were made the way they were.

Roughly 6,700 lines of Go and 4,400 lines of tests, across 13 packages.
Dependencies: a YAML parser, and a Redis client used only when state is shared
across instances.

## The problem this is shaped around

A gateway sits between a client and a provider, and the client usually already
carries a credential. Claude Code with a claude.ai subscription puts an OAuth
token in `Authorization`. A gateway that consumes that header for its own
authentication destroys the subscription; one that ignores it cannot tell who is
calling.

Everything else follows from separating those two questions:

| Plane | Question | Carried in |
|---|---|---|
| **Client** | Who is calling *the gateway*? | `x-gateway-key`, a custom header |
| **Upstream** | How does the gateway call *the provider*? | the deployment's own key, **or** a relay of the caller's |

Each deployment declares an `auth_mode`: `api_key` substitutes a server-side
credential, `passthrough` relays the caller's own untouched. That single
distinction propagates surprisingly far — into cost attribution, cooldown
policy, and what may be cached — and each of those is called out below.

## Request path

```
  POST /v1/messages
        │
        ├─ 1  authenticate            extract the virtual key, discriminating by
        │                             prefix so sk-ant-* is never read as one
        ├─ 2  annotate                key alias + model onto the access log
        ├─ 3  read body               bounded, presized from Content-Length
        ├─ 4  peek                    model / stream / disable_fallbacks, without
        │                             materializing the document
        ├─ 5  authorize model         key allowlist, then group existence
        ├─ 6  cache lookup ───────────▶ hit: serve and return. No upstream, no
        │                                    limit charged, no spend recorded
        ├─ 7  budget check            refuse before incurring further cost
        ├─ 8  admit                   charge the key's rate limit
        ├─ 9  fingerprint prefix      hash the part of the body that survives a
        │                             turn, for prompt-cache affinity
        ├─ 10 route ──────────────────┐
        │     filter cooldowns        │  retry excludes already-failed
        │     filter passthrough      │  deployments; backoff only when
        │     filter format           │  nothing untried remains
        │     filter capacity         │
        │     prefer the prefix pin   │  a preference among survivors, never
        │     strategy picks          │  a constraint
        │     reserve capacity        │  fallback to another group once the
        │                             │  first is exhausted
        │       └─ 10a annotate       │  per deployment, at dispatch: a cache
        │                             │  breakpoint, a usage option, or nothing
        │                             │  at all for a passthrough upstream
        ├─ 11 relay ──────────────────┘  byte-for-byte, flushed per chunk
        ├─ 12 store in cache             complete successful responses only
        └─ 13 record                     spend + metrics, on a detached context
```

Steps 6 and 13 are where several subtle decisions live; see below.

## Packages

| Package | Responsibility |
|---|---|
| `core` | Value types, sentinel errors, credential-prefix rules, pricing. A leaf everything may import. |
| `config` | The object model, YAML loading with `${ENV}` expansion, and validation that reports every problem at once. |
| `jsonx` | Byte-preserving JSON inspection and editing. |
| `auth` | Virtual keys, the header-extraction rules, key storage. |
| `limiter` | Monotonic-window request and token counters. |
| `router` | Model groups, the four strategies, retries, cooldowns, fallbacks. |
| `provider` | Upstream transport, the header policy, the SSE relay, and the per-deployment body an attempt actually sends. |
| `server` | HTTP surface, middleware, and the single instrumentation point. |
| `spend` | Usage and cost ledger with windowed budgets. |
| `metrics` | Prometheus text exposition, hand-rolled. |
| `cache` | Response cache with per-key or shared scope. |
| `promptcache` | Fingerprints a request's cacheable prefix, and marks one where the caller marked none. About the *provider's* cache, not this gateway's. |
| `rstate` | Redis-backed routing state and ledger, with local fallback. |
| `testutil` | Shared test helpers. |

## Decisions worth knowing

### Request bodies are never re-serialized

Anthropic strips Claude Code's system-prompt attribution block *positionally*,
which only works if the `system` array arrives exactly as sent, and prompt cache
keys depend on the body bytes. So `jsonx` locates a value by scanning for its
extent and splices a replacement in, leaving every other byte identical. Nested
containers are stepped over rather than decoded, which is why a large request
costs 14 allocations rather than ~123,000.

Duplicate top-level keys are rejected outright. Parsers disagree about which
occurrence wins, so a body relying on one could be authorized against
`"model": "allowed"` and executed against a later `"model": "forbidden"`.

### The body is decided per deployment, not per request

The gateway annotates a request for its own benefit — a cache breakpoint so an
Anthropic upstream has something to cache, `stream_options.include_usage` so a
streamed OpenAI-compatible reply can be billed at all. Whether an upstream takes
one is a fact about that upstream, not about the request, so `provider.Request`
carries the body as it arrived plus an `Annotator`, and `Client.Do` derives what
one deployment receives at dispatch — beside the model rewrite, which was always
per deployment.

A passthrough deployment is therefore sent the caller's bytes untouched while an
API deployment in the same group gets its breakpoints. Fixing one body before
routing forced the opposite: injection alongside passthrough had to be refused
at load, so a gateway fronting both Claude Code and plain API callers could
cache for neither.

It also makes a refusal knowable. An upstream that answers `400` to an annotated
body is retried with the request as it arrived, and where that succeeds the
deployment is not annotated again — the round trip is paid once rather than on
every request. Refusal is recorded only on that succeeding branch, so a `400` the
caller earned, which fails both bodies, cannot switch the optimization off for
everyone else.

### Cross-format translation is opt-in, and says what it costs

By default an Anthropic ingress reaches only `anthropic` deployments and an
OpenAI ingress only `openai` ones. A model group may not mix formats and a
fallback may not cross them. Set `router.translation.enabled` and all three
restrictions lift: a request is rewritten for whichever upstream serves it, and
the reply is rewritten back.

The reason to have it is that a client speaks one format for its whole life.
Claude Code speaks the Anthropic Messages API and nothing else, so without
translation a developer pointed at the gateway can reach the Anthropic-compatible
half of a fleet and no more — switching to a GPT model mid-session is not a
configuration away, it is impossible. With it, one endpoint and one set of
virtual keys serve both halves, and the model aliases Claude Code already has
become the switch. It also gives an operator the one thing nothing else here
could provide: a fallback that crosses vendors, so an Anthropic outage has an
escape hatch.

The reason it is off by default is that translation is the opposite operation to
everything else on this path. Editing a body splices one value and leaves every
other byte identical; translating it parses the whole document, rebuilds it, and
takes ownership of the fidelity of every field forever — including the fields
that do not map.

Several do not, and none of them fails loudly. They produce a request that
succeeds and behaves worse, which is the failure class the rest of this gateway
is shaped to avoid. So they are enumerated rather than discovered: `translate.Loss`
returns them as sentences, and the gateway logs the ones this fleet's own
configuration can actually produce, once, at startup. What is dropped, and why:

| Not carried | Why, and what happens instead |
|---|---|
| `top_k` | Chat Completions has no equivalent. Dropped, so sampling differs |
| `thinking` → | Becomes `reasoning_effort` by budget band. The returned reasoning is unsigned, so a later turn replaying it has it dropped rather than refused |
| ← `reasoning_effort` | Becomes a thinking budget, and stands down entirely when `max_tokens` leaves no room for one — Anthropic refuses a budget below 1024 or one that crowds out the reply. Enabling it also drops `temperature` and `top_p`, which Anthropic refuses alongside thinking |
| `cache_control` | Dropped unless the deployment sets `supports_cache_control`. Most OpenAI-compatible servers answer `400` to an unknown member |
| `metadata`, `document` blocks | No counterpart. Dropped |
| `n`, `seed`, penalties, `logit_bias`, `logprobs` | No counterpart in the Messages API. Dropped |
| `response_format` | Including `json_schema`. Dropped, so a caller relying on structured output gets prose |
| Images in a tool result | An OpenAI `tool` message takes no image parts. Reduced to its text |

Two structural differences are carried rather than dropped, because leaving them
would fail the call outright. Anthropic holds tool results as blocks inside a
user message where OpenAI holds them as separate `role: "tool"` messages, so one
turn becomes several — and the tool messages are emitted first, because OpenAI
requires each to answer the assistant turn that called it. And the Messages API
refuses consecutive turns of the same role where Chat Completions permits them,
so adjacent same-role turns are merged; a run of parallel tool results, which is
exactly what produces them, lands in one turn.

Streaming is where the two grammars differ most. OpenAI emits a flat sequence of
deltas and says nothing about where one piece of content ends and the next
begins; Anthropic emits an explicitly bracketed structure, and a client that
receives a delta for a block that was never opened treats the stream as corrupt.
So the boundaries are inferred. Text and reasoning are relayed as they arrive —
Claude Code renders tokens as they land and aborts a stream silent for 300
seconds, so buffering a whole reply would turn every long generation into a
timeout. Tool calls are not: their arguments are accumulated per upstream index
and emitted as complete blocks at the end. OpenAI numbers its tool calls and is
free to interleave their fragments, while an Anthropic block once closed cannot
be reopened, so a translator streaming them live would have to either reopen a
closed block or route a fragment into the wrong one — and both produce a tool
call whose arguments are invalid JSON assembled from two different calls. The
progressive rendering of a tool call is the price; correct arguments are what it
buys.

Two things translation never touches:

**A passthrough deployment.** Its body must reach the upstream exactly as the
caller wrote it — Anthropic's gateway rules require it, and the endpoint strips
Claude Code's attribution block positionally — and its credential is the caller's
own subscription, so a rewritten body sent there would spend a person's personal
quota on a document they never wrote. Enforced in `router.candidates` and again
in `provider.prepare`, on the path that actually puts bytes on the wire. This is
also what lets one group hold both a subscription deployment for Claude Code and
a translated one for everyone else: the passthrough member is simply unreachable
from an ingress of the other format, which is a restriction rather than a hole.

**`/v1/messages/count_tokens`.** There is no OpenAI endpoint that measures a
prompt without running the model. A gateway that guessed would have Claude Code
trimming conversations against a number nobody computed, so the route is refused
instead.

Usage is the one field that is recomputed rather than mapped. Anthropic reports
an `input_tokens` that excludes both cache counters; an OpenAI-compatible
response reports a `prompt_tokens` that includes them. Copying the number across
would bill every cached token twice — once at the full input rate inside the
total, once at the cache rate beside it — so the cached parts are carved out of
the total on the way across, and clamped against the total the provider itself
reported so an upstream's arithmetic error cannot mint savings. The invariant is
checked against the gateway's own usage parser rather than a restatement of it,
in both directions, so the two cannot drift apart.

A translated reply carries `x-gateway-translated: openai->anthropic`. Nothing
else about it is distinguishable from a native reply, which is precisely what
makes a fidelity problem impossible to attribute from the client side without
it.

### Headers are forwarded as an open list

`anthropic-*` headers pass through unfiltered. Capability values arrive with new
Claude Code releases, and a gateway pinned to the values it knows today silently
breaks the next one — including the OAuth capability a subscription login
requires, whose absence 401s every request.

### Timeouts are split

`stream_timeout` bounds time to the **response headers**, enforced by a timer
that is stopped once they arrive. Attaching it to the request context would
bound reading the body too, severing a long completion mid-stream after the
status line had already been sent. A non-streaming request keeps the deadline
for the whole exchange, since nothing further is expected.

### Failures are attributed to whoever caused them

On a passthrough deployment the credential is the caller's own, so a 401, 403 or
429 describes that caller rather than the deployment. Counting those toward
ejection would let one developer's expired token cool down the shared upstream
for everyone — a cross-tenant denial of service reachable by any valid key.

### Accounting runs on a detached context

By the time a request is recorded, the response has been relayed and a client
that hung up leaves the request context cancelled. Recording on it would abandon
the write, letting anyone dodge a budget by disconnecting.

### Cost is attributed only where the operator pays it

A passthrough deployment bills the caller's own subscription. Those requests
record usage with **no cost**, never consume an operator budget, and pricing one
is rejected at load. `requests` and `billable_requests` are reported separately
so a key serving only subscription traffic is visibly busy but free — which a
bare cost of `0` could not distinguish from idle.

### Cache isolation is structural

The key hash is part of the cache key under the default per-key scope, so one
caller cannot be served another's completion. Sharing is opt-in, and refused
while any passthrough deployment exists.

### The provider's prompt cache is a routing input

A prompt cache lives on one upstream account, so load balancing a conversation
across deployments turns every cache read into a cache write — a premium instead
of a discount, with no error and no latency signal to notice it by. The router
pins a request's cacheable prefix to the deployment that served it, and yields
that pin to every health, permission and capacity filter. Affinity can cost some
balance; it can never cost a request. See
[prompt-caching.md](prompt-caching.md).

### Shared state is a deliberate subset

Rate limits, cooldowns and spend go to Redis because they must be global to be
correct. Latency and in-flight stay per-instance: latency measures *this
instance's* path to the upstream, and a shared in-flight counter would leak
permanently whenever an instance died mid-request.

## Extension points

Each is an interface with a working implementation, placed where a second one
was expected.

| Interface | Today | Designed for |
|---|---|---|
| `router.StateStore` | in-memory, Redis | any shared store |
| `router.Strategy` | four strategies | custom selection policy |
| `router.Executor` | HTTP transport | alternative transports, test doubles |
| `auth.KeyStore` | in-memory, file | Postgres, a secret manager |
| `spend.Store` | in-memory/file, Redis | a database with history |
| `cache.Cache` | LRU, Redis | any cache |

## Testing

Tests run under `-race` in CI, which also installs `redis-server` so the
integration tests cannot silently skip.

- **`testing/synctest`** for everything time-dependent — cooldown expiry, window
  rollover, retry backoff — under virtual time, with no sleeping.
- **Real Redis** rather than a mock, because the behaviour under test is largely
  Lua atomicity and expiry semantics, which a mock reimplements approximately.
- **A fuzz target** on the JSON splicer, run for 30s in CI.
- **Mutation checks** on the load-bearing guarantees: reverting a fix must fail
  its test. Several tests were rewritten after a mutation showed they passed
  with the bug still present.

## What has not been verified

**Nothing here has ever talked to a real LLM provider.** Every test runs against
`httptest` fakes, and the end-to-end runs used local fake upstreams. The design
turns on Anthropic accepting a relayed OAuth token plus `anthropic-beta`, and
that specific assertion is untested against the real endpoint.

[roadmap.md](roadmap.md) tracks that and everything else outstanding.
