# Routing Claude Code through the gateway — while keeping your subscription

This is the gateway's headline use case: Claude Code's traffic flows through
your gateway, so you get routing, failover, rate limits and per-developer keys,
**and** Claude Code keeps billing against your claude.ai Pro/Max subscription
rather than per-token API credits.

It works because Anthropic supports it. From
[Other LLM gateways](https://code.claude.com/docs/en/llm-gateway):

> `ANTHROPIC_BASE_URL` is the variable that points Claude Code at the gateway.
> Setting only that variable, without a gateway credential, doesn't replace the
> subscription. Requests still route through the gateway, but a saved claude.ai
> login remains the active credential, so its usage limits and billing apply.
> Gateways that pass this traffic on to Anthropic must forward the OAuth
> capability in `anthropic-beta`.

## The two credential planes

Every request through the gateway carries two independent credentials:

```
  Claude Code                    ai-gateway                 api.anthropic.com
      │                               │                             │
      │  Authorization: Bearer        │                             │
      │    sk-ant-oat01-…  ───────────┼──── relayed verbatim ──────►│  subscription
      │    (your subscription)        │                             │  recognised
      │                               │                             │
      │  x-gateway-key: sk-vk-…  ────►│  consumed here.             │
      │    (issued by the gateway)    │  Never forwarded upstream.  │
      │                               │                             │
      │  anthropic-beta:              │                             │
      │    oauth-2025-04-20  ─────────┼──── forwarded verbatim ────►│  required,
      │                               │                             │  or 401
```

- **Client plane** — who is calling *the gateway*. Your virtual key, in a custom
  header, used for authentication, model allowlists and rate limits.
- **Upstream plane** — how the gateway calls *the provider*. In `passthrough`
  mode it relays your own credential untouched; in `api_key` mode it substitutes
  a server-side key.

The key rides a **custom header**, never `Authorization`, precisely so it sits
*beside* your subscription token instead of replacing it.

## Setup

### 1. Configure a passthrough deployment

```yaml
model_list:
  - model_name: anthropic-claude
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      model: anthropic-claude     # equal to model_name ⇒ bodies forwarded byte for byte
      auth_mode: passthrough      # relay the caller's own credential
    weight: 10

virtual_keys:
  allowed_upstream_hosts:
    - api.anthropic.com           # required: passthrough relays a credential here
  keys:
    - key: ${DEV_KEY}
      alias: dev-laptop
      models: ["anthropic-claude"]
      allow_passthrough: true     # required for this key to use a passthrough deployment
```

### 2. Point Claude Code at the gateway

In `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4000",
    "ANTHROPIC_MODEL": "anthropic-claude",
    "ANTHROPIC_CUSTOM_HEADERS": "x-gateway-key: sk-vk-your-key-here"
  }
}
```

Or as shell exports:

```bash
export ANTHROPIC_BASE_URL="http://localhost:4000"
export ANTHROPIC_MODEL="anthropic-claude"
export ANTHROPIC_CUSTOM_HEADERS="x-gateway-key: sk-vk-your-key-here"
```

### 3. Log in with your subscription

```bash
claude
/login          # choose "Claude account with subscription"
```

### 4. Verify

Run `/status`. You want to see:

- ✅ a line showing the gateway base URL
- ✅ a login line naming your claude.ai account
- ❌ **no** auth-token or API-key line

An API-key line means the subscription has been displaced and you are being
billed per token.

## ⚠️ What breaks the subscription

**Setting either of these replaces your subscription login**, and the traffic is
billed per token to whoever owns the credential:

| Variable | Effect |
|---|---|
| `ANTHROPIC_API_KEY` | Sent as `x-api-key`. Used *instead of* your Pro/Max/Team subscription even while logged in. |
| `ANTHROPIC_AUTH_TOKEN` | Overwrites `Authorization`, destroying the OAuth token. |
| `apiKeyHelper` | Same effect as above. |

For the subscription path, set **only** `ANTHROPIC_BASE_URL`,
`ANTHROPIC_MODEL` and `ANTHROPIC_CUSTOM_HEADERS`. If a subscription login
stops being used, run `unset ANTHROPIC_API_KEY` and check for an
`apiKeyHelper` in your settings files.

## `ANTHROPIC_CUSTOM_HEADERS` format

One `Name: Value` pair per line. In a JSON settings file, separate pairs with
`\n` because JSON strings cannot span lines:

```json
{
  "env": {
    "ANTHROPIC_CUSTOM_HEADERS": "x-gateway-key: sk-vk-…\nx-team: platform"
  }
}
```

Requires **Claude Code v2.1.227 or later**.

A custom header replaces a built-in header of the same name, matched
case-insensitively. That is exactly why the virtual key must use a name Claude
Code does not already set — `x-gateway-key` is additive, whereas putting the key
in `Authorization` would clobber your subscription token.

## Known limitations

**Model discovery is skipped.** Claude Code skips `GET /v1/models` when its only
credential comes from `ANTHROPIC_CUSTOM_HEADERS`, which is exactly this setup. Set
`ANTHROPIC_MODEL` (and the `ANTHROPIC_DEFAULT_*_MODEL` variables if you want the
`sonnet` / `opus` / `haiku` aliases to resolve to gateway model names). The
gateway still serves `/v1/models` for other clients.

**Remote Control and MCP tool search.** Pointing `ANTHROPIC_BASE_URL` at a
non-Anthropic host disables Remote Control, and disables MCP tool search unless
you set `ENABLE_TOOL_SEARCH=true`.

## What the gateway guarantees

These are enforced by tests, not just intent:

- `Authorization` is relayed **byte for byte** in passthrough mode.
- `anthropic-beta` is forwarded **verbatim, as an open list**. It is never
  filtered against a known-values allowlist — new Claude Code releases add
  capability values, and a pinned list breaks the next one. Stripping the
  `oauth-2025-04-20` capability would 401 every subscription request.
- The virtual key is **never** forwarded upstream, and neither is the header
  that authenticated the caller.
- Request bodies are **never** unmarshalled and re-marshalled. Anthropic strips
  Claude Code's system-prompt attribution block positionally, and prompt cache
  keys depend on the body bytes.
- Responses **stream** with a flush per chunk. SSE ping events and comment lines
  are relayed, never filtered: they are the only traffic during long thinking
  pauses, and Claude Code aborts a stream silent for 300 seconds.
- Upstream errors are relayed **unmodified**. Claude Code matches on the
  upstream's own error wording to decide whether to retry with a capability
  disabled; rewrapping errors breaks that recovery.
- No credential is ever written to a log, an error message, or the key store.

## Security posture

The gateway is a **transparent relay, never a token store**. It does not
persist, cache or log your subscription token, and it never originates a request
with a relayed credential — it only forwards one that is already in flight.

Anthropic's credential policy restricts subscription OAuth tokens to Claude Code
and claude.ai. The supported pattern is genuine Claude Code traffic being
relayed by a gateway, which is what this does. Extracting a token and replaying
it from your own code is not supported and is blocked server-side.

Passthrough is defence-in-depth gated: it is opt-in per deployment
(`auth_mode: passthrough`), opt-in per key (`allow_passthrough: true`), and
restricted to hosts named in `allowed_upstream_hosts`, so a key cannot make the
gateway forward your credential to an arbitrary host.
