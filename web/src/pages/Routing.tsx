import type { ConfigResponse, Deployment, DeploymentsResponse } from '../api'
import { useApi } from '../useApi'
import { Empty, Notice, PageHead, Panel, Pill } from '../components/ui'
import { count, money, seconds, tokens } from '../format'
import type { PageProps } from './Overview'

export function Routing({ onUnauthorized }: PageProps) {
  const { data, error } = useApi<DeploymentsResponse>('/deployments', 10_000, onUnauthorized)
  const config = useApi<ConfigResponse>('/config', 60_000, onUnauthorized)

  const rows = data?.deployments ?? []
  const groups = config.data?.groups ?? []
  const router = config.data?.router ?? {}

  const byGroup = new Map<string, Deployment[]>()
  for (const d of rows) {
    byGroup.set(d.model_name, [...(byGroup.get(d.model_name) ?? []), d])
  }

  const unpriced = rows.filter((d) => d.billable && !d.priced)

  return (
    <>
      <PageHead title="Routing">
        The model groups callers ask for, the upstreams behind each, and what the router does when
        one fails. Read-only: routing is configuration, not console state.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      {data?.prompt_cache.affinity_note && (
        <Notice tone="warn">{data.prompt_cache.affinity_note}</Notice>
      )}

      {unpriced.length > 0 && (
        <Notice tone="warn">
          <strong>{unpriced.length} billable deployment{unpriced.length === 1 ? '' : 's'} carry no price.</strong>{' '}
          Their traffic reports as costing nothing, so every budget drawn against them is being
          enforced against zero: {unpriced.map((d) => d.deployment).join(', ')}.
        </Notice>
      )}

      <div className="grid grid-4 mb tight">
        <Summary label="Model groups" value={String(byGroup.size)} sub="names a caller may ask for" />
        <Summary
          label="Deployments ready"
          value={`${data?.healthy_deployments ?? 0} / ${data?.total_deployments ?? 0}`}
          sub={`${data?.strategy ?? '—'} strategy`}
          tone={data && data.healthy_deployments < data.total_deployments ? 'bad' : undefined}
        />
        <Summary
          label="Retries"
          value={String(router.num_retries ?? 0)}
          sub={`backoff ${router.backoff_initial_ms ?? 0}ms → ${router.backoff_max_ms ?? 0}ms`}
        />
        <Summary
          label="Cooldown"
          value={`${router.cooldown_allowed_fails ?? 0} fails`}
          sub={`then ${seconds(router.cooldown_period_seconds)} out of rotation`}
        />
      </div>

      {groups.length > 0 && (
        <div className="grid grid-2 mb">
          {groups.map((group) => {
            const members = byGroup.get(group.name) ?? []
            const ready = members.filter((d) => !d.cooling_down).length
            return (
              <Panel
                key={group.name}
                title={
                  <>
                    {group.name}
                    {group.format && <Pill tone="neutral">{group.format}</Pill>}
                  </>
                }
                actions={
                  <Pill tone={members.length === 0 ? 'neutral' : ready === members.length ? 'ok' : ready === 0 ? 'bad' : 'warn'} dot>
                    {ready}/{members.length} ready
                  </Pill>
                }
              >
                <div className="panel-body" style={{ paddingTop: 12 }}>
                  {members.length === 0 ? (
                    <p className="hint" style={{ margin: 0 }}>No deployment resolves to this group.</p>
                  ) : (
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                      {members.map((d) => (
                        <div key={d.deployment} style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                          <span className={`nav-dot ${d.cooling_down ? 'bad' : 'ok'}`} style={{ marginLeft: 0 }} />
                          <span className="mono wrap-anywhere" style={{ flex: 1, fontSize: 12 }}>{d.deployment}</span>
                          <span className="hint nowrap" style={{ marginTop: 0 }}>
                            weight {d.weight}{d.in_flight > 0 ? ` · ${d.in_flight} in flight` : ''}
                          </span>
                        </div>
                      ))}
                    </div>
                  )}

                  <FallbackChain
                    label="On failure"
                    to={group.fallbacks}
                    empty="No fallback — a failure here is returned to the caller."
                  />
                  <FallbackChain label="On a context-window error" to={group.context_window_fallbacks} />
                  <FallbackChain label="On a content-policy refusal" to={group.content_policy_fallbacks} />
                </div>
              </Panel>
            )
          })}
        </div>
      )}

      <Panel
        title="Deployments"
        actions={<span className="hint" style={{ marginTop: 0 }}>{data?.strategy} routing</span>}
        note="One row per upstream. Weight is how much of a group's traffic this deployment is offered; in-flight is what it is carrying right now."
      >
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Deployment</th>
                <th>Group</th>
                <th>Upstream</th>
                <th>Auth</th>
                <th className="num">Weight</th>
                <th className="num">Limits</th>
                <th className="num">In flight</th>
                <th>Price / 1M</th>
                <th className="num">Requests</th>
                <th className="num">Cost</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((d) => (
                <tr key={d.deployment}>
                  <td className="mono wrap-anywhere">{d.deployment}</td>
                  <td>{d.model_name}</td>
                  <td className="wrap-anywhere">
                    <div className="mono">{d.api_base}</div>
                    <div className="cell-sub">{d.upstream_model} · {d.format}</div>
                  </td>
                  <td>
                    <Pill tone={d.auth_mode === 'passthrough' ? 'accent' : 'neutral'}
                      title={d.auth_mode === 'passthrough'
                        ? "relays the caller's own credential upstream, so their subscription is billed"
                        : 'substitutes a server-side provider key'}>
                      {d.auth_mode}
                    </Pill>
                  </td>
                  <td className="num">{d.weight}</td>
                  <td className="num hint" style={{ marginTop: 0 }}>
                    {[d.rpm ? `${d.rpm} rpm` : null, d.tpm ? `${d.tpm} tpm` : null]
                      .filter(Boolean).join(' · ') || '—'}
                  </td>
                  <td className="num">{d.in_flight}</td>
                  <td className="hint" style={{ marginTop: 0 }}>
                    {!d.billable ? (
                      <span title="the caller's own subscription is billed, so no price applies here">
                        billed to caller
                      </span>
                    ) : d.priced ? (
                      <>
                        in {money(d.cost.input_per_1m)} · out {money(d.cost.output_per_1m)}
                        <div>cache {money(d.cost.cache_read_per_1m)} / {money(d.cost.cache_write_per_1m)}</div>
                      </>
                    ) : (
                      <Pill tone="warn" title="billable traffic with no price configured reports as costing nothing">
                        unpriced
                      </Pill>
                    )}
                  </td>
                  <td className="num">
                    {count(d.spend.requests)}
                    <div className="cell-sub">{tokens(d.spend.input_tokens + d.spend.output_tokens)} tok</div>
                  </td>
                  <td className="num">{d.billable ? money(d.spend.cost) : '—'}</td>
                  <td>
                    {d.cooling_down
                      ? <Pill tone="bad" dot>cooling down</Pill>
                      : <Pill tone="ok" dot>ready</Pill>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {rows.length === 0 && <Empty><strong>No deployments configured.</strong></Empty>}
      </Panel>

      {config.data && (
        <Panel title="Router settings">
          <div className="panel-body">
            <div className="settings-grid">
              <Setting label="Strategy" value={config.data.strategy} />
              <Setting label="Retries per attempt" value={String(router.num_retries ?? 0)} />
              <Setting label="Max fallback hops" value={String(router.max_fallback_hops ?? 0)} />
              <Setting label="Request timeout" value={seconds(router.timeout_seconds)} />
              <Setting label="Stream timeout" value={seconds(router.stream_timeout_seconds)} />
              <Setting label="Backoff jitter" value={String(router.backoff_jitter ?? 0)} />
              <Setting label="Lowest-latency buffer" value={String(router.lowest_latency_buffer ?? 0)} />
              <Setting
                label="Cross-format translation"
                value={config.data.translation.enabled ? 'enabled' : 'off'}
              />
            </div>
            <p className="hint" style={{ marginTop: 16, maxWidth: '84ch' }}>
              <strong>Cross-format translation.</strong> {config.data.translation.note}
            </p>
          </div>
        </Panel>
      )}
    </>
  )
}

function Setting({ label, value }: { label: string; value: string }) {
  return (
    <div className="setting">
      <span className="setting-label">{label}</span>
      <span className="setting-dots" />
      <span className="mono">{value}</span>
    </div>
  )
}

function Summary({ label, value, sub, tone }: {
  label: string
  value: string
  sub: string
  tone?: 'bad'
}) {
  return (
    <div className="stat" style={{ minHeight: 0 }}>
      <div className="stat-label">{label}</div>
      <div className="stat-value" style={{ fontSize: 20, color: tone === 'bad' ? 'var(--bad)' : undefined }}>{value}</div>
      <div className="stat-sub">{sub}</div>
    </div>
  )
}

/**
 * FallbackChain draws where a group's traffic goes when it cannot be served.
 *
 * The arrow matters more than it looks: a fallback is the one piece of routing
 * that is invisible in every other view — a request that fell back reports the
 * deployment that answered, not the group it was asked of — so this is the only
 * place the configured path can be read before it is needed.
 */
function FallbackChain({ label, to, empty }: {
  label: string
  to: string[] | null | undefined
  empty?: string
}) {
  if (!to?.length) {
    if (!empty) return null
    return (
      <p className="hint" style={{ marginTop: 12 }}>{empty}</p>
    )
  }
  return (
    <div style={{ marginTop: 12 }}>
      <div className="hint" style={{ marginTop: 0, marginBottom: 5 }}>{label}</div>
      <div className="pill-row" style={{ alignItems: 'center' }}>
        {to.map((name, i) => (
          <span key={name} style={{ display: 'inline-flex', alignItems: 'center', gap: 5 }}>
            {i > 0 && <span style={{ color: 'var(--faint)' }}>→</span>}
            <Pill tone="accent">{name}</Pill>
          </span>
        ))}
      </div>
    </div>
  )
}
