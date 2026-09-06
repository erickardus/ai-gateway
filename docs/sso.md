# SSO: a key per developer, with nothing to paste

Without this, onboarding a developer is: an operator calls `/key/generate`,
copies the `sk-vk-…` out of the response, sends it to them somehow, and they
paste it into `~/.claude/settings.json`. Manual at both ends, a long-lived
credential through a chat window, and a key tied to nothing — so offboarding
means remembering which hash belonged to whom.

With it:

```bash
gateway login --gateway https://llm-gateway.example.com
```

A browser opens, the developer's identity provider recognises the session they
already have, and Claude Code is configured. They never see the key.

## Why this is a command and not a web page

Claude Code has exactly one credential channel that does **not** displace a
claude.ai subscription login: `ANTHROPIC_CUSTOM_HEADERS`. That is the whole
reason [claude-code.md](claude-code.md) puts the virtual key in
`x-gateway-key`.

Every mechanism Claude Code offers for fetching a credential *dynamically*
writes the other headers:

| Mechanism | Header it writes | Usable here |
|---|---|---|
| `ANTHROPIC_API_KEY` | `x-api-key` | no — displaces the subscription |
| `ANTHROPIC_AUTH_TOKEN` | `Authorization` | no — overwrites the OAuth token |
| `apiKeyHelper` | **both** | no — and it is the one documented for "an SSO command" |
| `otelHeadersHelper` | OpenTelemetry only | not a credential channel |
| `ANTHROPIC_CUSTOM_HEADERS` | whatever you name | **yes**, and it is static |

So the only header that works is the one with no helper behind it. Something on
the developer's machine has to write it, and `gateway login` is that something.
An admin distributing `apiKeyHelper` through managed settings — which is what
Anthropic's own rollout guide suggests — would silently move every developer
onto per-token billing.

## Operator setup

### 1. Register the gateway with your provider

One OIDC application, one redirect URI: `https://your-gateway/sso/callback`.
The gateway is the only OIDC client — developers' machines register nothing —
so this is done once.

Request a client that can issue refresh tokens. Renewal spends one against the
provider, which is what makes disabling an account stop its renewals without
anyone touching the gateway.

### 2. Configure

```yaml
sso:
  issuer: https://example.okta.com
  client_id: ${SSO_CLIENT_ID}
  client_secret: ${SSO_CLIENT_SECRET}
  redirect_url: https://llm-gateway.example.com/sso/callback

  key_duration: 720h          # 30 days
  renew_within: 168h          #  7 days

  role_claim: groups
  roles:
    - match: platform-eng
      models: ["anthropic-claude"]
      allow_passthrough: true
      rpm_limit: 120
      max_budget: 200
      budget_duration: 720h

    - match: contractors
      models: ["anthropic-claude"]
      allow_passthrough: true
      max_budget: 25
      budget_duration: 720h
```

`allow_passthrough: true` is what lets the issued key reach a `passthrough`
deployment. Without it the subscription path is closed and the role can only
use `api_key` deployments, billed to you.

There is deliberately no `*` catch-all above. An identity matching no role is
refused, and a provider covering the whole company authenticates the whole
company. Add `- match: "*"` last only if that is what you mean.

### 3. Tell developers one thing

The gateway's address. Everything else — base URL, model name, header name —
comes back from the gateway in the login response and is written for them.

## What a developer runs

```bash
gateway login --gateway https://llm-gateway.example.com
```

which writes, into `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://llm-gateway.example.com",
    "ANTHROPIC_MODEL": "anthropic-claude",
    "ANTHROPIC_CUSTOM_HEADERS": "x-gateway-key: sk-vk-…"
  },
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/usr/local/bin/gateway login --renew --if-expiring"}]}
    ]
  }
}
```

Then, as always:

```bash
claude
/login          # "Claude account with subscription"
/status         # base URL and a claude.ai login line; no auth-token or API-key line
```

Other commands:

| Command | Effect |
|---|---|
| `gateway login` | Sign in again. Reuses the remembered gateway address. |
| `gateway login --renew` | Renew now, no browser. |
| `gateway login --renew --if-expiring` | What the hook runs. Silent unless the key is inside `renew_within`. |
| `gateway login --no-hook` | Skip installing the renewal hook. |
| `gateway logout` | Remove the settings and forget the refresh token. |

`login` refuses outright if the settings already set `ANTHROPIC_API_KEY`,
`ANTHROPIC_AUTH_TOKEN` or `apiKeyHelper`. A key written beside any of those
would work, and quietly bill the developer per token.

## How renewal stays invisible

`ANTHROPIC_CUSTOM_HEADERS` is read once, at startup. A hook that rewrote it
during startup would be racing the value the session already has.

So renewal happens **ahead** of expiry, not at it. With the defaults a key lives
30 days and the hook renews inside the last 7: the running session keeps the
value it read, the file is rewritten underneath it, and the new key applies at
the next launch. No session ever starts with an expired key, and the hook is a
no-op on all but a handful of starts.

A developer who does not run Claude Code for a month comes back to an expired
key and runs `gateway login` again. That is one browser round trip a month in
the worst case, and none in the normal one.

## What happens on renewal

1. The client sends the refresh token it stored.
2. The gateway spends it at the provider and verifies the ID token that comes
   back.
3. Roles are re-derived from the fresh claims — so someone moved out of a group
   loses access at their next renewal, not at their next expiry.
4. A new key is issued and the previous one for that person **on that machine**
   is retired.

Scoping retirement to the machine is why a second laptop gets its own key
instead of signing the first one out.

## Spend follows the person, not the key

An SSO key is reissued over its owner's life — every renewal, every re-login,
every new machine. If budgets were tracked per credential, a developer could
clear their own budget window by signing in again.

So SSO keys accumulate spend under `sso:<subject>`, and `/spend/keys` reports
one row per person rather than one per key. `core.Key.SpendSubject` is the whole
of that decision. Keys not issued by SSO are unaffected and still account per
key.

## Offboarding

| You want | Do |
|---|---|
| Cut access now | `POST /key/delete` with the key's hash, from `/key/list` |
| Cut access at the next renewal | Disable the account, or remove them from the group, at the provider |

Disabling at the provider is the one that scales; deletion is the one that is
immediate. Neither needs the other.

## Running more than one instance

A login leaves for the provider and comes back as a separate request, which
behind a load balancer may reach a different replica. Pending logins therefore
go to Redis whenever `redis.addr` is set, and the gateway says which it is using
at startup.

Unlike the rate limiters, this does not degrade to local state when Redis is
unreachable: a login cannot be completed against state another instance holds,
and pretending otherwise would turn a clear failure into an intermittent one.
Logins fail while Redis is down; traffic on keys already issued carries on.

## What the gateway verifies

Standard library only — no JOSE dependency. An ID token is accepted only if:

- it is signed with RSA or ECDSA against a key from the provider's published
  JWKS. `alg: none` is refused, and so is the HMAC family, which would make the
  provider's public key the verification secret
- `iss` matches the configured issuer, and the discovery document's own `iss`
  matched it before its endpoints were used
- `aud` contains this gateway's client id — a token minted for another
  application of the same provider is a valid token that says nothing here
- `exp` has not passed and `nbf`/`iat` are not in the future, within a minute
- `nonce` matches the login the gateway started

And on the client side of the flow:

- a `redirect_uri` must be loopback, checked by parsing the host as an IP.
  `127.0.0.1.evil.example.com` begins with a loopback address and belongs to
  someone else; the gateway sends a redeemable code to that address, so this is
  the check the flow rests on
- PKCE is `S256` only, and the code is single-use — spent even by a failed
  redemption, so it cannot be guessed at

## Known gaps

- **No device-authorization flow.** `gateway login` needs a browser on the same
  machine. Over SSH, run it locally and copy `~/.claude/settings.json`, or use a
  key from `/key/generate`.
- **The refresh token is a `0600` file**, not an OS keychain entry. It sits
  beside the virtual key it renews, at the same permissions, so it is not a new
  class of secret on the machine — but a keychain would be better.
- **No SAML.** It would mean hand-rolled XML signature verification, which is a
  large surface and a classic source of auth bypasses, or a dependency this
  project does not otherwise need.
- **Settings key order is not preserved.** The file is decoded, edited and
  re-encoded, so keys come back alphabetised. Content is preserved exactly;
  ordering is not.
