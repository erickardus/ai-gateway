import { Link } from 'react-router-dom'
import type { AnalyticsResponse, Overview as OverviewData, TrafficResponse } from '../api'
import { useApi } from '../useApi'
import { Empty, Notice, PageHead, Panel, Pill, Stat } from '../components/ui'
import { BarList, Percentiles, ShareBar, Sparkline, TimeSeries } from '../components/charts'
import { count, money, ms, percent, timestamp, tokens } from '../format'

export type PageProps = { onUnauthorized: () => void }

// Outcome colours are the status palette, not chart series, because an outcome
// is a state: the same green means "healthy" wherever it appears in this
// console, and no series ever borrows it. Cache hits are the exception and take
// the informational blue — a hit is not a state of health, it is a different
// path through the gateway — and every one of them is labelled, so the colour
// is never carrying the meaning alone.
export const OUTCOME_COLOR: Record<string, string> = {
  success: 'var(--ok)',
  cache_hit: 'var(--info)',
  rejected: 'var(--warn)',
  upstream_error: 'var(--bad)',
  gateway_error: 'var(--bad)',
}

export function outcomeTone(outcome: string): 'ok' | 'warn' | 'bad' | 'info' | 'neutral' {
  switch (outcome) {
    case 'success': return 'ok'
    case 'cache_hit': return 'info'
    case 'rejected': return 'warn'
    case 'upstream_error':
    case 'gateway_error': return 'bad'
    default: return 'neutral'
  }
}

export function Overview({ onUnauthorized }: PageProps) {
  const { data, error } = useApi<OverviewData>('/overview', 10_000, onUnauthorized)
  const analytics = useApi<AnalyticsResponse>('/analytics?window=1h&buckets=48', 15_000, onUnauthorized)
  const recent = useApi<TrafficResponse>('/traffic?limit=8', 10_000, onUnauthorized)

  if (error) return <Notice tone="bad">{error}</Notice>

  const spend = data?.spend
  const cache = data?.prompt_cache
  const a = analytics.data
  const series = a?.series ?? []
  const totals = a?.totals
  const windowSeconds = a ? (new Date(a.to).getTime() - new Date(a.from).getTime()) / 1000 : 0

  return (
    <>
      <PageHead title="Overview">
        What this gateway is doing right now. Cost covers only deployments you pay for — passthrough
        traffic bills the caller's own subscription and appears here as usage with no cost.
      </PageHead>

      {data?.status === 'unhealthy' && (
        <Notice tone="bad">
          <strong>Every deployment is cooling down.</strong> The gateway is refusing traffic until one
          recovers. <Link to="/routing">See routing →</Link>
        </Notice>
      )}
      {data?.shared_state && !data.shared_state.reachable && (
        <Notice tone="warn">
          <strong>Shared state is unreachable.</strong> Rate limits and budgets are being enforced per
          instance until Redis recovers — a limit of 60 rpm is 60 per gateway, not 60 in total.
        </Notice>
      )}

      <div className="grid grid-4 mb">
        <Stat
          label="Requests · last hour"
          value={count(totals?.requests)}
          sub={totals
            ? `${percent(totals.errors + totals.rejected, totals.requests)} not served`
            : 'no traffic recorded'}
          chart={<Sparkline
            values={series.map((b) => b.requests)}
            label="requests per bucket over the last hour"
          />}
        />
        <Stat
          label="Spend this window"
          value={spend ? money(spend.cost) : '—'}
          sub={spend
            ? `${count(spend.billable_requests)} of ${count(spend.requests)} requests billable`
            : 'accounting disabled'}
          chart={<Sparkline
            values={series.map((b) => b.cost)}
            label="cost per bucket over the last hour"
          />}
        />
        <Stat
          label="Latency p95"
          value={a ? ms(a.latency.p95_ms) : '—'}
          sub={a ? `p50 ${ms(a.latency.p50_ms)} · p99 ${ms(a.latency.p99_ms)}` : undefined}
          chart={<Sparkline
            values={series.map((b) => b.latency_p95_ms)}
            color="var(--chart-1)"
            label="p95 latency per bucket"
          />}
        />
        <Stat
          label="Deployments ready"
          value={`${data?.healthy_deployments ?? 0} / ${data?.total_deployments ?? 0}`}
          sub={`${data?.strategy ?? '—'} routing`}
          chart={
            <div className="pill-row">
              {(data?.deployments ?? []).slice(0, 6).map((d) => (
                <Pill
                  key={d.deployment}
                  tone={d.cooling_down ? 'bad' : 'ok'}
                  title={`${d.deployment} — ${d.cooling_down ? 'cooling down' : 'ready'}`}
                  dot
                >
                  {d.model_name}
                </Pill>
              ))}
            </div>
          }
        />
      </div>

      <div className="split mb">
        <Panel
          title="Traffic"
          actions={<span className="hint" style={{ marginTop: 0 }}>last hour · {a?.bucket_seconds ?? 60}s buckets</span>}
        >
          <div className="panel-body">
            <TimeSeries
              points={series}
              height={200}
              mode="bars"
              series={[
                { key: 'requests', label: 'Requests', color: 'var(--chart-accent)', format: (v) => count(v) },
              ]}
              empty="No requests recorded in the last hour."
            />
          </div>
        </Panel>

        <div className="stack">
          <Panel title="Outcomes">
            <div className="panel-body">
              <ShareBar
                parts={(a?.outcomes ?? []).map((o) => ({
                  label: o.outcome.replace(/_/g, ' '),
                  value: o.count,
                  color: OUTCOME_COLOR[o.outcome] ?? 'var(--muted)',
                }))}
                format={(v) => count(v)}
              />
            </div>
          </Panel>

          <Panel title="Latency">
            <div className="panel-body">
              {a ? (
                <Percentiles
                  format={ms}
                  marks={[
                    { label: 'p50', value: a.latency.p50_ms },
                    { label: 'p95', value: a.latency.p95_ms, tone: 'warn' },
                    { label: 'p99', value: a.latency.p99_ms, tone: 'bad' },
                  ]}
                />
              ) : <p className="hint" style={{ margin: 0 }}>No timings yet.</p>}
              <p className="hint" style={{ marginTop: 12 }}>
                Measured over requests that reached an upstream. A refusal answered in a millisecond
                is excluded, because averaging it in makes a slow gateway look fast.
              </p>
            </div>
          </Panel>
        </div>
      </div>

      <div className="split mb">
        <Panel
          title="Busiest model groups"
          actions={<Link className="hint" to="/analytics" style={{ marginTop: 0 }}>Analytics →</Link>}
        >
          <div className="panel-body" style={{ padding: 8 }}>
            <BarList
              rows={(a?.groups ?? []).map((g) => ({
                name: g.name,
                value: g.requests,
                sub: `${ms(g.latency_p50_ms)} p50 · ${money(g.cost)}`,
              }))}
              format={(v) => count(v)}
              empty="No traffic in the last hour."
            />
          </div>
        </Panel>

        <Panel title="Prompt cache">
          <div className="panel-body">
            <div className="pill-row" style={{ marginBottom: 12 }}>
              <Pill tone={cache?.affinity ? 'ok' : 'neutral'} dot>
                affinity {cache?.affinity ? 'active' : 'inactive'}
              </Pill>
              <Pill tone={cache?.inject ? 'ok' : 'neutral'} dot>
                injection {cache?.inject ? 'on' : 'off'}
              </Pill>
              <Pill tone="neutral">ttl {cache?.affinity_ttl}</Pill>
            </div>
            {cache?.affinity_note && (
              <p className="hint" style={{ marginTop: 0, marginBottom: 12 }}>{cache.affinity_note}</p>
            )}
            {spend && (
              <>
                <ShareBar
                  parts={[
                    { label: 'Input', value: spend.input_tokens, color: 'var(--chart-1)' },
                    { label: 'Output', value: spend.output_tokens, color: 'var(--chart-2)' },
                    { label: 'Cache read', value: spend.cache_read_tokens, color: 'var(--chart-3)' },
                    { label: 'Cache write', value: spend.cache_write_tokens, color: 'var(--chart-4)' },
                  ]}
                  format={(v) => tokens(v) + ' tok'}
                />
                <p className="hint" style={{ marginTop: 12 }}>
                  Prompt caching has saved <strong>{money(spend.cache_savings)}</strong> against what
                  the same traffic would have cost uncached. Reported beside cost, never inside it.
                </p>
              </>
            )}
          </div>
        </Panel>
      </div>

      <Panel
        title="Latest requests"
        actions={<Link className="hint" to="/traffic" style={{ marginTop: 0 }}>All traffic →</Link>}
      >
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>Key</th>
                <th>Group</th>
                <th>Outcome</th>
                <th className="num">Tokens</th>
                <th className="num">Latency</th>
                <th className="num">Cost</th>
              </tr>
            </thead>
            <tbody>
              {(recent.data?.records ?? []).map((r) => (
                <tr key={r.id + r.at}>
                  <td className="mono">{timestamp(r.at)}</td>
                  <td>{r.key_alias || <span className="hint">—</span>}</td>
                  <td>{r.model_group || '—'}</td>
                  <td>
                    <Pill tone={outcomeTone(r.outcome)} dot>
                      {r.reject_reason || r.outcome.replace(/_/g, ' ')}
                    </Pill>
                  </td>
                  <td className="num">{tokens((r.usage.input_tokens ?? 0) + (r.usage.output_tokens ?? 0))}</td>
                  <td className="num">{ms(r.latency_ms)}</td>
                  <td className="num">{r.billable ? money(r.cost) : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {(recent.data?.records ?? []).length === 0 && (
          <Empty>
            <strong>No requests yet.</strong>
            This fills as traffic flows through this instance.
          </Empty>
        )}
      </Panel>

      {windowSeconds > 0 && totals && totals.requests === 0 && (
        <Notice>
          Nothing has been served in the last hour, so the charts above are empty rather than broken.
          The traffic buffer is held in this process and starts again from nothing after a restart.
        </Notice>
      )}
    </>
  )
}
