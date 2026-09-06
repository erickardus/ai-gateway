# Routing

One public `model_name` can be served by several deployments. Together they form
a **model group**, and the router load-balances across it, ejects failing
members, retries, and falls back to other groups.

## Selection pipeline

Every request walks this pipeline in order:

1. **Resolve the model group** from the request body's `model` field.
2. **Filter ejected deployments** (in cooldown).
3. **Filter unauthorized deployments** — a `passthrough` deployment is only
   offered to keys with `allow_passthrough: true`.
4. **Filter deployments already tried** on this request, so a retry makes
   progress rather than landing back on the one that just failed.
5. **Filter over-capacity deployments** using a non-consuming check.
6. **Prefer the prompt-prefix pin**, if the request has one and the pinned
   deployment is among the survivors. See
   [prompt-caching.md](prompt-caching.md#prefix-affinity).
7. **Strategy picks** one survivor, when no pin decided it.
8. **Reserve capacity** on the chosen deployment only. Budget is never spent on
   candidates that go unused.

If nothing survives, the request fails with `503` and
`no healthy deployment available`.

Note the order: the pin is consulted **after** every filter, so it can only ever
choose among deployments that were already eligible. A pinned deployment that is
cooling down, unauthorized, already failed, or at its rate limit is passed over
exactly as if there were no pin.

## Strategies

| `router.strategy` | Picks | Notes |
|---|---|---|
| `weighted-shuffle` | Randomly, proportional to `weight` | Default. `weight: 9` vs `weight: 1` ⇒ ~90% / ~10%. |
| `least-busy` | Fewest in-flight requests | Ties broken randomly. |
| `usage-based` | Fewest tokens used this window | Spreads by cost rather than request count. |
| `latency-based` | Lowest mean recent latency | Mean over the last 10 samples, within `lowest_latency_buffer`; unseen deployments are tried first. |

## Retries

Total upstream attempts are `1 + num_retries`.

- A retry **always moves to a different deployment** while an untried one is
  available.
- **Backoff is skipped** when an untried deployment exists — with an alternative
  available there is nothing to wait for.
- When every deployment has failed, the router waits (exponential from
  `backoff.initial`, capped at `backoff.max`, with jitter) and then tries again
  from a clean slate.
- An upstream `Retry-After` in `(0, 60]` seconds overrides the computed delay.

Only retryable failures are retried: HTTP `408`, `409`, `429`, any `5xx`, and
transport-level errors. A `400` is the caller's fault and fails immediately.

## Cooldowns

A deployment is ejected for `cooldown.period` once it exceeds
`cooldown.allowed_fails` failures within that period. Recovery is by elapsed
time, and it resets the failure tally so a recovered deployment starts clean.

`allowed_fails: 0` means "eject on the first failure". The configured value
always means what it says — it is a pointer internally so an explicit `0` is
distinguishable from an omitted field.

Not every failure counts toward ejection:

| Failure | Ejects? | Why |
|---|---|---|
| `401`, `403`, `429` on a **passthrough** deployment | No | The credential is the caller's own, so the failure describes that caller. Counting it would let one developer's expired token eject the shared upstream for everyone. |
| Connection error / timeout | No | Says more about the network than the deployment. |
| `400`, `422`, other 4xx | No | The caller's fault; one bad request must not eject a healthy upstream. |
| `401`, `404`, `408`, `429` | Yes | The deployment itself is unusable or saturated. |
| Any `5xx` | Yes | The upstream is unhealthy. |

## Fallbacks

Retries are fully exhausted within a group **before** any fallback is
considered, and each fallback hop gets its own full retry budget.

```yaml
router:
  fallbacks:
    - from: anthropic-claude
      to: [anthropic-claude-haiku]
  context_window_fallbacks:
    - from: big-context
      to: [huge-context]
  content_policy_fallbacks:
    - from: strict-model
      to: [permissive-model]
```

The failure kind selects the list: context-window overflows use
`context_window_fallbacks`, content-policy refusals use
`content_policy_fallbacks`, everything else uses `fallbacks`. Both specialised
kinds are detected from the upstream's error body, since providers signal them
only in the message.

Chains are bounded by `max_fallback_hops` and guarded by an attempted-target
set, so a cyclic configuration cannot loop. **If everything fails, the original
error is returned**, not the last fallback's — the first failure is what
actually describes the problem.

## Error classification

Context-window overflows and content-policy refusals are signalled only in the
upstream's error body, and providers word them differently. Classification
therefore checks the provider's **structured error type first**
(`error.type`, `error.code`), falling back to substring matching. Both lists are
package variables — `ContextWindowErrorTypes`, `ContextWindowErrorMarkers`,
`ContentPolicyErrorTypes`, `ContentPolicyErrorMarkers` — so a deployment against
a provider with different wording can extend them.

## Rate limits

`rpm` and `tpm` on a deployment are **enforced limits**, checked before dispatch
and consumed only by the request actually sent. Virtual keys carry their own
`rpm_limit` and `tpm_limit`. A key's budget is charged only once the request is
known to be one the gateway will actually dispatch — after the model is resolved
and authorized — so a key is never billed for its own rejected requests, and
model discovery does not consume inference budget.

Windows are monotonic: each counter tracks its own start and rolls over a minute
later. They are deliberately not keyed on a wall-clock `HH-MM` bucket, which
wraps every 24 hours and skews when the clock is adjusted.

State is in-process, which is correct for a single instance. `StateStore` is an
interface, so a shared Redis implementation can make several instances agree
without any change to strategy or router code.

## Per-request overrides

Header names match LiteLLM's, so existing tooling works unchanged:

| Header | Effect |
|---|---|
| `x-litellm-num-retries` | Override `num_retries` for this request (0–10). |
| `x-litellm-timeout` | Override the request timeout, in seconds. |
| `x-litellm-stream-timeout` | Override the time-to-first-chunk timeout. Independent of `x-litellm-timeout`. |
| `x-litellm-tags` | Comma-separated labels recorded for attribution. Consumed by the gateway, never forwarded. |

Both are capped at 30 minutes. A larger value would overflow the conversion to a
duration and read as no deadline at all.

## Timeouts

`timeout` bounds a non-streaming request end to end. `stream_timeout` bounds
only the time until the upstream's response headers arrive — it is deliberately
**not** attached to the request context, because that would also bound reading
the body and would sever a long completion mid-stream after the status line had
already been sent.

A `"disable_fallbacks": true` field in the request body skips fallbacks entirely.

## Response headers

| Header | Meaning |
|---|---|
| `x-gateway-model-id` | The model group that served the request. |
| `x-gateway-deployment` | The deployment that served it. |
| `x-gateway-attempted-retries` | Retries used within the final group. |
| `x-gateway-attempted-fallbacks` | Fallback hops taken. |
| `x-gateway-request-id` | Correlates with the access log. |
| `x-gateway-prompt-affinity` | `hit`, `miss`, or `new` when a prompt-prefix pin was consulted; absent otherwise. |

## Differences from LiteLLM

Ported deliberately, after reading LiteLLM's source. Where its documented
behaviour and actual behaviour diverge, these follow neither blindly:

| Behaviour | LiteLLM | Here |
|---|---|---|
| `rpm` / `tpm` under the default strategy | Used as *weights*, never enforced | Always enforced limits; `weight` is the only weighting knob |
| Metric chosen for weighting | By inspecting whichever deployment sits first in the candidate list, making it order-dependent | `weight` only |
| `allowed_fails: 3` in config | A no-op — it equals the package default, so it is treated as unset | Always honoured |
| Retry target | Re-enters selection, but nothing forces a different deployment | Already-failed deployments are excluded |
| Backoff with a healthy peer | Skipped | Skipped — this one is right |
| Rate-limit window | Wall-clock `HH-MM`, wraps daily | Monotonic |
| In-flight counter | Can go negative | Clamped; released exactly once, on a context detached from the client's so an aborted request still decrements |
| Wire format | Not a routing dimension | Enforced: an ingress reaches only deployments of its own format, groups must be homogeneous, and fallbacks may not cross formats |
| Duplicate deployments | Undetectable, because the ID includes a per-group ordinal | Rejected at load: a duplicate would double that upstream's traffic share and rate limit |
| Provider prompt cache | An opt-in pre-call check, keyed on the messages up to and including the last `cache_control` marker — which its own injection walks forward every turn | On by default. Conversations are pinned to the upstream holding their warm prefix, on a fingerprint taken with the markers stripped, so a client walking one forward keeps its pin |
| Rate-limit charging | At authentication, so rejected requests spend budget | After the model is resolved and authorized |
| Error classification | Substring matching only | Structured provider error type first, substrings as fallback, both configurable |
| `InternalServerErrorRetries` | Declared but never read | Every declared policy field is honoured |
