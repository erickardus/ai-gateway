import { useState } from 'react'
import type { SpendResponse } from '../api'
import { useApi } from '../useApi'
import { Empty, Notice, PageHead, Panel } from '../components/ui'
import { count, dateTime, money, tokens } from '../format'
import type { PageProps } from './Overview'

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
  const { data, error } = useApi<SpendResponse>(`/spend/${tab}`, 15_000, onUnauthorized)
  const active = TABS.find((t) => t.id === tab)!
  const rows = data?.entries ?? []

  return (
    <>
      <PageHead title="Spend">
        Consumption over each subject's current budget window. Cost covers only deployments you pay
        for; passthrough traffic bills the caller's own subscription and is reported as usage with no
        cost.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

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
