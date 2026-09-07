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

If your gateway has an identity provider configured, skip the rest of this
section: `gateway login` writes all of it, and the developer never handles a
key. See **[sso.md](sso.md)**.

Otherwise `gateway setup` writes it for you. It asks the gateway which model
groups it serves, offers them, and writes the result into the project's
`.claude/settings.json`:

```bash
gateway setup                      # picks up GATEWAY_KEY, or the key already written
gateway setup -yes -model kimi-k3  # same thing without the prompts
```

It writes a **project** directory rather than `~/.claude`, because a model
pinned at user level applies to every checkout on the machine. Run it again to
change models; it reuses the key already in the file, so the credential does not
pass through a shell history twice.

The manual form follows, and is worth reading once even if you use the command —
the third variable in particular is not guessable.

In `.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4000",
    "ANTHROPIC_MODEL": "anthropic-claude",
    "ANTHROPIC_CUSTOM_HEADERS": "x-gateway-key: sk-vk-your-key-here"
  }
}
```

**`ANTHROPIC_MODEL` alone does not make a gateway model *selectable*.** Claude
Code resolves a model name before using it, and a name it does not recognise —
which is every name that does not begin `claude-` — is not an error: it falls
back to the saved default *silently*, and answers arrive from a model nobody
chose. `ANTHROPIC_CUSTOM_MODEL_OPTION` registers one such name so that `/model`
will resolve it, which is what makes switching away and back possible:

```json
{
  "env": {
    "ANTHROPIC_CUSTOM_MODEL_OPTION": "kimi-k3-anthropic",
    "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME": "Kimi K3",
    "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION": "Kimi K3 through the gateway"
  }
}
```

There is one slot, so one gateway model can be selectable at a time. Note also
that Claude Code sends dated ids for some models — `claude-haiku-4-5-20251001`
for Haiku 4.5 — while sending current-generation ones bare. A group is an exact
map key with no wildcards, so declare whichever spelling your client sends.

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

Note that `apiKeyHelper` is the one Anthropic's own gateway documentation
suggests when the credential "rotates or comes from a vault or SSO command" —
its value is sent in **both** `Authorization` and `x-api-key`, so following that
advice here would end the subscription. [sso.md](sso.md) is the version of that
idea that works: the credential still comes from an SSO command, but the command
writes `ANTHROPIC_CUSTOM_HEADERS` rather than being read as a credential.

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

## Reaching GPT and other OpenAI-format models from Claude Code

Claude Code speaks the Anthropic Messages API and nothing else. Left alone, that
means the only upstreams it can reach through the gateway are the
Anthropic-compatible ones — a GPT deployment is configured, reachable by any
OpenAI client, and invisible to the editor.

Cross-format translation removes that wall. Switch it on:

```yaml
router:
  translation:
    enabled: true

model_list:
  # Unchanged: the subscription deployment from the setup above.
  - model_name: anthropic-claude
    params: {format: anthropic, api_base: https://api.anthropic.com, model: anthropic-claude, auth_mode: passthrough}

  - model_name: openai-gpt
    params:
      format: openai
      api_base: https://api.openai.com/v1
      model: gpt-5
      auth_mode: api_key
      auth_header: authorization
      auth_scheme: Bearer
      api_key: ${OPENAI_API_KEY}
      # OpenAI's reasoning models refuse the older max_tokens spelling.
      max_completion_tokens: true
    cost: {input_per_1m: 1.25, output_per_1m: 10, cache_read_per_1m: 0.125}
```

Then map Claude Code's own model aliases onto the gateway's group names, so
`/model` switches vendor:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4000",
    "ANTHROPIC_MODEL": "anthropic-claude",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "anthropic-claude",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "anthropic-claude",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "openai-gpt",
    "ANTHROPIC_CUSTOM_HEADERS": "x-gateway-key: sk-vk-your-key-here"
  }
}
```

Now `/model` inside a session moves between them. The alias names still say
"opus" and "haiku" — Claude Code's picker is built around them, and the gateway
cannot rename it — so choose which alias points at which group with that in
mind.

### What changes when you switch to a translated model

- **The subscription does not follow you.** Only the `passthrough` deployment
  bills your claude.ai plan. A translated request is never sent to a passthrough
  deployment, so `openai-gpt` traffic is billed per token to the server-side key
  in that deployment's config. This is a property of the credential, not a
  limitation of the translator: your subscription token is Anthropic's and
  OpenAI would not accept it.
- **The context meter may go quiet.** Claude Code measures context with
  `/v1/messages/count_tokens`, and there is no OpenAI endpoint that measures a
  prompt without running the model. On a group holding only `openai`
  deployments the gateway answers `503` rather than inventing a number you would
  then trim conversations against. Put one `anthropic` deployment in the same
  group and counting works again, because the request routes to it.
- **Some capabilities degrade quietly.** Extended thinking becomes a
  `reasoning_effort` band and its returned reasoning is unsigned, so it does not
  carry into the next turn. `top_k` is dropped. Images inside a tool result are
  reduced to their text. The full list is in
  [architecture.md](architecture.md#cross-format-translation-is-opt-in-and-says-what-it-costs),
  and the gateway logs the ones your fleet can produce at startup.
- **A translated reply says so.** It carries
  `x-gateway-translated: openai->anthropic`, which is the only thing that
  distinguishes it from a native one.

### Mixing both in one group

A group may hold deployments of both formats once translation is on, which is
how a fallback crosses vendors:

```yaml
model_list:
  - model_name: coding
    params: {format: anthropic, api_base: https://api.anthropic.com, model: claude-sonnet-4-5-20250929, auth_mode: api_key, auth_header: x-api-key, api_key: ${ANTHROPIC_API_KEY}}
    weight: 9
  - model_name: coding
    params: {format: openai, api_base: https://api.openai.com/v1, model: gpt-5, auth_mode: api_key, auth_header: authorization, auth_scheme: Bearer, api_key: ${OPENAI_API_KEY}, max_completion_tokens: true}
    weight: 1
```

Claude Code asking for `coding` reaches either, and an Anthropic outage no
longer stops the session. A `passthrough` deployment can sit in such a group
too — it simply keeps serving the Anthropic callers and is passed over by
everyone else.

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
