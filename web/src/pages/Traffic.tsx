import { useState } from 'react'
import type { TrafficRecord, TrafficResponse } from '../api'
import { useApi } from '../useApi'
import { Empty, Notice, PageHead, Panel, Pill } from '../components/ui'
import { count, dateTime, money, ms, shortHash, timestamp, tokens } from '../format'
import type { PageProps } from './Overview'

// outcomeTone maps a recorded outcome onto the three colours an operator scans
// for. Anything unrecognised stays neutral rather than being guessed at.
function outcomeTone(outcome: string): 'ok' | 'warn' | 'bad' | 'neutral' {
  switch (outcome) {
    case 'success': return 'ok'
    case 'cache_hit': return 'ok'
    case 'rejected': return 'warn'
    case 'upstream_error': case 'gateway_error': return 'bad'
    default: return 'neutral'
  }
}

// costCell distinguishes the three ways a request can cost nothing: it was
// billed to the caller's own subscription, it never reached an upstream, or it
// really did cost nothing. A bare dash for all three would hide the first,
// which is the arrangement this gateway exists to serve.
function costCell(r: TrafficRecord) {
  if (r.billable) return money(r.cost)
  if (r.outcome === 'cache_hit') return <span title="served from the response cache — no upstream was called">—</span>
  if (!r.deployment) return <span title="refused before dispatch — no upstream was called">—</span>
  return <span title="passthrough: billed to the caller's own subscription">—</span>
}

export function Traffic({ onUnauthorized }: PageProps) {
  const [errorsOnly, setErrorsOnly] = useState(false)
  const [group, setGroup] = useState('')
  const [live, setLive] = useState(true)
  const [selected, setSelected] = useState<TrafficRecord>()

  const query = new URLSearchParams({ limit: '200' })
  if (errorsOnly) query.set('errors', 'true')
  if (group) query.set('model_group', group)

  const { data, error } = useApi<TrafficResponse>(
    `/traffic?${query.toString()}`,
    live ? 3_000 : 0,
    onUnauthorized,
  )

  const records = data?.records ?? []
  const groups = [...new Set(records.map((r) => r.model_group).filter(Boolean))] as string[]

  return (
    <>
      <PageHead title="Traffic">
        The last {count(data?.capacity ?? 0)} requests this instance served, with the routing decision
        each one produced. Metadata only — no request or response bodies are retained.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      <Panel
        title={`${count(data?.held ?? 0)} held`}
        actions={
          <div className="toolbar">
            <select value={group} onChange={(e) => setGroup(e.target.value)}>
              <option value="">All model groups</option>
              {groups.map((g) => <option key={g} value={g}>{g}</option>)}
            </select>
            <label className="checkbox" style={{ margin: 0 }}>
              <input type="checkbox" checked={errorsOnly} onChange={(e) => setErrorsOnly(e.target.checked)} />
              Errors only
            </label>
            <label className="checkbox" style={{ margin: 0 }}>
              <input type="checkbox" checked={live} onChange={(e) => setLive(e.target.checked)} />
              Live
            </label>
          </div>
        }
      >
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>Key</th>
                <th>Group</th>
                <th>Deployment</th>
                <th>Outcome</th>
                <th>Routing</th>
                <th className="num">Tokens</th>
                <th className="num">Latency</th>
                <th className="num">Cost</th>
              </tr>
            </thead>
            <tbody>
              {records.map((r) => (
                <tr
                  key={r.id + r.at}
                  className={`clickable${selected?.id === r.id ? ' selected' : ''}`}
                  onClick={() => setSelected(r)}
                >
                  <td className="mono">{timestamp(r.at)}</td>
                  <td>{r.key_alias || <span className="mono">{shortHash(r.spend_subject)}</span>}</td>
                  <td>{r.model_group || '—'}</td>
                  <td className="mono wrap-anywhere">{r.deployment || '—'}</td>
                  <td>
                    <Pill tone={outcomeTone(r.outcome)} title={r.reject_reason}>
                      {r.reject_reason || r.outcome}{r.status_class ? ` ${r.status_class}` : ''}
                    </Pill>
                  </td>
                  <td>
                    <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                      {r.retries > 0 && <Pill tone="warn">{r.retries} retr{r.retries === 1 ? 'y' : 'ies'}</Pill>}
                      {r.fallbacks > 0 && <Pill tone="warn">{r.fallbacks} fallback</Pill>}
                      {r.prompt_affinity && (
                        <Pill tone={r.prompt_affinity === 'hit' ? 'ok' : 'neutral'} title="prompt-prefix pin: hit means the request landed on the deployment already holding its prefix">
                          pin {r.prompt_affinity}
                        </Pill>
                      )}
                      {r.streaming && <Pill tone="neutral">stream</Pill>}
                    </div>
                  </td>
                  <td className="num">
                    {tokens((r.usage.input_tokens ?? 0) + (r.usage.output_tokens ?? 0))}
                  </td>
                  <td className="num">{ms(r.latency_ms)}</td>
                  <td className="num">{costCell(r)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {records.length === 0 && (
          <Empty>
            No requests recorded yet. This buffer fills as traffic flows through this instance.
          </Empty>
        )}
      </Panel>

      {selected && <RecordDrawer record={selected} onClose={() => setSelected(undefined)} />}
    </>
  )
}

function RecordDrawer({ record, onClose }: { record: TrafficRecord; onClose: () => void }) {
  const r = record
  return (
    <div className="drawer-backdrop" onClick={onClose}>
      <div className="drawer" onClick={(e) => e.stopPropagation()}>
        <h2>{r.model_group || 'request'}</h2>
        <p className="hint">{dateTime(r.at)} · {r.id}</p>

        <p className="section-label">Routing</p>
        <dl className="kv">
          <dt>Deployment</dt><dd>{r.deployment || '—'}</dd>
          <dt>Format</dt><dd>{r.format || '—'}</dd>
          <dt>Outcome</dt><dd>{r.outcome}{r.reject_reason ? ` (${r.reject_reason})` : ''}</dd>
          <dt>Upstream status</dt><dd>{r.status_class || '—'}</dd>
          <dt>Retries</dt><dd>{r.retries}</dd>
          <dt>Fallbacks</dt><dd>{r.fallbacks}</dd>
          <dt>Prefix pin</dt><dd>{r.prompt_affinity || 'not consulted'}</dd>
        </dl>

        <p className="section-label">Caller</p>
        <dl className="kv">
          <dt>Key alias</dt><dd>{r.key_alias || '—'}</dd>
          <dt>Spend subject</dt><dd>{r.spend_subject || '—'}</dd>
          <dt>Scope</dt><dd>{r.scope || 'none'}</dd>
        </dl>

        <p className="section-label">Usage</p>
        <dl className="kv">
          <dt>Input</dt><dd>{count(r.usage.input_tokens)}</dd>
          <dt>Output</dt><dd>{count(r.usage.output_tokens)}</dd>
          <dt>Cache read</dt><dd>{count(r.usage.cache_read_tokens)}</dd>
          <dt>Cache write</dt><dd>{count(r.usage.cache_write_tokens)}</dd>
          <dt>Cost</dt>
          <dd>
            {r.billable
              ? money(r.cost)
              : r.outcome === 'cache_hit'
                ? 'served from the response cache'
                : r.deployment
                  ? 'not billable — passthrough bills the caller'
                  : 'refused before dispatch'}
          </dd>
          <dt>Cache savings</dt><dd>{r.billable ? money(r.cache_savings) : '—'}</dd>
        </dl>

        <p className="section-label">Timing</p>
        <dl className="kv">
          <dt>Total latency</dt><dd>{ms(r.latency_ms)}</dd>
          <dt>Time to first token</dt><dd>{ms(r.ttft_ms)}</dd>
          <dt>Streamed</dt>
          <dd>{r.streaming ? (r.stream_completed ? 'yes, completed' : 'yes, did not complete') : 'no'}</dd>
          <dt>Throughput</dt>
          <dd>{r.throughput_tps ? `${r.throughput_tps.toFixed(1)} tok/s` : r.streaming ? '—' : 'not streamed'}</dd>
          <dt>Request bytes</dt><dd>{count(r.request_bytes)}</dd>
          <dt>Response bytes</dt><dd>{count(r.response_bytes)}</dd>
        </dl>

        <div className="button-row" style={{ marginTop: 22 }}>
          <button onClick={onClose}>Close</button>
        </div>
      </div>
    </div>
  )
}
