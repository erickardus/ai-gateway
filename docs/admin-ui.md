# The admin console

A browser console for the operator's questions: what is the gateway doing, who
is signed in, what did they spend, and revoke this person. It is served from the
gateway's own binary at `/ui`, and it is off by default.

```yaml
ui:
  enabled: true
```

```bash
make ui && make build
./bin/gateway -config config/gateway.yaml   # then open http://localhost:4000/ui
```

Sign in with `virtual_keys.master_key`. Enabling the console without one is
refused at load: signing in means presenting it, so a console without a master
key is a sign-in page nobody can pass.

## Why a third credential plane

The gateway already juggles two credentials on every request — the caller's
virtual key, which authenticates them to the gateway, and the caller's own
provider credential, which may need to reach the upstream untouched. The console
adds a third: a browser session that says who is looking at the operator view.

Keeping the third apart from the first is the whole design of this feature. A
cookie that could authenticate `POST /v1/messages` would be a session riding
along on every inference request a page could be tricked into making — which is
the credential collision this gateway exists to avoid, wearing a different hat.

Two things enforce the separation, and they are independent:

- **The cookie is scoped to `Path=/ui`.** The browser itself will not attach it
  to `/v1/messages`, `/key/*`, `/spend/*` or anything else the gateway serves.
  This is why the console has its own API under `/ui/api` rather than reusing
  the master-key endpoints: an endpoint outside `/ui` is an endpoint the cookie
  cannot reach.
- **`auth.ExtractGatewayKey` reads headers only.** Nothing on the inference path
  can read a cookie even if one arrived.

There is a test that presents a valid session to `/key/list`, `/spend/keys`,
`/health` and `/v1/messages` and requires all four to refuse it.

## The session

The session is a signed token carrying its own expiry, not a row in a table.
Nothing is stored server-side and nothing has to be replicated — which matters
for the arrangement `deploy/docker-compose.yml` runs, several gateways behind
one load balancer, where a session in one process's memory would sign the
operator out on every second request. The signing key is derived from the master
key with HKDF, so every instance verifies every other's sessions while sharing
no state at all.

The cost of statelessness is that a session cannot be revoked before it expires.
That is why `session_ttl` defaults to twelve hours rather than a week, and why
rotating the master key invalidates every session at once: the derived signing
key changes with it.

The expiry is signed into the token rather than left to the cookie's `Max-Age`,
which is the browser's to honour. The signature is checked before the expiry is
read, because an expiry read from an unverified token is a number the client
chose.

Requests that change state must carry an `x-gateway-ui` header. `SameSite=Strict`
is the first defence and this is the second: SameSite is a browser policy, while
a header a cross-site form post or image tag cannot set is a property of the
request itself.

## What each page answers

**Overview** — deployment health, the routing strategy, prompt-cache affinity
state, and spend over the current window with the billable and passthrough
halves distinguished, over an hour of throughput, latency and outcomes. Composed
server-side in one request rather than stitched from `/health` and `/spend/*` in
the browser, because four round trips each observe a slightly different instant
and a page reporting twelve deployments healthy beside a total of eleven is
worse than a page that waits.

**Analytics** — the same buffer the Traffic page lists row by row, aggregated
into contiguous time buckets: requests stacked by how they ended, latency p50
and p95 over time, token composition, and the same three dimensions — group,
deployment, key — ranked beneath. The gateway does the bucketing because summing
a few thousand records in the browser on every poll is work the process holding
them can do once.

Latency percentiles cover only requests that **actually called an upstream**. A
refusal is decided in microseconds and arrives in volume, and a cache hit is
recorded against the sentinel deployment `cache` — so a filter that merely asks
whether a deployment was named admits it. Counting either would make a gateway
look faster the more often it refused a key or was asked the same question
twice, which is the one thing a latency figure must not do.

**Traffic** — the last `request_log_size` requests this instance served, with
the routing decision each produced: retries, fallbacks, whether the prompt-prefix
pin was honoured, time to first token, and what it cost. This is where the two
things that make this gateway different — subscription passthrough and prefix
affinity — become visible; a counter can say a fallback happened, but only a
record says which key, to which group, away from which deployment.

**Keys** — every virtual key with its budget drawn against its cap, and a panel
grouping SSO-issued keys by the person who holds them. One developer signing in
from a laptop and a desktop holds two keys that share one spend subject, so
offboarding them means revoking both; a flat list makes that a search problem.

**Spend** — a daily trend over the last week, month or quarter, and the three
ledgers as tabs beneath it. A scope's row is the pool, not the sum of its keys:
a key can leave a team and its historical spend does not leave with it, so the
pool is its own ledger subject and is read as one.

The trend and the tables read different systems, and the page says so. The
tables read the ledger that enforces budgets and show the window it is enforcing
over, so a key whose window rolled over this morning shows as zero; the trend
reads the durable history, where a day keeps its figure afterwards. That is what
makes it a line rather than a number — and where no history is configured, the
panel says which setting turns one on rather than drawing an empty chart. See
[observability.md](observability.md#spend-history).

The trend is charted over deployments whichever tab is open. A request has one
deployment, so those buckets sum the money once; it is charged to every scope
above it as well, so a scope chart over every subject would draw an organisation
and its teams on top of each other and double the height of every bar.

**Audit** — the chain from `/ui/api/audit`, newest first, with its verification
state stated at the top. The log is a shipped feature the console was previously
blind to, which made the one question it exists to answer — who administered
what — a question only `jq` could answer.

A sink that cannot read back what it wrote says so instead of showing an empty
table: `audit.sink: stdout` hands its records to whatever collects this
process's logs and keeps nothing it can re-read. A file chain is re-verified on
each request, which is affordable because it holds one host's administrative
history; the Postgres chain is not, because verifying it walks a fleet's entire
history across a network and a page view is the wrong thing to pay that for. It
reports "not checked" rather than guessing, since `verified: false` beside no
reason reads as a broken chain.

**Routing** and **Organisations** — read-only. Both are declared in
`gateway.yaml` and resolved at load, and a console that edited them would be a
console rewriting the operator's configuration file behind their back.

Routing leads with a card per model group: the deployments behind it, which are
ready, and — the part invisible everywhere else — where its traffic goes when it
cannot be served. A request that fell back reports the deployment that answered,
not the group it was asked of, so the configured chain can only be read here,
before it is needed.

**Ops** — what this instance has enabled, shared-state health, where each store
persists, and the one lever the console offers: purging the response cache. The
targets it names are redacted to a host and database: a console is a place a
connection string with a password in it must never be readable off the screen.

## The traffic buffer

`request_log_size` bounds an in-memory ring of completed requests. Three things
about it are deliberate:

- **It holds no bodies.** The gateway relays those bytes without materializing
  them, and a debugging convenience is not a good enough reason to start keeping
  other people's prompts in a buffer an admin page reads.
- **It is per process.** Behind a load balancer each instance holds only what it
  served. The console says so rather than implying it is showing the fleet.
- **Unauthenticated refusals are counted but not kept.** They are the one
  rejection decided before any credential is verified, so they are reachable by
  anyone who can open a socket — and recording them would let an anonymous
  client evict the whole buffer in well under a second. The rejection counter
  still tallies them by reason, so the signal survives; only the per-request
  record does not.

Every other refusal *is* kept, because a budget exhausted or a blocked key is
exactly what an operator opens this page to find, and the counter cannot say
whose.

## Development

```bash
make ui-dev    # Vite on :5173, proxying /ui/api to a gateway on :4000
```

The console is a Vite + React + TypeScript app in `web/`, built into
`internal/ui/dist` and embedded with `go:embed`. `make build` does not depend on
`make ui`, so a Go-only change needs no node toolchain; a binary built that way
serves a page naming the command to run, and proxies inference normally.

The repository tracks only a `.gitkeep` under `internal/ui/dist`, which is what
keeps `go:embed all:dist` compiling on a clean checkout. The Vite build writes it
back after emptying the directory.
