import { useState } from 'react'
import type { AnalyticsResponse, Breakdown } from '../api'
import { useApi } from '../useApi'
import { Empty, Notice, PageHead, Panel, Pill, Segmented, Stat } from '../components/ui'
import { BarList, Percentiles, ShareBar, TimeSeries } from '../components/charts'
import { count, money, ms, percent, rate, tokens } from '../format'
import { OUTCOME_COLOR, type PageProps } from './Overview'

// The ranges the ring can actually answer. It is bounded by request_log_size
// rather than by time, so a busy gateway's "6h" is the last few thousand
// requests and may not reach back six hours — which the page says rather than
// drawing a flat line and letting it be read as quiet.
const RANGES = [
  { value: '15m', label: '15m', buckets: 45 },
  { value: '1h', label: '1h', buckets: 60 },
  { value: '6h', label: '6h', buckets: 72 },
  { value: '24h', label: '24h', buckets: 96 },
] as const

type Range = (typeof RANGES)[number]['value']

export function Analytics({ onUnauthorized }: PageProps) {
  const [range, setRange] = useState<Range>('1h')
  const [group, setGroup] = useState('')

  const buckets = RANGES.find((r) => r.value === range)!.buckets
  const query = new URLSearchParams({ window: range, buckets: String(buckets) })
  if (group) query.set('model_group', group)

  const { data, error, loading } = useApi<AnalyticsResponse>(
    `/analytics?${query.toString()}`, 15_000, onUnauthorized,
  )

  // "served" is not a field the gateway sends. The stack below has to sum to
  // the request count, and requests, errors, refusals and cache hits are four
  // overlapping counts of the same bucket — so the plain successes are what is
  // left once the other three are taken out, computed here rather than adding a
  // fifth number to the endpoint that only this chart would read.
  const series = (data?.series ?? []).map((b) => ({
    ...b,
    served: Math.max(b.requests - b.errors - b.rejected - b.cache_hits, 0),
  }))
  const totals = data?.totals
  const windowSeconds = data
    ? (new Date(data.to).getTime() - new Date(data.from).getTime()) / 1000
    : 0
  const failed = (totals?.errors ?? 0) + (totals?.rejected ?? 0)
  const allGroups = data?.groups ?? []

  return (
    <>
      <PageHead
        title="Analytics"
        actions={
          <>
            <select value={group} onChange={(e) => setGroup(e.target.value)} style={{ width: 'auto', minWidth: 160 }}>
              <option value="">All model groups</option>
              {allGroups.map((g) => <option key={g.name} value={g.name}>{g.name}</option>)}
            </select>
            <Segmented options={RANGES} value={range} onChange={setRange} />
          </>
        }
      >
        Throughput, latency, errors and cost over time, aggregated from the request buffer this
        instance holds. {data?.note}
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      <div className="grid grid-4 mb">
        <Stat
          label="Requests"
          value={count(totals?.requests)}
          sub={rate(totals?.requests ?? 0, windowSeconds)}
        />
        <Stat
          label="Failure rate"
          value={totals?.requests ? percent(failed, totals.requests) : '—'}
          sub={totals ? `${count(totals.errors)} errored · ${count(totals.rejected)} refused` : undefined}
        />
        <Stat
          label="Tokens"
          value={tokens((totals?.input_tokens ?? 0) + (totals?.output_tokens ?? 0))}
          sub={totals ? `${tokens(totals.input_tokens)} in · ${tokens(totals.output_tokens)} out` : undefined}
        />
        <Stat
          label="Cost"
          value={money(totals?.cost ?? 0)}
          sub={totals ? `${count(totals.billable_requests)} billable requests` : undefined}
        />
      </div>

      <Panel
        title="Requests over time"
        note="Stacked by how each request ended, so a spike in traffic and a spike in refusals are distinguishable at a glance rather than by opening the table below."
      >
        <div className="panel-body">
          {loading && series.length === 0 ? (
            <div className="chart-empty" style={{ height: 220 }}>Loading…</div>
          ) : (
            <TimeSeries
              points={series}
              height={220}
              mode="bars"
              series={[
                { key: 'served', label: 'Served', color: 'var(--ok)', format: count },
                { key: 'cache_hits', label: 'Cache hit', color: 'var(--info)', format: count },
                { key: 'rejected', label: 'Refused', color: 'var(--warn)', format: count },
                { key: 'errors', label: 'Errored', color: 'var(--bad)', format: count },
              ]}
              empty="No requests recorded in this window."
            />
          )}
        </div>
      </Panel>

      <div className="split mb">
        <Panel title="Latency over time" note="p50 and p95 per bucket, over requests that reached an upstream.">
          <div className="panel-body">
            <TimeSeries
              points={series}
              height={190}
              mode="area"
              yFormat={(v) => (v >= 1000 ? (v / 1000).toFixed(1) + 's' : Math.round(v) + 'ms')}
              series={[
                { key: 'latency_p50_ms', label: 'p50', color: 'var(--chart-1)', format: ms },
                { key: 'latency_p95_ms', label: 'p95', color: 'var(--chart-2)', format: ms },
              ]}
              empty="No requests reached an upstream in this window."
            />
          </div>
        </Panel>

        <Panel title="Distribution">
          <div className="panel-body">
            {data ? (
              <>
                <Percentiles
                  format={ms}
                  max={data.latency.max_ms}
                  marks={[
                    { label: 'p50', value: data.latency.p50_ms },
                    { label: 'p95', value: data.latency.p95_ms, tone: 'warn' },
                    { label: 'p99', value: data.latency.p99_ms, tone: 'bad' },
                    { label: 'max', value: data.latency.max_ms, tone: 'bad' },
                  ]}
                />
                <p className="section-label" style={{ marginTop: 20 }}>Streaming</p>
                <dl className="kv" style={{ gridTemplateColumns: '1fr auto' }}>
                  <dt>Time to first token, p50</dt><dd>{ms(data.latency.ttft_p50_ms)}</dd>
                  <dt>Time to first token, p95</dt><dd>{ms(data.latency.ttft_p95_ms)}</dd>
                  <dt>Throughput, p50</dt>
                  <dd>{data.latency.throughput_p50_tps ? data.latency.throughput_p50_tps.toFixed(1) + ' tok/s' : '—'}</dd>
                  <dt>Streamed</dt>
                  <dd>{count(totals?.streamed)} of {count(totals?.requests)}</dd>
                </dl>
              </>
            ) : <p className="hint">No timings yet.</p>}
          </div>
        </Panel>
      </div>

      <div className="split mb">
        <Panel title="Tokens over time" note="Stacked: what was sent, what came back, and what the prompt cache served instead of either.">
          <div className="panel-body">
            <TimeSeries
              points={series}
              height={190}
              mode="stacked"
              yFormat={tokens}
              series={[
                { key: 'input_tokens', label: 'Input', color: 'var(--chart-1)', format: tokens },
                { key: 'output_tokens', label: 'Output', color: 'var(--chart-2)', format: tokens },
                { key: 'cache_read_tokens', label: 'Cache read', color: 'var(--chart-3)', format: tokens },
                { key: 'cache_write_tokens', label: 'Cache write', color: 'var(--chart-4)', format: tokens },
              ]}
            />
          </div>
        </Panel>

        <Panel title="How requests ended">
          <div className="panel-body">
            <ShareBar
              parts={(data?.outcomes ?? []).map((o) => ({
                label: o.outcome.replace(/_/g, ' '),
                value: o.count,
                color: OUTCOME_COLOR[o.outcome] ?? 'var(--muted)',
              }))}
              format={count}
            />
            {(data?.reject_reasons ?? []).length > 0 && (
              <>
                <p className="section-label" style={{ marginTop: 22 }}>Why requests were refused</p>
                <div style={{ marginTop: 8 }}>
                  <BarList
                    rows={(data?.reject_reasons ?? []).map((r) => ({
                      name: r.reason.replace(/_/g, ' '),
                      value: r.count,
                    }))}
                    format={count}
                  />
                </div>
              </>
            )}
            <p className="hint" style={{ marginTop: 14 }}>
              A refusal is the gateway's own decision — a blocked key, an exhausted budget, a model
              the key may not reach. An error is an upstream's, or this gateway failing to reach one.
            </p>
          </div>
        </Panel>
      </div>

      <div className="grid grid-3">
        <BreakdownPanel title="By model group" rows={data?.groups} />
        <BreakdownPanel title="By deployment" rows={data?.deployments} />
        <BreakdownPanel title="By key" rows={data?.keys} note="Top ten by request count." />
      </div>

      {totals && totals.requests === 0 && (
        <Notice>
          No requests in this window. The buffer holds the most recent requests this instance served
          and nothing from before its last restart, so an empty window here does not mean an idle
          gateway — only an idle process.
        </Notice>
      )}
    </>
  )
}

function BreakdownPanel({ title, rows, note }: { title: string; rows: Breakdown[] | null | undefined; note?: string }) {
  const list = rows ?? []
  return (
    <Panel title={title} note={note}>
      {list.length === 0 ? (
        <Empty>Nothing in this window.</Empty>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th className="num">Req</th>
                <th className="num">p95</th>
                <th className="num">Cost</th>
              </tr>
            </thead>
            <tbody>
              {list.map((row) => (
                <tr key={row.name}>
                  <td className="wrap-anywhere">
                    {row.name}
                    {row.errors > 0 && (
                      <div style={{ marginTop: 3 }}>
                        <Pill tone="bad">{row.errors} failed</Pill>
                      </div>
                    )}
                  </td>
                  <td className="num">{count(row.requests)}</td>
                  <td className="num">{ms(row.latency_p95_ms)}</td>
                  <td className="num">{row.cost > 0 ? money(row.cost) : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  )
}
