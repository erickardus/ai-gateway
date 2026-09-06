import { useState } from 'react'
import type { SpendHistoryResponse, SpendResponse } from '../api'
import { useApi } from '../useApi'
import { Empty, Notice, PageHead, Panel } from '../components/ui'
import { Trend } from '../components/Trend'
import { count, dateTime, money, tokens } from '../format'
import type { PageProps } from './Overview'

// The trend is charted over deployments regardless of which tab is open, and
// that is deliberate rather than lazy.
//
// A request has one deployment, so summing deployment buckets sums the money
// once. It also has a *chain* of scopes and is charged to every level, so a
// scope chart over every subject would draw an organisation and its teams on
// top of each other and double the height of every bar. Keys would total
// correctly too, but the trend asks one question — is the gateway's spend
// rising — and that question has one answer whichever table sits below it.
const TREND_KIND = 'deployment'

const RANGES = [
  { days: 7, label: '7d' },
  { days: 30, label: '30d' },
  { days: 90, label: '90d' },
] as const

const TABS = [
  { id: 'keys', label: 'Keys', note: 'What each virtual key consumed.' },
  {
    id: 'scopes',
    label: 'Organisations',
    note: "Each team's pooled spend. This is not the sum of its keys: a key can leave a team, and its historical spend does not leave with it, so the pool is its own ledger subject.",
  },
  { id: 'deployments', label: 'Deployments', note: 'What each upstream served.' },
] as const

export function Spend({ onUnauthorized }: PageProps) {
  const [tab, setTab] = useState<(typeof TABS)[number]['id']>('keys')
  const [days, setDays] = useState<number>(30)
  const { data, error } = useApi<SpendResponse>(`/spend/${tab}`, 15_000, onUnauthorized)
  // Polled far more slowly than the ledger above it: this is a month of
  // history, it moves by the day, and refetching every fifteen seconds would be
  // a range scan every fifteen seconds for a picture that had not changed.
  const from = new Date(Date.now() - days * 86_400_000).toISOString()
  const history = useApi<SpendHistoryResponse>(
    `/spend/history?kind=${TREND_KIND}&from=${encodeURIComponent(from)}&interval=day`,
    120_000,
    onUnauthorized,
  )
  const active = TABS.find((t) => t.id === tab)!
  const rows = data?.entries ?? []
  // A gateway keeping no history answers 404 here, which is not an error worth
  // showing as one: it is a feature that was not switched on, and the panel
  // says so in its own words.
  const historyOff = history.error !== null && !history.data

  return (
    <>
      <PageHead title="Spend">
        Consumption over each subject's current budget window. Cost covers only deployments you pay
        for; passthrough traffic bills the caller's own subscription and is reported as usage with no
        cost.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      <Panel
        title="Trend"
        actions={
          !historyOff && (
            <div className="tabs">
              {RANGES.map((r) => (
                <button
                  key={r.days}
                  className={`tab${r.days === days ? ' active' : ''}`}
                  onClick={() => setDays(r.days)}
                >
                  {r.label}
                </button>
              ))}
            </div>
          )
        }
        note={
          historyOff
            ? undefined
            : 'Daily cost across the whole gateway, from the durable history. Unlike the table below, a day here keeps its figure after the budget window it fell in has rolled over, which is what makes it a line rather than a number.'
        }
      >
        <div className="panel-body">
          {historyOff ? (
            <p className="hint">
              No spend history is being kept, so there is nothing to chart. Set{' '}
              <code className="mono">observability.spend_history.dsn</code> to record one — the
              ledger below holds only the current budget window, which is why every figure on this
              page is a number rather than a line.
            </p>
          ) : (
            <Trend buckets={history.data?.buckets ?? []} />
          )}
        </div>
      </Panel>

      <Panel
        title={
          <div className="tabs">
            {TABS.map((t) => (
              <button key={t.id} className={`tab${t.id === tab ? ' active' : ''}`} onClick={() => setTab(t.id)}>
                {t.label}
              </button>
            ))}
          </div>
        }
        actions={
          data && (
            <span className="hint">
              {money(data.total_cost)} charged · {money(data.total_cache_savings)} saved by prompt caching
            </span>
          )
        }
        note={active.note}
      >
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Subject</th>
                <th className="num">Requests</th>
                <th className="num">Billable</th>
                <th className="num">Input</th>
                <th className="num">Output</th>
                <th className="num">Cache read</th>
                <th className="num">Cache write</th>
                <th className="num">Savings</th>
                <th className="num">Cost</th>
                <th>Window since</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.subject}>
                  <td>
                    {row.alias || <span className="mono wrap-anywhere">{row.subject}</span>}
                    {row.alias && <div className="hint mono wrap-anywhere">{row.subject}</div>}
                  </td>
                  <td className="num">{count(row.requests)}</td>
                  <td className="num" title="requests the operator paid for; the rest were billed to a caller's own subscription">
                    {count(row.billable_requests)}
                  </td>
                  <td className="num">{tokens(row.input_tokens)}</td>
                  <td className="num">{tokens(row.output_tokens)}</td>
                  <td className="num">{tokens(row.cache_read_tokens)}</td>
                  <td className="num">{tokens(row.cache_write_tokens)}</td>
                  <td className="num" title="negative where caches were written and never read back">
                    {money(row.cache_savings)}
                  </td>
                  <td className="num">{money(row.cost)}</td>
                  <td className="hint">{dateTime(row.window_start)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {rows.length === 0 && <Empty>Nothing recorded in this ledger yet.</Empty>}
      </Panel>

      {data && (
        <Notice>
          <strong>Savings go negative</strong> where caches were written and never read back — which
          is what a conversation scattered across deployments costs. They sit beside cost rather than
          inside it: cost is what was charged, savings are what was not.
        </Notice>
      )}
    </>
  )
}
