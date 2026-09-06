# ai-gateway

A LiteLLM-style AI gateway in Go 1.25. Load-balances across upstream
deployments, authenticates callers with virtual keys, and — the reason it
exists — lets **Claude Code keep using a claude.ai subscription login while its
traffic flows through the gateway**.

Standard library only, apart from a YAML parser and, when you run more than one
instance, a Redis client for shared limits and a Postgres driver for the shared
key store.

## Why

Point Claude Code at a gateway and you hit a credential collision: Claude Code
already puts its subscription OAuth token in `Authorization`. A gateway that
consumes that header for its own authentication destroys the subscription; one
that ignores it cannot tell who is calling.

This gateway separates the two. Your virtual key travels in a custom header
(`x-gateway-key`, injected via `ANTHROPIC_CUSTOM_HEADERS`), while each
deployment declares how to authenticate upstream:

- `auth_mode: passthrough` — relay the caller's own credential untouched, so the
  subscription keeps working.
- `auth_mode: api_key` — substitute a server-side provider key.

See **[docs/claude-code.md](docs/claude-code.md)** for the full setup.

## Quickstart

```bash
# Build
make build

# Configure
cp config/gateway.example.yaml config/gateway.yaml
export ANTHROPIC_API_KEY=sk-ant-api03-...      # for the api_key deployments
export OPENAI_API_KEY=sk-...                   # for the openai deployment
export GATEWAY_MASTER_KEY=sk-master-...        # enables /key/* endpoints
export DEV_KEY=sk-vk-...                       # a config-declared virtual key

# Run
./bin/gateway -config config/gateway.yaml
```

Mint a key at runtime instead:

```bash
curl -sX POST localhost:4000/key/generate \
  -H "x-gateway-key: $GATEWAY_MASTER_KEY" \
  -d '{"alias":"laptop","models":["anthropic-claude"],"allow_passthrough":true}'
```

Or let developers issue their own through your identity provider, so nobody
copies a key anywhere — see **[docs/sso.md](docs/sso.md)**:

```bash
gateway login --gateway http://localhost:4000
```

Put those keys in a team, and the team's budget is one pool its members share
rather than a copy each of them receives — see
**[docs/configuration.md](docs/configuration.md#rbac)**:

```yaml
rbac:
  organizations:
    - id: acme
      teams:
        - id: platform
          max_budget: 2000
          budget_duration: 720h
```

Watch what the gateway is doing from a browser, rather than from curl — see
**[docs/admin-ui.md](docs/admin-ui.md)**:

```yaml
ui:
  enabled: true
```

```bash
make ui && make build     # the console is a separate build step
```

Then point Claude Code at it:

```bash
export ANTHROPIC_BASE_URL="http://localhost:4000"
export ANTHROPIC_MODEL="anthropic-claude"
export ANTHROPIC_CUSTOM_HEADERS="x-gateway-key: sk-vk-..."
claude    # /login → "Claude account with subscription"
```

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /v1/messages` | Anthropic Messages API. Matched on path, so `?beta=true` routes correctly. |
| `POST /v1/messages/count_tokens` | Token counting. |
| `POST /v1/chat/completions` | OpenAI Chat Completions API. |
| `GET /v1/models` | Model discovery. Served directly — never redirects. |
| `HEAD /api/hello` | Connection-warming probe. |
| `GET /health` | Per-deployment status and the active strategy. Requires a key: it discloses upstream hosts. |
| `GET /health/liveliness`, `/health/readiness` | Probes, unauthenticated. |
| `GET /sso/login`, `GET /sso/callback`, `POST /sso/exchange`, `POST /sso/renew` | SSO login, when `sso.issuer` is configured. Issues a virtual key from an OpenID Connect identity. |
| `POST /key/generate`, `GET /key/info`, `GET /key/list`, `POST /key/update`, `POST /key/delete` | Key management, master-key only. `update` edits a key in place, so blocking or re-budgeting one keeps the hash its spend is addressed by. |
| `GET /spend/keys`, `GET /spend/scopes`, `GET /spend/deployments` | Usage and cost reports, master-key only. `/spend/scopes` reports each team's pooled spend. |
| `GET /metrics` | Prometheus metrics, when `observability.metrics` is on. The same metrics push to an OpenTelemetry collector when `observability.otlp.endpoint` is set. |
| `POST /cache/purge` | Empty the response cache, master-key only. |
| `GET /ui/`, `/ui/api/*` | The admin console, when `ui.enabled`. A browser session, never a virtual key. |

## Scope

v1 is the routing core plus virtual keys. Everything else is deferred but has an
interface waiting for it.

| Feature | v1 |
|---|---|
| Unified API surface (Anthropic + OpenAI native) | ✅ |
| Model groups — one name, many deployments | ✅ |
| Load balancing — weighted, least-busy, usage-, latency-based | ✅ |
| Retries with backoff | ✅ |
| Fallbacks — generic, context-window, content-policy | ✅ |
| Cooldowns with automatic recovery | ✅ |
| Rate limits — per deployment and per key | ✅ |
| Virtual keys — issue, revoke, model allowlists | ✅ |
| SSO — OIDC login issues a key and configures Claude Code | ✅ |
| JWT auth — verify the provider's own token on every request | ✅ |
| Organisations, teams and projects, with **shared** budgets and limits | ✅ |
| Credential isolation + subscription passthrough | ✅ |
| Streaming (SSE) | ✅ |
| Health checks, timeouts | ✅ |
| Spend tracking, cost attribution and budgets | ✅ |
| Response caching | ✅ |
| Prompt caching — prefix affinity, breakpoints, savings reporting | ✅ |
| Prompt-cache accounting for OpenAI-compatible providers | ✅ |
| Prometheus metrics — traffic, tokens, cost, cache, streaming, routing | ✅ |
| OpenTelemetry — OTLP/HTTP metrics export | ✅ |
| Guardrails | ⏳ |
| Cross-format translation (Anthropic ↔ OpenAI) | ⏳ deliberate |
| Multi-instance shared state (Redis) | ✅ |
| Multi-instance shared key store (Postgres) | ✅ |
| Admin UI — health, traffic, keys, spend, budgets | ✅ |
| MCP gateway | ⏳ |

There is **no cross-format translation** in v1: an Anthropic ingress routes only
to `anthropic` deployments, an OpenAI ingress only to `openai` ones. That is a
deliberate choice — Anthropic's gateway rules require forwarding request bodies
unchanged, and translation is the opposite of that. The reasoning is recorded in
[architecture.md](docs/architecture.md#there-is-no-cross-format-translation), and
[roadmap.md](docs/roadmap.md#-cross-format-translation) sizes what building it
would take.

## Documentation

- **[docs/architecture.md](docs/architecture.md)** — how it works and why
- **[docs/claude-code.md](docs/claude-code.md)** — subscription passthrough setup
- **[docs/sso.md](docs/sso.md)** — one command to sign a developer in and configure Claude Code
- **[docs/configuration.md](docs/configuration.md)** — every config key, endpoint and status code
- **[docs/routing.md](docs/routing.md)** — strategies, retries, cooldowns, fallbacks
- **[docs/admin-ui.md](docs/admin-ui.md)** — the operator console, and how its session stays out of the inference plane
- **[docs/observability.md](docs/observability.md)** — usage, cost, budgets, metrics, response caching
- **[docs/audit.md](docs/audit.md)** — the tamper-evident record of who administered what
- **[docs/prompt-caching.md](docs/prompt-caching.md)** — keeping the provider's prompt cache hittable behind a load balancer
- **[docs/roadmap.md](docs/roadmap.md)** — what is not built, and what is unverified

## Running more than one instance

Every limit the gateway enforces — a deployment's `rpm`, a key's `rpm_limit`, a
key's budget — is counted per process until several instances share one Redis.
`deploy/docker-compose.yml` wires up two gateways against one Redis so that
arrangement can actually be run:

```bash
cp config/gateway.example.yaml config/gateway.yaml   # then edit it
docker compose -f deploy/docker-compose.yml up --build
```

Keys need sharing too, and separately: set `virtual_keys.store.kind: postgres` so
every replica reads one set of keys, or a key issued on one instance is unusable
at the next. See
[docs/configuration.md](docs/configuration.md#choosing-a-key-store) for the store
kinds, and
[docs/observability.md](docs/observability.md#running-more-than-one-instance)
for what is shared and what stays local.

## Development

```bash
make test     # go test -race ./...
make vet
make cover
make fuzz     # fuzz the JSON splicer
make ui       # build the admin console into internal/ui/dist
make ui-dev   # serve it with hot reload against a gateway on :4000
make docker
```

`make build` deliberately does not depend on `make ui`, so a Go-only change does
not need node installed. A binary built without the console serves a page saying
which command to run; inference is unaffected.
