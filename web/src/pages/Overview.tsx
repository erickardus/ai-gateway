import type { Overview as OverviewData } from '../api'
import { useApi } from '../useApi'
import { Notice, PageHead, Panel, Pill, Stat } from '../components/ui'
import { count, money, tokens } from '../format'

export type PageProps = { onUnauthorized: () => void }

export function Overview({ onUnauthorized }: PageProps) {
  const { data, error } = useApi<OverviewData>('/overview', 10_000, onUnauthorized)

  if (error) return <Notice tone="bad">{error}</Notice>
  if (!data) return <PageHead title="Overview" />

  const spend = data.spend
  const cache = data.prompt_cache

  return (
    <>
      <PageHead title="Overview">
        The gateway's current state. Cost covers only deployments you pay for — passthrough traffic
        bills the caller's own subscription and appears here as usage with no cost.
      </PageHead>

      {data.status === 'unhealthy' && (
        <Notice tone="bad">
          Every deployment is cooling down. The gateway is refusing traffic until one recovers.
        </Notice>
      )}
      {data.shared_state && !data.shared_state.reachable && (
        <Notice tone="warn">
          Shared state is unreachable. Rate limits and budgets are being enforced per instance until
          Redis recovers — a limit of 60 rpm is 60 per gateway, not 60 in total.
        </Notice>
      )}

      <div className="grid grid-4" style={{ marginBottom: 18 }}>
        <Stat
          label="Deployments"
          value={`${data.healthy_deployments} / ${data.total_deployments}`}
          sub={`${data.strategy} routing`}
        />
        <Stat
          label="Spend this window"
          value={spend ? money(spend.cost) : '—'}
          sub={spend ? `${count(spend.billable_requests)} of ${count(spend.requests)} requests billable` : 'accounting disabled'}
        />
        <Stat
          label="Prompt-cache savings"
          value={spend ? money(spend.cache_savings) : '—'}
          sub="not included in cost; what was not charged"
        />
        <Stat
          label="Keys"
          value={count(data.keys?.total)}
          sub={
            data.keys
              ? [
                  data.keys.blocked ? `${data.keys.blocked} blocked` : null,
                  data.keys.expired ? `${data.keys.expired} expired` : null,
                  data.keys.sso_issued ? `${data.keys.sso_issued} from SSO` : null,
                ].filter(Boolean).join(' · ') || 'all active'
              : undefined
          }
        />
      </div>

      <div className="grid grid-2">
        <Panel title="Deployments">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Deployment</th>
                  <th>Group</th>
                  <th>Auth</th>
                  <th className="num">In flight</th>
                  <th>State</th>
                </tr>
              </thead>
              <tbody>
                {data.deployments.map((d) => (
                  <tr key={d.deployment}>
                    <td className="mono wrap-anywhere">{d.deployment}</td>
                    <td>{d.model_name}</td>
                    <td>
                      <Pill tone={d.auth_mode === 'passthrough' ? 'accent' : 'neutral'}>
                        {d.auth_mode}
                      </Pill>
                    </td>
                    <td className="num">{d.in_flight}</td>
                    <td>
                      {d.cooling_down
                        ? <Pill tone="bad">cooling down</Pill>
                        : <Pill tone="ok">ready</Pill>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Panel>

        <div>
          <Panel title="Prompt cache">
            <div className="panel-body">
              <div style={{ display: 'flex', gap: 8, marginBottom: 10, flexWrap: 'wrap' }}>
                <Pill tone={cache.affinity ? 'ok' : 'neutral'}>
                  affinity {cache.affinity ? 'active' : 'inactive'}
                </Pill>
                <Pill tone={cache.inject ? 'ok' : 'neutral'}>
                  breakpoint injection {cache.inject ? 'on' : 'off'}
                </Pill>
                <Pill tone="neutral">ttl {cache.affinity_ttl}</Pill>
              </div>
              {cache.affinity_note && <p className="hint" style={{ margin: 0 }}>{cache.affinity_note}</p>}
              {spend && (
                <dl className="kv">
                  <dt>Cache reads</dt><dd>{tokens(spend.cache_read_tokens)} tokens</dd>
                  <dt>Cache writes</dt><dd>{tokens(spend.cache_write_tokens)} tokens</dd>
                  <dt>Input</dt><dd>{tokens(spend.input_tokens)} tokens</dd>
                  <dt>Output</dt><dd>{tokens(spend.output_tokens)} tokens</dd>
                </dl>
              )}
            </div>
          </Panel>

          <Panel title="Enabled">
            <div className="panel-body">
              <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                {Object.entries(data.features).map(([name, on]) => (
                  <Pill key={name} tone={on ? 'ok' : 'neutral'}>
                    {name.replace(/_/g, ' ')}{on ? '' : ' off'}
                  </Pill>
                ))}
              </div>
              <p className="hint" style={{ marginTop: 12 }}>
                Traffic buffer holds {count(data.traffic.held)} of {count(data.traffic.capacity)} records.
                {' '}{data.traffic.note}
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  )
}
