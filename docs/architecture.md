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
