import { useMemo, useState } from 'react'
import type { TrafficRecord, TrafficResponse } from '../api'
import { useApi, useRefresh } from '../useApi'
import { Drawer, Empty, Notice, PageHead, Panel, Pill, Search, Segmented } from '../components/ui'
import { Sparkline } from '../components/charts'
import { bytes, count, dateTime, money, ms, percent, shortHash, timestamp, tokens } from '../format'
import { outcomeTone, type PageProps } from './Overview'

/**
 * costCell distinguishes the three ways a request can cost nothing: it was
 * billed to the caller's own subscription, it never reached an upstream, or it
 * really did cost nothing. A bare dash for all three would hide the first,
 * which is the arrangement this gateway exists to serve.
 */
function costCell(r: TrafficRecord) {
  if (r.billable) return money(r.cost)
  if (r.outcome === 'cache_hit') return <span title="served from the response cache — no upstream was called">—</span>
  if (!r.deployment) return <span title="refused before dispatch — no upstream was called">—</span>
  return <span title="passthrough: billed to the caller's own subscription">—</span>
}

const VIEWS = [
  { value: 'all', label: 'All' },
  { value: 'errors', label: 'Failed' },
  { value: 'slow', label: 'Slowest' },
] as const

type View = (typeof VIEWS)[number]['value']

export function Traffic({ onUnauthorized }: PageProps) {
  const [view, setView] = useState<View>('all')
  const [group, setGroup] = useState('')
  const [needle, setNeedle] = useState('')
  const [selected, setSelected] = useState<TrafficRecord>()
  const { paused } = useRefresh()

  const query = new URLSearchParams({ limit: '300' })
  if (view === 'errors') query.set('errors', 'true')
  if (group) query.set('model_group', group)

  const { data, error, loading } = useApi<TrafficResponse>(
    `/traffic?${query.toString()}`, 3_000, onUnauthorized,
  )

  const records = data?.records ?? []
  const groups = useMemo(
    () => [...new Set(records.map((r) => r.model_group).filter(Boolean))] as string[],
    [records],
  )

  // Filtering and sorting happen here rather than in the gateway because the
  // whole page is already in memory: asking the server to re-scan its ring for
  // a substring the browser can match itself would add a round trip per
  // keystroke to answer a question already answered.
  const rows = useMemo(() => {
    const term = needle.trim().toLowerCase()
    let list = records
    if (term) {
      list = list.filter((r) =>
        [r.key_alias, r.spend_subject, r.model_group, r.deployment, r.outcome, r.reject_reason, r.scope]
          .some((field) => field?.toLowerCase().includes(term)))
    }
    if (view === 'slow') list = [...list].sort((a, b) => b.latency_ms - a.latency_ms)
    return list
  }, [records, needle, view])

  const summary = useMemo(() => {
    const served = records.filter((r) => r.deployment && r.outcome !== 'cache_hit')
    const latencies = served.map((r) => r.latency_ms).sort((a, b) => a - b)
    const at = (q: number) => (latencies.length ? latencies[Math.min(latencies.length - 1, Math.floor(q * latencies.length))] : 0)
    const failed = records.filter((r) => r.outcome !== 'success' && r.outcome !== 'cache_hit').length
    return {
      failed,
      cost: records.reduce((sum, r) => sum + (r.billable ? r.cost : 0), 0),
      tokens: records.reduce((sum, r) => sum + (r.usage.input_tokens ?? 0) + (r.usage.output_tokens ?? 0), 0),
      p50: at(0.5),
      p95: at(0.95),
      retried: records.filter((r) => r.retries > 0 || r.fallbacks > 0).length,
    }
  }, [records])

  return (
    <>
      <PageHead title="Traffic">
        The last {count(data?.capacity ?? 0)} requests this instance served, with the routing decision
        each one produced. Metadata only — no request or response bodies are retained.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      <Panel>
        <div className="panel-body" style={{ display: 'flex', gap: 26, flexWrap: 'wrap', alignItems: 'center' }}>
          <Figure label="Held" value={count(data?.held ?? 0)} sub={`of ${count(data?.capacity ?? 0)}`} />
          <Figure
            label="Failed"
            value={count(summary.failed)}
            sub={percent(summary.failed, records.length) + ' of buffer'}
            tone={summary.failed > 0 ? 'bad' : undefined}
          />
          <Figure label="Latency p50" value={ms(summary.p50)} sub={`p95 ${ms(summary.p95)}`} />
          <Figure label="Tokens" value={tokens(summary.tokens)} sub="in + out" />
          <Figure label="Cost" value={money(summary.cost)} sub="billable only" />
          <Figure
            label="Re-routed"
            value={count(summary.retried)}
            sub="retried or fell back"
            tone={summary.retried > 0 ? 'warn' : undefined}
          />
          <div className="spacer" />
          <div style={{ width: 130 }}>
            <div className="hint" style={{ marginTop: 0, marginBottom: 2 }}>Latency, newest right</div>
            <Sparkline
              values={[...records].reverse().map((r) => r.latency_ms)}
              color="var(--chart-1)"
              height={26}
              label="latency of each held request"
            />
          </div>
        </div>
      </Panel>

      <Panel
        title={
          <>
            {count(rows.length)} shown
            {needle && <span className="hint" style={{ marginTop: 0 }}>filtered from {count(records.length)}</span>}
          </>
        }
        actions={
          <div className="toolbar">
            <Search value={needle} onChange={setNeedle} placeholder="Key, deployment, outcome…" />
            <select value={group} onChange={(e) => setGroup(e.target.value)}>
              <option value="">All model groups</option>
              {groups.map((g) => <option key={g} value={g}>{g}</option>)}
            </select>
            <Segmented options={VIEWS} value={view} onChange={setView} />
          </div>
        }
      >
        <div className="table-scroll tall">
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
              {rows.map((r) => (
                <tr
                  key={r.id + r.at}
                  className={`clickable${selected?.id === r.id ? ' selected' : ''}`}
                  onClick={() => setSelected(r)}
                >
                  <td className="mono nowrap">{timestamp(r.at)}</td>
                  <td className="nowrap">
                    {r.key_alias || <span className="mono">{shortHash(r.spend_subject)}</span>}
                    {r.scope && <div className="cell-sub mono">{r.scope}</div>}
                  </td>
                  <td className="nowrap">{r.model_group || '—'}</td>
                  <td className="mono wrap-anywhere">{r.deployment || '—'}</td>
                  <td>
                    <Pill tone={outcomeTone(r.outcome)} title={r.reject_reason} dot>
                      {r.reject_reason?.replace(/_/g, ' ') || r.outcome.replace(/_/g, ' ')}
                      {r.status_class ? ` ${r.status_class}` : ''}
                    </Pill>
                  </td>
                  <td>
                    <div className="pill-row">
                      {r.retries > 0 && <Pill tone="warn">{r.retries} retr{r.retries === 1 ? 'y' : 'ies'}</Pill>}
                      {r.fallbacks > 0 && <Pill tone="warn">{r.fallbacks} fallback</Pill>}
                      {r.prompt_affinity && (
                        <Pill
                          tone={r.prompt_affinity === 'hit' ? 'ok' : 'neutral'}
                          title="prompt-prefix pin: hit means the request landed on the deployment already holding its prefix"
                        >
                          pin {r.prompt_affinity}
                        </Pill>
                      )}
                      {r.streaming && <Pill tone="neutral">stream</Pill>}
                    </div>
                  </td>
                  <td className="num">{tokens((r.usage.input_tokens ?? 0) + (r.usage.output_tokens ?? 0))}</td>
                  <td className="num">{ms(r.latency_ms)}</td>
                  <td className="num">{costCell(r)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {rows.length === 0 && !loading && (
          <Empty>
            <strong>{records.length === 0 ? 'No requests recorded yet.' : 'Nothing matches that filter.'}</strong>
            {records.length === 0
              ? 'This buffer fills as traffic flows through this instance.'
              : 'Clear the search or widen the range.'}
          </Empty>
        )}
      </Panel>

      {paused && <Notice>Automatic refresh is paused, so this table is a snapshot. Resume it from the toolbar.</Notice>}

      {selected && <RecordDrawer record={selected} onClose={() => setSelected(undefined)} />}
    </>
  )
}

function Figure({ label, value, sub, tone }: {
  label: string
  value: string
  sub?: string
  tone?: 'bad' | 'warn'
}) {
  return (
    <div>
      <div className="stat-label">{label}</div>
      <div style={{
        fontSize: 19,
        fontWeight: 620,
        letterSpacing: '-0.02em',
        marginTop: 2,
        color: tone === 'bad' ? 'var(--bad)' : tone === 'warn' ? 'var(--warn)' : undefined,
      }}>
        {value}
      </div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  )
}

function RecordDrawer({ record: r, onClose }: { record: TrafficRecord; onClose: () => void }) {
  const totalTokens = (r.usage.input_tokens ?? 0) + (r.usage.output_tokens ?? 0)

  // A cache hit is recorded against the sentinel deployment "cache" and carries
  // no cost, so the two obvious tests — "has a deployment" and "is not
  // billable" — both say passthrough about it. They are opposite facts: a
  // passthrough request was answered by an upstream on the caller's own
  // credential, and this one was answered by this process out of memory.
  const calledUpstream = !!r.deployment && r.outcome !== 'cache_hit'

  return (
    <Drawer
      title={r.model_group || 'request'}
      subtitle={<>{dateTime(r.at)} · <span className="mono">{r.id}</span></>}
      onClose={onClose}
    >
      <div className="pill-row" style={{ marginTop: 14 }}>
        <Pill tone={outcomeTone(r.outcome)} dot>
          {r.reject_reason?.replace(/_/g, ' ') || r.outcome.replace(/_/g, ' ')}
        </Pill>
        {r.status_class && <Pill tone="neutral">upstream {r.status_class}</Pill>}
        {r.streaming && <Pill tone="neutral">streamed</Pill>}
        {r.format && <Pill tone="neutral">{r.format}</Pill>}
        {!r.billable && calledUpstream && <Pill tone="accent">passthrough</Pill>}
      </div>

      <p className="section-label">Routing</p>
      <dl className="kv">
        <dt>Deployment</dt><dd>{r.deployment || '—'}</dd>
        <dt>Wire format</dt><dd>{r.format || '—'}</dd>
        <dt>Retries</dt><dd>{r.retries}</dd>
        <dt>Fallbacks</dt><dd>{r.fallbacks}</dd>
        <dt>Prefix pin</dt><dd>{r.prompt_affinity || 'not consulted'}</dd>
      </dl>
      {(r.retries > 0 || r.fallbacks > 0) && (
        <p className="hint">
          This request did not go where it was first sent. {r.retries > 0 && `It was retried ${r.retries} time${r.retries === 1 ? '' : 's'}. `}
          {r.fallbacks > 0 && `It fell back ${r.fallbacks} time${r.fallbacks === 1 ? '' : 's'} to another group. `}
          The latency below is the whole journey, not the attempt that answered.
        </p>
      )}

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
        <dt>Effective rate</dt>
        <dd>
          {/* Only for a request that actually generated something. Dividing a
              token count by the microsecond a cache hit took reports hundreds
              of millions of tokens a second, which is a true division and a
              false claim about the gateway. */}
          {calledUpstream && totalTokens > 0 && r.latency_ms >= 1
            ? `${(totalTokens / (r.latency_ms / 1000)).toFixed(0)} tok/s end to end`
            : r.outcome === 'cache_hit' ? 'not generated — served from cache' : '—'}
        </dd>
        <dt>Request bytes</dt><dd>{bytes(r.request_bytes)}</dd>
        <dt>Response bytes</dt><dd>{bytes(r.response_bytes)}</dd>
      </dl>
    </Drawer>
  )
}
