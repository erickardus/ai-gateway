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
| Passthrough cost | not billed to the operator | it is billed to the caller's subscription |

---

## Recording new work

Add an entry with a status marker, say what breaks without it, and note the
seam it would attach to. If something is a deliberate non-goal, mark it ⚪ and
say why — the reasoning is the part that gets lost.
