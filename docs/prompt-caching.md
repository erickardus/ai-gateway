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
  affinity: true                       # the default
  affinity_ttl: 5m
  affinity_max_in_flight_lead: 4
```

The gateway fingerprints the part of a request that stays byte-identical as a
conversation grows — the tool definitions, the system blocks and the opening
turns — and pins that fingerprint to whichever deployment served it. The next
request carrying the same prefix goes back to the same upstream, and hits the
cache it warmed.

Trailing messages are deliberately excluded: they change on every turn, and a
fingerprint that included them would be a different fingerprint each time, which
is the same as having none.

**Cache breakpoints are excluded too**, for the same reason and less obviously.
A `cache_control` marker says where a prefix ends; it is not part of the prompt
an upstream tokenizes, and a client that walks its marker forward as the
conversation grows — Claude Code does, and so does the automatic caching in the
SDKs — is still sending the prefix the previous turn warmed. Fingerprinting the
raw bytes would read that moving marker as a new prefix on every turn: the pin
would be rebuilt each time, and the deployment holding the warm cache would be
one candidate among many. So the fingerprint is taken over the request with
every `cache_control` member removed, which the structural walk under
[injection](#cache-breakpoints) already knows how to distinguish from prompt
text that merely mentions one.

How much of the conversation joins the fingerprint has to be a count the *first*
request already satisfies. Under Anthropic the opening user turn is distinctive
on its own, so it is the only message read; reading two would mean a
conversation's first request — which has one message where every later one has
three — fingerprinting differently from its own second turn, and warming an
upstream that nothing afterwards had a reason to return to.

Under OpenAI the first message is usually a system prompt many callers share, so
reading it alone would put unrelated conversations on one pin, and the second —
the first user turn — is what tells them apart. That holds only where the system
message is there: a caller that sends none has a first request carrying one
message where its second carries three, which is the instability above. So the
second message joins the fingerprint only behind a leading `system` or
`developer` message, which is present from a conversation's first request or not
at all.

Two requests sharing everything that is fingerprinted share a pin even where
they diverge later on. That is not a loss of precision: they carry the same
prefix, so they belong on the same upstream, and it is
[bounded](#the-bound-on-concentration) like any other concentration.

**A pin is a preference, never a constraint.** The pinned deployment is filtered
for health, format, permissions and capacity exactly like every other candidate.
If it is cooling down, at its rate limit, or has already failed this request,
selection carries on as though there were no pin at all. Affinity can cost you
some balance; it can never cost you a request.

### The bound on concentration

A fingerprint covers a *prefix*, not a conversation. Traffic that shares a
system prompt, its tools and its opening turns shares one pin — a templated
single-turn caller, for instance — and with nothing else in play every request
of it would land on one deployment while the rest of the group sat idle.

`affinity_max_in_flight_lead` bounds that. A pin is passed over once the pinned
deployment is carrying that many more in-flight requests than the least busy
deployment that could serve the request instead.

It is a **load comparison rather than a share quota**, because concentration is
only a problem when there is contention. One busy conversation pinned to one
deployment while its peers are idle is the feature working; diverting it would
buy a cache write and nothing else. So the bound does not fire until the pinned
deployment is genuinely ahead of somewhere else the request could go.

The default of 4 is deliberately loose. A handful of concurrent Claude Code
sessions never reaches it. Traffic that has collapsed onto one deployment passes
it almost immediately, and each request that yields lowers the lead, so the group
settles with most requests still reaching a warm cache rather than swinging to an
even split.

A yielded request re-pins to whoever served it — that deployment now holds the
prefix, so it is where the next request should go. Under sustained overload the
pin will move between deployments, and each move costs one cache write. That is
the trade the bound exists to make.

In-flight is per-instance even where the rest of routing state is shared, so this
costs no round trip: it reads the load this instance is carrying, which is also
the load it is in a position to redistribute. Set the lead to `0` to yield to any
idler peer at all; there is no value that disables the bound, because `affinity:
false` already does that.

The pin's TTL is refreshed on every success, so it lives as long as the
conversation is active and lapses once it stops — the same lifetime the upstream
gives the cache entry itself. `affinity_ttl` defaults to five minutes, matching
Anthropic's ephemeral cache.

### A pin follows the evidence that there is a cache

A pin says one deployment holds a warm copy of this prefix. Whether it does is
something only the response knows, so the pin is written after the reply has
been read rather than at the moment the upstream answers.

An Anthropic response says outright what its cache did, reporting the tokens it
served from an entry and the tokens it wrote into one. Silence there means
nothing was cached: the prompt sat below the provider's minimum, or the caller
sent no breakpoint and injection is off. Pinning that prefix would concentrate
its traffic on one deployment in exchange for nothing, so it is not pinned at
all, on any turn, until an upstream says otherwise.

An OpenAI-compatible provider says no such thing. Its caching is automatic and
its writes are free, so a first request reports no cached tokens whether or not
the prefix was stored — and requiring evidence there would mean never
establishing a first pin. Those are pinned on success, as is any response that
reported no usage at all: that is an accounting gap rather than evidence, and
losing cache hits is the more expensive way to be wrong.

Deferring costs a window, between dispatch and the end of the response, in which
a concurrent request carrying the same prefix sees no pin. A conversation cannot
race itself — its next turn is waiting on this reply — so the window belongs to
distinct callers sharing a prefix, which is the concentration case affinity
already bounds.

**Token counting is not pinned.** `/v1/messages/count_tokens` takes the same body
as an inference request and never reaches the model, so it warms nothing: a pin
taken from it names a deployment holding no warm prefix, and refreshing an
existing pin from it would keep a conversation pointed at an upstream on the
strength of a request that ran nothing. Claude Code counts tokens on most turns,
so this is the ordinary case rather than an edge. It consults no pin either, and
reports no affinity header.

**A caller using the one-hour cache gets a one-hour pin**, whatever
`affinity_ttl` says, because the pin is meant to expire with the entry it points
at rather than on a schedule of its own. The extended entry costs twice base
input to write against the default tier's 1.25x, and it survives gaps the
default would not: a conversation that pauses for ten minutes still has its
cache, and a five-minute pin would hand that turn back to the load balancer to
write the same prefix somewhere else at the same premium. It is the most
expensive cache miss the gateway can arrange, on the traffic that opted into the
most expensive writes.

The declared lifetime is read from the breakpoints on the tools and the system
prompt. Both are small and already walked to fingerprint the request, and the
API requires longer-lived entries to appear before shorter-lived ones, so a
one-hour breakpoint that coexists with any five-minute one is in one of them.
Scanning the message history for the remaining case would cost a walk over the
largest part of every request, which is the expense the fingerprint itself is
shaped to avoid.

### What it does not do

- **Nothing, in a group with one deployment.** There is nothing to choose
  between, so the fingerprint is not even computed. The question is asked per
  group rather than per fleet: a gateway fronting one balanced group and a dozen
  single-deployment ones would otherwise hash the prompt of every request to all
  thirteen. `/health` reports affinity as inactive, with a note saying why,
  where no group can use it.
- **Nothing across model groups.** Pins are scoped by group, so a fallback hop
  never inherits a pin for an upstream that cannot serve it.
- **Nothing about deployments that already share a cache.** A cache belongs to
  the workspace behind the credential and is scoped to one model at one
  endpoint, so two deployments in a group could only share one by naming the
  same endpoint and model — which is [refused at
  load](configuration.md) as a duplicate. Two spellings of one endpoint would
  slip through, and cost some balance rather than any cache hits.

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

## OpenAI-compatible deployments

Everything above applies to an `openai` deployment unchanged. The fingerprint
covers the same stable prefix, the pin is scoped by group the same way, and the
same headers and metrics report what happened. What differs is what the provider
does with it, and what it says afterwards.

| | Anthropic | OpenAI-compatible |
|---|---|---|
| How a prefix is cached | explicitly, at a `cache_control` breakpoint | usually automatically, above a minimum prefix length |
| What a write costs | a premium over input | usually nothing |
| Where the discount shows up | `cache_read_input_tokens` | `prompt_tokens_details.cached_tokens` |
| Whether input includes it | no | **yes** |

That last row is the one that costs money. Anthropic reports an `input_tokens`
that already excludes every cache counter, so the figures are simply added up at
their own prices. An OpenAI-compatible response reports a `prompt_tokens` that
**includes** them, and breaks them out beside it.

The two "usually" qualifiers are the second thing that costs money, and they are
why the counters are read from more than one place:

| Provider | Cached how | A read is reported as | A write |
|---|---|---|---|
| OpenAI | automatically | `prompt_tokens_details.cached_tokens` | free, unreported |
| Kimi (Moonshot) | automatically | the same | free, unreported |
| GLM (Z.ai) | automatically | the same | free, unreported |
| DeepSeek | automatically | `prompt_cache_hit_tokens`, top level | free, unreported |
| **Qwen (Alibaba)** | **explicitly, at a `cache_control` breakpoint** | the same | **charged**, nested in `prompt_tokens_details` |
| **MiniMax** | automatically | the same, and again as `cache_read_input_tokens` | **charged**, nested in `prompt_tokens_details` |

The last two rows are the ones a gateway gets wrong. They put their write
counters — and, for Qwen, an Anthropic-shaped `cache_creation` TTL breakdown —
*inside* the details object rather than at the top level of `usage` where
Anthropic puts them. Read only the top level and those writes are invisible;
they do not vanish, they stay inside `prompt_tokens` and are billed at the input
rate. On a request writing 2048 tokens of a 2059-token prompt that is a fifth
off the bill, and on a larger explicit cache it is more. It fails in the
expensive direction: a budget that should have stopped a key keeps letting it
spend, and `/spend` quietly disagrees with the invoice.

Read with Anthropic's rule, every cached token on an OpenAI deployment is
charged twice — once at the full input rate inside `prompt_tokens`, once at the
cache-read rate beside it. On a conversation with a 100k-token cached prefix
that is roughly four times the real cost, reported as confidently as the right
number would be. Ignore the field entirely, which is what a gateway does by
default, and the deployment has no prompt-cache accounting at all: its cheapest
tokens are billed as its most expensive, `cache_savings` reads zero forever, and
the hit-rate metric says caching is not working when it is.

So the gateway normalizes at the point of parsing, under the format of the
deployment that served the request:

```
InputTokens = prompt_tokens - cached_tokens - cache_creation_tokens   # openai
InputTokens = input_tokens                                           # anthropic
```

The invariant is that every prompt token is counted exactly once, in whichever
bucket the provider priced it in — the three buckets partition `prompt_tokens`
rather than overlapping it. MiniMax states the same identity from its own side,
reporting a `non_cached_input_tokens` that equals what this arithmetic produces.

Every name a counter is known by is read, in both placements, and **no two are
ever added together**: one figure under two names is still one figure, so the
larger is taken rather than the sum. That covers
`prompt_tokens_details.cached_tokens`, `input_tokens_details.cached_tokens`,
`cache_read_input_tokens`, DeepSeek's top-level `prompt_cache_hit_tokens`, and
`cache_creation_input_tokens` at either level.

Reads settle against the reported total first and writes take what is left, so a
provider claiming more cached and written tokens than it charged input for is
clamped rather than allowed to drive input negative or mint savings out of an
upstream's arithmetic error.

### Streamed replies report nothing unless asked

An OpenAI-compatible reply reports usage in the response body. A **streamed** one
does not: the chunks carry content and the stream ends, with no usage object
anywhere, unless the request set `stream_options.include_usage`. Anthropic has no
equivalent condition — it reports usage on `message_start` whether asked or not.

Missing usage is not a missing field on an otherwise correct bill. It is the
whole bill: the request records zero input, zero output and zero cached tokens,
so it costs nothing, charges nothing against a budget or a token limit, and
contributes nothing to the cache counters. On a deployment serving nothing but
streamed traffic — the normal case for a chat UI — every figure in `/spend`
reads zero, and `/metrics` reports that prompt caching is not working because it
cannot see it working. Nothing errors.

So the gateway asks, under `observability.stream_usage` (on by default):

```yaml
observability:
  stream_usage: true
```

It adds `stream_options: {"include_usage": true}` to a streamed request in the
OpenAI format that did not set `stream_options` itself. A caller that did has
said what it wants and is left alone, byte for byte.

What it costs is one extra chunk at the end of the stream, carrying the usage and
an empty `choices` array. That is part of the OpenAI protocol and the official
clients expect it, but a hand-written client that indexes `choices[0]` on every
chunk does not, and a strict OpenAI-compatible server may reject the field
outright. Either is a reason to turn it off — at the price of an unpriced
deployment.

Where a streamed reply still arrives with no usage — the setting is off, the
caller asked for none, or the upstream ignored the field — the gateway says so
once per deployment:

```
streamed reply reported no usage, so it is billed as nothing
  deployment=openai/gpt-4o remedy=set observability.stream_usage, or have the
  caller send stream_options.include_usage
```

It is a log line rather than an error because the response itself is fine. Only
the accounting is missing, and nothing else would ever mention it.

**Breakpoint injection does not apply.** `cache_control` is an Anthropic
construct, caching on an OpenAI-compatible provider is automatic, and there is
nothing for the gateway to mark. An `openai` deployment is simply never sent a
breakpoint, whatever `inject` says; configuration refuses the setting only where
*no* deployment in the fleet could carry one.

What the gateway does not yet do is pass OpenAI's `prompt_cache_key`, which
biases that provider's own cache routing for callers sending the same prefix at
high rates. It is a body rewrite, and body rewrites are constrained here for the
same reasons injection is; [roadmap.md](roadmap.md) records the seam.

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

With this on, the gateway marks three things.

**The end of the tool definitions and the end of the system prompt** get an
explicit ephemeral breakpoint. Together they are the part of a request that does
not change between turns, and each is a read point that survives whatever
happens later in the conversation.

**The conversation itself** is covered by the top-level `cache_control` field —
Anthropic's automatic caching, which places a breakpoint on the last cacheable
block and walks it forward as turns accumulate. This is the part the explicit
markers cannot reach. Render order is tools, then system, then messages, so a
breakpoint at the end of the system prompt caches everything before it and
nothing after; the messages are what grow. A caller with a modest system prompt
and a long history would re-read all of it at full price on every turn while
`/metrics` reported that caching was working.

It is the top-level field rather than a marker written onto the newest turn, but
not because that marker would be wasteful. A breakpoint moved forward over a
prefix an upstream already holds writes only the tokens past the previous one
and reads the rest, which is why every Anthropic client walks its own marker
forward and why [affinity](#prefix-affinity) is built to survive one doing it.
Marking the newest turn is a documented pattern, and the one other gateways
inject.

The reasons to prefer the top-level field here are narrower. It is a member of
the root object, so placing it edits the request beside the conversation rather
than splicing into the largest array in it. It leaves the fourth breakpoint
unspent. And it lands the gateway's marker nowhere: the last block of the newest
turn may be a tool result, an image, a document or a thinking block, and hanging
a breakpoint on a shape whose accepted form the gateway cannot check is the same
gamble it [declines to take](#cache-breakpoints) on a server tool — except that
here it would be taken on every request rather than on the ones with an
unfamiliar tool. Letting the API place that breakpoint itself costs nothing and
cannot be wrong.

That is three of the four breakpoints a request may hold. The two documented
ways of combining an explicit marker with the automatic one are both refused
with a 400 — all four slots taken, and an explicit marker on the last block
whose lifetime differs from the top-level field's — and neither can arise here:
injection runs only on a body carrying no breakpoint of its own, and every
marker it places has the same default lifetime.

Two things it deliberately does not mark:

- **A trailing message**, for the reason above.
- **A tool it does not recognize.** A custom tool declares an `input_schema`. A
  server tool — web search, code execution — is named by `type` alone, and an
  MCP toolset names a server; both are shapes the gateway cannot validate a
  breakpoint against, and hanging one there to find out costs a 400 on a request
  that would otherwise have been served. Skipping it costs nothing, which is
  what makes this cheap rather than a compromise: tools render before the system
  prompt, so the breakpoint at the end of the system prompt already caches every
  tool in the list whether or not the tools themselves carry one.

It is skipped entirely when:

- the body **already carries a `cache_control` member** anywhere. A caller that
  placed its own knows where its prompt repeats, and the API caps how many
  breakpoints one request may hold.

  The test is structural, not a substring search. Prompt text is JSON string
  data, and a conversation that discusses `cache_control` — a developer asking
  Claude Code about prompt caching, a diff that touches the gateway's own source
  — contains those bytes without carrying a breakpoint. Reading that as "the
  caller manages its own" would switch injection off for the rest of that
  conversation, and show up only as a larger bill nobody could trace back to a
  word in a message. The cheap byte scan is kept as a prefilter, so the walk
  that distinguishes a member name from prose runs only where the name appears
  at all.
- the prompt is **shorter than `inject_min_bytes`**. Anthropic ignores a
  breakpoint below its own minimum rather than rejecting it, so this is an
  economy, not a correctness rule. It is measured over the whole prompt — tools,
  system and messages — because that is what the automatic breakpoint caches;
  measuring only the static prefix would skip exactly the caller this is for,
  whose history is long and whose system prompt is not.

  The provider's own minimum is counted in tokens and differs by model: 1024 for
  the larger Claude models and 2048 for the smaller ones, where the default
  4096 bytes is roughly 1000 tokens. A group serving only a small model wants
  this raised to about `8192`; left at the default it marks prompts the provider
  will ignore, which costs nothing but buys nothing either. It is a byte count
  rather than a token count because counting tokens means running a tokenizer
  over every request, which is a real cost per request for a threshold that only
  decides whether an optimization is attempted.
- the request is **not in the Anthropic format**. Breakpoints are an Anthropic
  construct.

The top-level field additionally goes only to `/v1/messages`. Token counting
takes the same body and answers a different question, so asking it to cache the
conversation is at best inert.

### The body is decided per deployment

Everything above is something the gateway adds for its own benefit; the caller
asked for none of it. Whether an upstream will take it is a fact about that
upstream rather than about the request, so the decision is made once routing has
chosen one — at dispatch, beside the model rewrite that was always per
deployment.

| Deployment | What it is sent |
|---|---|
| passthrough | the caller's bytes, untouched |
| `anthropic`, api-key | breakpoints, per this page |
| `openai`, api-key | `stream_options.include_usage` on a streamed request |
| one that has refused an annotation | the caller's bytes, untouched |

**A passthrough deployment is never annotated.** It forwards the caller's own
credential to an upstream that bills their subscription, and its body must
arrive as they sent it: Anthropic's gateway rules require it, and the endpoint
strips Claude Code's system-prompt attribution block positionally, which only
works on an array that was not spliced.

Standing down per deployment rather than refusing at load is what lets one
gateway front both kinds of traffic. Injection alongside passthrough used to
fail configuration outright, because a single body fixed before routing had to
suit every deployment in the group — so an operator running Claude Code and
plain API callers through one gateway had to choose which half to serve
properly. Now the subscription traffic is forwarded verbatim and the API traffic
gets its breakpoints, in the same model group.

What configuration still refuses is `inject` on a fleet where **no** deployment
could carry a breakpoint — every deployment passthrough, or none of them
`anthropic`. That is a setting that says one thing and does nothing, which is
refused for the same reason [a partial cost model
is](configuration.md#a-partial-cost-model-is-refused-at-load).

### When an upstream refuses an annotation

Which shapes a breakpoint may legally hang on differs between providers and
changes over time — the legacy Bedrock integration, for one, rejects the
top-level field outright — so a wrong guess must not cost the request.

An upstream answering `400` to an annotated body gets one more attempt with the
request exactly as it arrived. If that is accepted, the annotation was the cause:
the deployment is named once in the log and **is not annotated again**.

```
upstream rejected an annotated request, so this deployment will not be annotated
again
  deployment=… cost=one round trip, paid once per deployment per process
```

Remembering is the point. The retry alone serves the request, so nothing errors
and nothing is lost except a round trip — paid on *every* request, for as long
as the deployment is configured, which is a doubling of upstream calls visible
only as latency. It could not be remembered while the body was fixed before
routing, because nothing then knew which deployment had refused.

**A `400` the caller earned is not a refusal.** It fails the plain body too, so
the annotation is only blamed once removing it is shown to help. Without that
rule one malformed request would switch prompt caching off for every other
caller of that deployment, silently and until a restart. The caller still gets
their own error, wording intact; it just costs one extra round trip to establish
that it was theirs.

The memory is per process rather than persisted. What an upstream accepts can
change with a release in either direction, and a restart is the cheapest way to
re-ask: it costs one round trip on one request.

The whole arrangement applies to any body the gateway annotated, the
streamed-usage field included.

A string `system` prompt has nowhere to hang a breakpoint, so it is promoted to
the single text block the API defines it to equal. The original string's bytes
are reused verbatim rather than decoded and re-encoded.

Everything the gateway does edit, it edits by splicing rather than re-encoding.
A body run through a JSON decoder and back reorders object keys and renormalizes
numbers — which changes the very prefix bytes the upstream cache keys on.

---

## Seeing whether it works

Per response:

| Header | Meaning |
|---|---|
| `x-gateway-prompt-affinity: hit` | served by the deployment holding this prefix |
| `x-gateway-prompt-affinity: new` | this prefix had no pin; it has one now |
| `x-gateway-prompt-affinity: miss` | a pin existed and something else served it |
| absent | no pin was in play |

A pin passed over for load that the strategy then chose anyway still reports
`hit`: the request did reach the warm upstream, which is what the metric is for.

`new` and `miss` are deliberately separate. Both mean the request did not land on
a pinned deployment, but only a miss is a problem — a new prefix has nothing to
honour yet. Merging them would put every conversation's opening turn into the
miss count.

In `/metrics`:

| Metric | Labels | Answers |
|---|---|---|
| `gateway_prompt_cache_tokens_total` | model, deployment, outcome=`read`\|`write`\|`write_1h` | is caching saving money |
| `gateway_prompt_cache_requests_total` | model, deployment, outcome=`hit`\|`miss` | is caching working |
| `gateway_prompt_affinity_total` | model, deployment, outcome=`hit`\|`miss`\|`new` | is routing keeping it working |

The three are separate on purpose. Read/write tokens show the money. Hit rate
shows whether prompts are being cached at all. Affinity shows whether the
router, rather than the prompts, is the reason they are not.

A useful alert is a rising `gateway_prompt_affinity_total{outcome="miss"}` share
while traffic is steady: something has started scattering conversations away from
the upstreams holding their prefixes, and the bill will follow. Measure it
against `hit` alone — including `new` in the denominator makes the ratio move
with how many fresh conversations start, which is a fact about your traffic
rather than about your routing.

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
rather than inferred from a hit rate on a dashboard. The identity it is defined
by:

```
cost + cache_savings = what the same tokens would have cost as ordinary input
```

**It is net of the write premium, so it goes negative.** Establishing an entry
costs 1.25x base input, or twice at the one-hour tier, so a request that writes
a cache and never reads one back is dearer than the same request with no caching
at all. That is every turn of a conversation scattered across deployments, and
reported as a floor of zero it was invisible: the reads that never happened
simply did not appear, and the premiums paid for them looked like ordinary cost.
A negative `cache_savings` says prompt caching is currently costing this operator
money, which is the one number on this page that says it outright.

A steady negative is worth alerting on. Read it beside
`gateway_prompt_affinity_total{outcome="miss"}`: the affinity metric says routing
is scattering conversations, and this says what that is worth.

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
  cache_write_1h_per_1m: 6.00   # only if your callers use the one-hour cache
```

Claude Code leans on prompt caching heavily, so a cost model using only input
and output is wrong by a wide margin on exactly the traffic this gateway exists
to carry. It is wrong quietly, too — nothing errors, the numbers just do not
match the invoice — so a cost model that prices input without pricing the cache
is [refused at load](configuration.md#a-partial-cost-model-is-refused-at-load)
rather than served.

The gateway reads Anthropic's `cache_read_input_tokens` and
`cache_creation_input_tokens` from the response, including from the
`message_start` event of a streamed reply, and the OpenAI-compatible
`prompt_tokens_details.cached_tokens` under the rule
[above](#openai-compatible-deployments).

### The two write tiers

Anthropic's default breakpoint writes a cache that lives about five minutes and
costs 1.25x base input. A caller can ask for one that lives an hour, and that
one costs **2x**. The response reports the split under `cache_creation`, and a
gateway that flattened the two would under-report every long write by more than
a third — on exactly the traffic that opts into the longer cache, which is the
traffic large enough to have bothered.

`cache_write_1h_per_1m` prices the long tier. Left unset it falls back to
`cache_write_per_1m`, which is correct for a deployment whose callers never ask
for the longer TTL. For one whose callers do, the fallback is understating the
bill, so the gateway says so: the first response reporting a one-hour write on a
deployment without the price logs a warning naming the deployment and the key to
add, and `gateway_prompt_cache_tokens_total{outcome="write_1h"}` counts the
tokens it is happening to.

The split is read from `cache_creation` on the response. A reply produced by a
**server-side tool loop** is several turns against the model reported as one
response, and reports that split only per iteration, under `usage.iterations`.
The totals there are already summed, so only the split is taken from them —
adding the totals again would bill one reply several times over — and whatever
the iterations leave unaccounted for is charged at the five-minute rate rather
than at the premium. Without that, a request whose long writes all happened
inside the loop looks like a request with no long writes at all.

### The long-context tier

A provider may also charge more for **every** token of a request once its prompt
crosses a size. Anthropic's line is at 200k tokens, above which input, output,
cache reads and cache writes all cost more:

```yaml
cost:
  input_per_1m: 3.00
  output_per_1m: 15.00
  cache_read_per_1m: 0.30
  cache_write_per_1m: 3.75
  long_context:
    above_prompt_tokens: 200000
    input_per_1m: 6.00
    output_per_1m: 22.50
    cache_read_per_1m: 0.60
    cache_write_per_1m: 7.50
```

This matters here more than anywhere else in the cost model, because a
conversation long enough to cross the threshold is the conversation a prompt
cache exists for. Priced at the small-request rates it is billed at roughly half
what the provider charged, and `cache_savings` is understated in the same
proportion — the discount is measured against an input rate the request never
paid.

The threshold is compared against every prompt token the response reported,
cache reads included, since that is what the provider measures its own threshold
against: a turn reading 190k tokens out of the cache is a large request and is
charged as one. The block is optional, and
[refused when incomplete](configuration.md#the-long-context-tier) for the reason
a partial cost model is.

---

## What guards this

Prompt caching is the difference between a bill and several times a bill, and
every way of getting it wrong is silent. So the tests assert on money rather
than on mechanism:

| Test | What it would catch |
|---|---|
| `TestAConversationPaysForItsPrefixOnce` | a ten-turn conversation through a balanced group, billed against a hand-computed figure. Each fake upstream holds its own cache, so a scattered conversation pays real cache writes |
| `TestScatteringAConversationCostsRealMoney` | the same workload with affinity off, proving the guarantee above is doing something rather than describing a group that never balanced |
| `TestOpenAICachedPrefixIsBilledOnce` | the double-billing bug, end to end, plus the invariant that input and cache-read tokens sum to what the provider reported |
| `TestAnExplicitCacheWriteIsBilledAtItsOwnRate` | an explicit-cache provider's writes billed at the input rate because it nests them, which understates the bill and lets a budget overrun |
| `TestUsageIsReadUnderTheFormatThatProducedIt` | every provider usage shape in use, read under both formats, against expected counters |
| `FuzzUsageAccounting` | a hostile or broken upstream producing negative counts, negative cost, or savings nobody made |
| `TestInjectionAddsBreakpointsAndNothingElse`, `FuzzInject` | a rewritten body that differs from the original by anything other than its breakpoints — which would change the very prefix bytes the cache keys on |
| `TestInjectionIsIdempotent` | breakpoints accumulating past the API's cap through a retry or a second gateway |
| `TestOneHourWritesArePricedAtTheirOwnRate` | the long tier billed at the short tier's price |
| `TestPromptAffinitySurvivesAMovingBreakpoint` | a conversation re-pinned every turn because its caller's breakpoint moved, which is what every Anthropic client's does |
| `TestFingerprintIgnoresAMovingBreakpoint`, `TestFingerprintReadsTheTurnThatDiscriminates` | the same at the fingerprint, in both wire formats |
| `TestAStreamedOpenAIConversationIsBilled`, `TestAnUnaskedStreamIsBilledAtNothing` | a streamed OpenAI-compatible workload billed at zero, and the proof that the first test would notice |
| `TestAWriteNobodyReadsIsReportedAsALoss` | a cache written and never read reported as a saving, or as nothing, rather than as the premium it cost |
| `TestCostAndSavingsReconstructTheUncachedBill`, `FuzzUsageAccounting` | savings that are not the difference between the bill and the one the same tokens would have run up uncached |
| `TestInjectionCachesTheConversationToo` | a growing conversation re-read at full price behind a breakpoint that only covers tools and system |
| `TestInjectionOnlyMarksAToolItUnderstands` | a breakpoint hung on a server tool or an MCP toolset, whose accepted shape the gateway cannot check |
| `TestAnUpstreamThatRefusesAnAnnotationStillServesTheRequest` | a caller losing a request over an optimization it never asked for |
| `TestInjectionMarksOnlyTheDeploymentsThatCanTakeIt` | a subscription body rewritten, or an API caller left uncached, because one fleet holds both |
| `TestARefusedAnnotationIsNotAskedForAgain` | an upstream's refusal rediscovered on every request, doubling its call count for as long as it is configured |
| `TestACallersOwnBadRequestLeavesAnnotationOn` | one malformed request switching prompt caching off for every other caller of a deployment |
| `TestAPinOutlivesTheCacheItPointsAt` | a five-minute pin on a one-hour cache, which pays the long tier's premium twice |
| `TestPromptCacheCostConsumesABudget` | a budget that prices only input and output, letting a key run indefinitely on the tokens it costs most for |
| `TestAFailedAttemptIsNotBilled` | a retry billed twice, or billed to the deployment that failed rather than the one whose cache is now warm |
| `TestSpendTotalsCarryBothSignsOfSavings`, `TestSavingsAccumulateWithTheirSign` | a fleet losing money on caching reported as breaking even |
| `TestNegativeSavingsSurviveRedis`, `TestFilePersistenceCarriesEveryFigure` | a figure that survives one process and not a restart or a second replica |
| `TestAnUnpricedLongWriteIsReported` | a deployment quietly understating every long write, with nothing to say so |
| `TestPromptCacheMetricsCarryTheirValues` | a scrape naming the right series with the wrong numbers, which looks like an answer |
| `TestPricingValidation` | a cost model that would misreport what caching costs, accepted at load |
| `TestALongContextConversationIsBilledAtTheTierItRanIn`, `TestALargePromptIsBilledAtItsOwnTier` | the largest requests a deployment serves billed at the small-request rates, which is about half what they cost |
| `TestSavingsOnALargePromptUseTheTierItWasBilledAt` | a discount measured against an input rate the request never paid |
| `TestALongWriteInsideAToolLoopIsPricedAtItsOwnTier` | a long write inside a server-tool loop billed at the five-minute rate, because the split was only reported per iteration |
| `TestAPrefixTheProviderDidNotCacheIsNotPinned`, `TestAnUncachedPrefixIsNotPinned` | a prefix no upstream cached concentrating traffic on one deployment for nothing |
| `TestACacheWriteEstablishesThePin`, `TestAnOpenAIPrefixIsPinnedWithoutACacheSignal`, `TestAResponseThatReportedNoUsageKeepsThePin` | the same rule read so strictly that a first pin is never established at all |
| `TestTokenCountingLeavesNoPin` | a conversation pinned by a request that never reached the model |
| `TestAnOpenAIFingerprintIsStableFromTheFirstRequest` | an OpenAI conversation with no system message re-pinned on its second turn |
