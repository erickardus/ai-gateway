# Prompt caching

Anthropic keeps a cache of a request's leading tokens. A request whose prefix —
tools, system blocks, earlier turns — matches one already cached is billed at
roughly a tenth of the input price and starts generating sooner. Writing that
cache costs a premium, so the arithmetic only works if reads follow writes.

This is not the same thing as the response cache in
[observability.md](observability.md#response-caching). That one answers an
identical request without calling the provider at all. This one is the
**provider's** cache, and the gateway's job is to keep it hittable.

| | Response cache | Prompt cache |
|---|---|---|
| Who stores it | this gateway | the provider |
| What it stores | a whole response | a tokenized request prefix |
| A hit means | no upstream call at all | a cheaper, faster upstream call |
| Configured under | `cache:` | `prompt_cache:` |

---

## The problem a gateway creates

A prompt cache lives on **one upstream account**. Load balancing is the act of
spreading requests across several. Put the two together with no further thought
and every turn of a conversation lands somewhere new, so every request pays a
cache *write* — a premium — where it should have paid a *read*.

The failure is quiet. Nothing errors, latency looks normal, and the only symptom
is a bill several times larger than expected. It gets worse the better the load
balancing is.

## Prefix affinity

```yaml
prompt_cache:
  affinity: true         # the default
  affinity_ttl: 5m
```

The gateway fingerprints the part of a request that stays byte-identical as a
conversation grows — the tool definitions, the system blocks and the opening
turns — and pins that fingerprint to whichever deployment served it. The next
request carrying the same prefix goes back to the same upstream, and hits the
cache it warmed.

Trailing messages are deliberately excluded: they change on every turn, and a
fingerprint that included them would be a different fingerprint each time, which
is the same as having none.

**A pin is a preference, never a constraint.** The pinned deployment is filtered
for health, format, permissions and capacity exactly like every other candidate.
If it is cooling down, at its rate limit, or has already failed this request,
selection carries on as though there were no pin at all. Affinity can cost you
some balance; it can never cost you a request.

The pin's TTL is refreshed on every success, so it lives as long as the
conversation is active and lapses once it stops — the same lifetime the upstream
gives the cache entry itself. The default matches Anthropic's ephemeral cache.

### What it does not do

- **Nothing, in a group with one deployment.** There is nothing to choose
  between, so the fingerprint is not even computed. `/health` reports affinity
  as inactive with a note saying why, rather than claiming to be doing something
  it is not.
- **Nothing across model groups.** Pins are scoped by group, so a fallback hop
  never inherits a pin for an upstream that cannot serve it.

### More than one instance

Pins live in Redis when [shared state](observability.md#running-more-than-one-instance)
is configured, unlike latency and in-flight counts, which stay local. The reason
is the same one that makes the feature worth having: behind a load balancer the
next turn of a conversation arrives at a different replica, and a per-instance
pin would send it to a different upstream — precisely the thing the pin exists
to prevent.

With Redis unreachable, affinity degrades to no pin at all: more cache writes,
never incorrect routing.

### What travels

A fingerprint is a SHA-256 digest. Prompt text never reaches routing state, a
log line, or Redis.

---

## Cache breakpoints

Anthropic caches a prefix only where the request marks one with `cache_control`.
Claude Code marks its own; a plain API caller often marks none, and gets no
caching however well it is routed.

```yaml
prompt_cache:
  inject: true           # off by default
  inject_min_bytes: 4096
```

With this on, the gateway places an ephemeral breakpoint at the end of the tool
definitions and at the end of the system prompt — together, the part of a
request that does not change between turns. It never marks a trailing message: a
breakpoint there is rewritten every turn, buying one cache write for every cache
read.

It is skipped entirely when:

- the body **already carries a `cache_control`** anywhere. A caller that placed
  its own knows where its prompt repeats, and the API caps how many breakpoints
  one request may hold.
- the prefix is **shorter than `inject_min_bytes`**. Anthropic ignores a
  breakpoint below its own minimum rather than rejecting it, so this is an
  economy, not a correctness rule.
- the request is **not in the Anthropic format**. Breakpoints are an Anthropic
  construct.

A string `system` prompt has nowhere to hang a breakpoint, so it is promoted to
the single text block the API defines it to equal. The original string's bytes
are reused verbatim rather than decoded and re-encoded.

### Why it is refused alongside passthrough

**Configuration fails at load if `inject` is on while any passthrough deployment
exists.** Injection edits the request body. The passthrough path exists to
forward a body unchanged: Anthropic's gateway rules require it, and the endpoint
strips Claude Code's system-prompt attribution block positionally, which only
works when the array arrives exactly as sent.

This is a real limitation, not a technicality — a gateway fronting both Claude
Code and plain API callers cannot use injection today. It is refused at load
rather than left to surprise an operator whose subscription traffic quietly
starts being rewritten. [roadmap.md](roadmap.md) records the seam that would
lift it.

Everything the gateway does edit, it edits by splicing rather than re-encoding.
A body run through a JSON decoder and back reorders object keys and renormalizes
numbers — which changes the very prefix bytes the upstream cache keys on.

---

## Seeing whether it works

Per response:

| Header | Meaning |
|---|---|
| `x-gateway-prompt-affinity: hit` | served by the deployment holding this prefix |
| `x-gateway-prompt-affinity: miss` | a pin was consulted, something else served it |
| absent | no pin was in play |

In `/metrics`:

| Metric | Labels | Answers |
|---|---|---|
| `gateway_prompt_cache_tokens_total` | model, deployment, outcome=`read`\|`write` | is caching saving money |
| `gateway_prompt_cache_requests_total` | model, deployment, outcome=`hit`\|`miss` | is caching working |
| `gateway_prompt_affinity_total` | model, deployment, outcome=`hit`\|`miss` | is routing keeping it working |

The three are separate on purpose. Read/write tokens show the money. Hit rate
shows whether prompts are being cached at all. Affinity shows whether the
router, rather than the prompts, is the reason they are not.

A useful alert is a falling `gateway_prompt_affinity_total{outcome="hit"}` share
while traffic is steady: something has started scattering conversations, and the
bill will follow.

Requests that never reached an upstream — rejected, or served from the response
cache — are excluded from the hit rate. They gave the provider no prompt to
cache, and counting them as misses would understate it.

In `/spend`:

```json
{
  "entries": [{"subject": "…", "cost": 4.21, "cache_savings": 11.87}],
  "total_cost": 4.21,
  "total_cache_savings": 11.87
}
```

`cache_savings` is what the prompt cache took off the bill, against the same
tokens charged as ordinary input. It sits **beside** cost rather than inside it:
cost is what was charged, and this is what was not. It needs the price of the
deployment that actually served each request, which is why it is recorded here
rather than inferred from a hit rate on a dashboard.

Passthrough traffic reports no savings, for the same reason it reports no cost:
the caller's own subscription was billed, not the operator.

---

## Cost model

Prompt-cache pricing is not optional detail on a deployment:

```yaml
cost:
  input_per_1m: 3.00
  output_per_1m: 15.00
  cache_read_per_1m: 0.30
  cache_write_per_1m: 3.75
```

Claude Code leans on prompt caching heavily, so a cost model using only input
and output is wrong by a wide margin on exactly the traffic this gateway exists
to carry. The gateway reads Anthropic's `cache_read_input_tokens` and
`cache_creation_input_tokens` from the response, including from the
`message_start` event of a streamed reply.
