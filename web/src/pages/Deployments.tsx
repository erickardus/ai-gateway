import type { DeploymentsResponse } from '../api'
import { useApi } from '../useApi'
import { Empty, Notice, PageHead, Panel, Pill } from '../components/ui'
import { count, money, tokens } from '../format'
import type { PageProps } from './Overview'

export function Deployments({ onUnauthorized }: PageProps) {
  const { data, error } = useApi<DeploymentsResponse>('/deployments', 10_000, onUnauthorized)
  const rows = data?.deployments ?? []

  return (
    <>
      <PageHead title="Deployments">
        The upstreams behind each model group, as declared in gateway.yaml and as the router
        currently sees them. Read-only: routing is configuration, not console state.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      {data?.prompt_cache.affinity_note && (
        <Notice tone="warn">{data.prompt_cache.affinity_note}</Notice>
      )}

      <Panel
        title={`${data?.healthy_deployments ?? 0} of ${data?.total_deployments ?? 0} ready`}
        actions={<span className="hint">{data?.strategy} routing</span>}
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
                    <div className="hint">{d.upstream_model} · {d.format}</div>
                  </td>
                  <td>
                    <Pill tone={d.auth_mode === 'passthrough' ? 'accent' : 'neutral'}>{d.auth_mode}</Pill>
                  </td>
                  <td className="num">{d.weight}</td>
                  <td className="num hint">
                    {[d.rpm ? `${d.rpm} rpm` : null, d.tpm ? `${d.tpm} tpm` : null]
                      .filter(Boolean).join(' · ') || '—'}
                  </td>
                  <td className="num">{d.in_flight}</td>
                  <td className="hint">
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
                      <span className="pill pill-warn" title="billable traffic with no price configured reports as costing nothing">
                        unpriced
                      </span>
                    )}
                  </td>
                  <td className="num">
                    {count(d.spend.requests)}
                    <div className="hint">{tokens(d.spend.input_tokens + d.spend.output_tokens)} tok</div>
                  </td>
                  <td className="num">{d.billable ? money(d.spend.cost) : '—'}</td>
                  <td>{d.cooling_down ? <Pill tone="bad">cooling down</Pill> : <Pill tone="ok">ready</Pill>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {rows.length === 0 && <Empty>No deployments configured.</Empty>}
      </Panel>
    </>
  )
}
