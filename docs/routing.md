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
6. **Strategy picks** one survivor.
7. **Reserve capacity** on the chosen deployment only. Budget is never spent on
   candidates that go unused.

If nothing survives, the request fails with `503` and
`no healthy deployment available`.

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

## Rate limits

`rpm` and `tpm` on a deployment are **enforced limits**, checked before dispatch
and consumed only by the request actually sent. Virtual keys carry their own
`rpm_limit` and `tpm_limit`, applied at authentication.

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
| `x-litellm-stream-timeout` | Override the time-to-first-chunk timeout. |

A `"disable_fallbacks": true` field in the request body skips fallbacks entirely.

## Response headers

| Header | Meaning |
|---|---|
| `x-gateway-model-id` | The model group that served the request. |
| `x-gateway-deployment` | The deployment that served it. |
| `x-gateway-attempted-retries` | Retries used within the final group. |
| `x-gateway-attempted-fallbacks` | Fallback hops taken. |
| `x-gateway-request-id` | Correlates with the access log. |

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
| In-flight counter | Can go negative | Clamped; released exactly once |
| `InternalServerErrorRetries` | Declared but never read | Every declared policy field is honoured |
