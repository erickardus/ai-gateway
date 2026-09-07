import { useMemo, useState } from 'react'
import { api, type SpendHistoryResponse, type SpendResponse } from '../api'
import { useApi } from '../useApi'
import {
  Empty, Notice, PageHead, Panel, Search, Segmented, Skeleton, Stat, useToast,
} from '../components/ui'
import { BarList, ShareBar, TimeSeries } from '../components/charts'
import { Icon } from '../components/icons'
import { count, dateTime, money, percent, tokens } from '../format'
import type { PageProps } from './Overview'

/**
 * The trend is charted over deployments regardless of which tab is open, and
 * that is deliberate rather than lazy.
 *
 * A request has one deployment, so summing deployment buckets sums the money
 * once. It also has a *chain* of scopes and is charged to every level, so a
 * scope chart over every subject would draw an organisation and its teams on
 * top of each other and double the height of every bar. Keys would total
 * correctly too, but the trend asks one question — is the gateway's spend
 * rising — and that question has one answer whichever table sits below it.
 */
const TREND_KIND = 'deployment'

const RANGES = [
  { value: 7, label: '7d' },
  { value: 30, label: '30d' },
  { value: 90, label: '90d' },
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
  const [needle, setNeedle] = useState('')
  const [exporting, setExporting] = useState(false)
  const toast = useToast()

  const { data, error, loading } = useApi<SpendResponse>(`/spend/${tab}`, 15_000, onUnauthorized)

  // Polled far more slowly than the ledger above it: this is a month of
  // history, it moves by the day, and refetching every fifteen seconds would be
  // a range scan every fifteen seconds for a picture that had not changed.
  const from = useMemo(() => new Date(Date.now() - days * 86_400_000).toISOString(), [days])
  const history = useApi<SpendHistoryResponse>(
    `/spend/history?kind=${TREND_KIND}&from=${encodeURIComponent(from)}&interval=day`,
    120_000,
    onUnauthorized,
  )

  const active = TABS.find((t) => t.id === tab)!
  const allRows = data?.entries ?? []
  const rows = useMemo(() => {
    const term = needle.trim().toLowerCase()
    if (!term) return allRows
    return allRows.filter((r) =>
      r.alias?.toLowerCase().includes(term) || r.subject.toLowerCase().includes(term))
  }, [allRows, needle])

  // A gateway keeping no history answers 404 here, which is not an error worth
  // showing as one: it is a feature that was not switched on, and the panel
  // says so in its own words.
  const historyOff = !!history.error && !history.data

  // Buckets arrive one per subject per day. A chart of "what did we spend"
  // wants them summed per day, and summing here rather than asking the gateway
  // for it keeps the endpoint answering one shape.
  const daily = useMemo(() => {
    const byDay = new Map<string, { cost: number; savings: number; requests: number }>()
    for (const b of history.data?.buckets ?? []) {
      const day = b.start.slice(0, 10)
      const acc = byDay.get(day) ?? { cost: 0, savings: 0, requests: 0 }
      acc.cost += b.cost
      acc.savings += b.cache_savings
      acc.requests += b.requests
      byDay.set(day, acc)
    }
    return [...byDay.entries()]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([day, v]) => ({ start: day + 'T00:00:00Z', ...v }))
  }, [history.data])

  const totalTokens = allRows.reduce((sum, r) => sum + r.input_tokens + r.output_tokens, 0)

  const exportCsv = async () => {
    setExporting(true)
    try {
      await api.download(
        `/spend/export?from=${encodeURIComponent(from)}`,
        `spend-${from.slice(0, 10)}-to-${new Date().toISOString().slice(0, 10)}.csv`,
      )
      toast.ok('Export downloaded — one row per request over the selected range.')
    } catch (err) {
      toast.bad(err instanceof Error ? err.message : String(err))
    } finally {
      setExporting(false)
    }
  }

  return (
    <>
      <PageHead
        title="Spend"
        actions={
          <button onClick={exportCsv} disabled={exporting || historyOff}
            title={historyOff ? 'Needs observability.spend_history.dsn' : 'Download the per-request rows as CSV'}>
            <Icon.download size={15} /> {exporting ? 'Exporting…' : 'Export CSV'}
          </button>
        }
      >
        Consumption over each subject's current budget window. Cost covers only deployments you pay
        for; passthrough traffic bills the caller's own subscription and is reported as usage with no
        cost.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      <div className="grid grid-4 mb">
        <Stat
          label="Charged this window"
          value={money(data?.total_cost ?? 0)}
          sub={`across ${count(allRows.length)} ${active.label.toLowerCase()}`}
        />
        <Stat
          label="Saved by prompt caching"
          value={money(data?.total_cache_savings ?? 0)}
          sub="what was not charged; not included above"
        />
        <Stat
          label="Requests"
          value={count(allRows.reduce((sum, r) => sum + r.requests, 0))}
          sub={`${percent(
            allRows.reduce((sum, r) => sum + r.billable_requests, 0),
            allRows.reduce((sum, r) => sum + r.requests, 0),
          )} billable to you`}
        />
        <Stat label="Tokens" value={tokens(totalTokens)} sub="input + output" />
      </div>

      <Panel
        title="Daily cost"
        actions={!historyOff && <Segmented options={RANGES} value={days} onChange={setDays} />}
        note={
          historyOff
            ? undefined
            : 'From the durable history. Unlike the table below, a day here keeps its figure after the budget window it fell in has rolled over, which is what makes it a line rather than a number.'
        }
      >
        <div className="panel-body">
          {historyOff ? (
            <Empty icon={false}>
              <strong>No spend history is being kept.</strong>
              <span style={{ maxWidth: '58ch', display: 'inline-block' }}>
                Set <code>observability.spend_history.dsn</code> to record one. The ledger below holds
                only the current budget window, which is why every figure on this page is a number
                rather than a line.
              </span>
            </Empty>
          ) : (
            <TimeSeries
              points={daily}
              height={200}
              mode="bars"
              yFormat={(v) => money(v)}
              series={[{ key: 'cost', label: 'Cost', color: 'var(--chart-accent)', format: money }]}
              empty="No spend recorded over this range."
            />
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
        actions={<Search value={needle} onChange={setNeedle} placeholder="Subject or alias…" />}
        note={active.note}
      >
        {loading && allRows.length === 0 ? <Skeleton rows={4} cols={7} /> : (
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
                      {row.alias && <div className="cell-sub mono wrap-anywhere">{row.subject}</div>}
                    </td>
                    <td className="num">{count(row.requests)}</td>
                    <td className="num" title="requests the operator paid for; the rest were billed to a caller's own subscription">
                      {count(row.billable_requests)}
                    </td>
                    <td className="num">{tokens(row.input_tokens)}</td>
                    <td className="num">{tokens(row.output_tokens)}</td>
                    <td className="num">{tokens(row.cache_read_tokens)}</td>
                    <td className="num">{tokens(row.cache_write_tokens)}</td>
                    <td className="num" title="negative where caches were written and never read back"
                      style={{ color: row.cache_savings < 0 ? 'var(--bad)' : undefined }}>
                      {money(row.cache_savings)}
                    </td>
                    <td className="num">{money(row.cost)}</td>
                    <td className="hint nowrap" style={{ marginTop: 0 }}>{dateTime(row.window_start)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {!loading && rows.length === 0 && (
          <Empty>
            <strong>{allRows.length === 0 ? 'Nothing recorded in this ledger yet.' : 'Nothing matches that search.'}</strong>
          </Empty>
        )}
      </Panel>

      <div className="split">
        <Panel title={`Cost by ${active.label.toLowerCase().replace(/s$/, '')}`}>
          <div className="panel-body" style={{ padding: 8 }}>
            <BarList
              rows={[...allRows]
                .filter((r) => r.cost > 0)
                .sort((a, b) => b.cost - a.cost)
                .slice(0, 8)
                .map((r) => ({
                  name: r.alias || r.subject,
                  value: r.cost,
                  sub: `${count(r.requests)} req`,
                }))}
              format={money}
              empty="No billable spend in this ledger."
            />
          </div>
        </Panel>

        <Panel title="Where the tokens went">
          <div className="panel-body">
            <ShareBar
              parts={[
                { label: 'Input', value: allRows.reduce((s, r) => s + r.input_tokens, 0), color: 'var(--chart-1)' },
                { label: 'Output', value: allRows.reduce((s, r) => s + r.output_tokens, 0), color: 'var(--chart-2)' },
                { label: 'Cache read', value: allRows.reduce((s, r) => s + r.cache_read_tokens, 0), color: 'var(--chart-3)' },
                { label: 'Cache write', value: allRows.reduce((s, r) => s + r.cache_write_tokens, 0), color: 'var(--chart-4)' },
              ]}
              format={(v) => tokens(v) + ' tok'}
            />
            <p className="hint" style={{ marginTop: 16 }}>
              <strong>Savings go negative</strong> where caches were written and never read back —
              which is what a conversation scattered across deployments costs. They sit beside cost
              rather than inside it: cost is what was charged, savings are what was not.
            </p>
          </div>
        </Panel>
      </div>
    </>
  )
}
