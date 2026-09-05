# ai-gateway

A LiteLLM-style AI gateway in Go 1.25. Load-balances across upstream
deployments, authenticates callers with virtual keys, and — the reason it
exists — lets **Claude Code keep using a claude.ai subscription login while its
traffic flows through the gateway**.

Standard library only, apart from a YAML parser and — when you run more than one
instance — a Redis client.

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
| `POST /key/generate`, `GET /key/info`, `GET /key/list`, `POST /key/delete` | Key management, master-key only. |
| `GET /spend/keys`, `GET /spend/deployments` | Usage and cost reports, master-key only. |
| `GET /metrics` | Prometheus metrics, when `observability.metrics` is on. |
| `POST /cache/purge` | Empty the response cache, master-key only. |

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
| Credential isolation + subscription passthrough | ✅ |
| Streaming (SSE) | ✅ |
| Health checks, timeouts | ✅ |
| Spend tracking, cost attribution and budgets | ✅ |
| Response caching | ✅ |
| Prompt caching — prefix affinity, breakpoints, savings reporting | ✅ |
| Prompt-cache accounting for OpenAI-compatible providers | ✅ |
| Guardrails | ⏳ |
| Cross-format translation (Anthropic ↔ OpenAI) | ⏳ deliberate |
| Multi-instance shared state (Redis) | ✅ |
| Admin UI, teams, MCP gateway | ⏳ |

There is **no cross-format translation** in v1: an Anthropic ingress routes only
to `anthropic` deployments, an OpenAI ingress only to `openai` ones. That is a
deliberate choice — Anthropic's gateway rules require forwarding request bodies
unchanged, and translation is the opposite of that.

## Documentation

- **[docs/architecture.md](docs/architecture.md)** — how it works and why
- **[docs/claude-code.md](docs/claude-code.md)** — subscription passthrough setup
- **[docs/configuration.md](docs/configuration.md)** — every config key, endpoint and status code
- **[docs/routing.md](docs/routing.md)** — strategies, retries, cooldowns, fallbacks
- **[docs/observability.md](docs/observability.md)** — usage, cost, budgets, metrics, response caching
- **[docs/prompt-caching.md](docs/prompt-caching.md)** — keeping the provider's prompt cache hittable behind a load balancer
- **[docs/roadmap.md](docs/roadmap.md)** — what is not built, and what is unverified

## Development

```bash
make test     # go test -race ./...
make vet
make cover
make fuzz     # fuzz the JSON splicer
make docker
```
