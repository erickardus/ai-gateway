import { useMemo, useState } from 'react'
import type { AuditRecord, AuditResponse } from '../api'
import { useApi } from '../useApi'
import { Drawer, Empty, Notice, PageHead, Panel, Pill, Search, Skeleton } from '../components/ui'
import { BarList } from '../components/charts'
import { ago, dateTime, shortHash } from '../format'
import type { PageProps } from './Overview'

// Actions are grouped by what they are evidence of, which is the question
// someone opens this page with: who got access, who lost it, and who was
// looking. Anything unrecognised falls through as neutral rather than being
// guessed at, because a new action type shown in the wrong colour is worse than
// one shown in none.
function actionTone(action: string): 'ok' | 'warn' | 'bad' | 'info' | 'neutral' {
  if (action.startsWith('key.generate') || action.startsWith('sso.')) return 'ok'
  if (action.startsWith('key.delete')) return 'bad'
  if (action.startsWith('key.update') || action.startsWith('cache.')) return 'warn'
  if (action.startsWith('console.')) return 'info'
  return 'neutral'
}

export function Audit({ onUnauthorized }: PageProps) {
  const { data, error, loading } = useApi<AuditResponse>('/audit?limit=300', 30_000, onUnauthorized)
  const [needle, setNeedle] = useState('')
  const [selected, setSelected] = useState<AuditRecord>()

  const records = useMemo(() => data?.records ?? [], [data])

  const rows = useMemo(() => {
    const term = needle.trim().toLowerCase()
    if (!term) return records
    return records.filter((r) =>
      [r.action, r.target, r.target_kind, r.outcome, r.actor?.kind, r.actor?.id, r.actor?.subject, r.actor?.remote]
        .some((field) => field?.toLowerCase().includes(term)))
  }, [records, needle])

  const byAction = useMemo(() => {
    const counts = new Map<string, number>()
    for (const r of records) counts.set(r.action, (counts.get(r.action) ?? 0) + 1)
    return [...counts.entries()]
      .sort(([, a], [, b]) => b - a)
      .map(([name, value]) => ({ name, value }))
  }, [records])

  const failures = records.filter((r) => r.outcome !== 'success').length

  return (
    <>
      <PageHead title="Audit">
        Every administrative action, hash-chained so that a record removed or altered after the fact
        does not verify. Inference is not audited here — this is who administered the gateway, not
        who used it.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      {data && !data.readable && (
        <Notice tone="warn">
          <strong>This sink cannot be read back.</strong> {data.note}
        </Notice>
      )}

      {data?.sealed && (
        <Notice tone="bad">
          <strong>The chain is sealed.</strong> {data.seal_reason} The gateway keeps serving traffic
          and refuses every new audit record, which is what makes an administrative action fail
          rather than go unrecorded.
        </Notice>
      )}

      {data?.readable && data.verified === false && !data.sealed && (
        <Notice tone="bad">
          <strong>Verification failed.</strong> {data.verify_error}
        </Notice>
      )}

      {data?.readable && data.verified && (
        <Notice tone="ok">
          Chain verified through sequence {data.verified_through}. Every record links to the hash of
          the one before it, and all of them check out.
        </Notice>
      )}

      {data?.readable && (
        <div className="split mb">
          <Panel
            title={
              <>
                {rows.length} record{rows.length === 1 ? '' : 's'}
                {rows.length !== records.length && (
                  <span className="hint" style={{ marginTop: 0 }}>of {records.length}</span>
                )}
              </>
            }
            actions={
              <div className="toolbar">
                <Pill tone="neutral">{data.sink} sink</Pill>
                {failures > 0 && <Pill tone="bad" dot>{failures} failed</Pill>}
                <Search value={needle} onChange={setNeedle} placeholder="Action, actor, target…" />
              </div>
            }
          >
            {loading && records.length === 0 ? <Skeleton rows={6} cols={5} /> : (
              <div className="table-scroll tall">
                <table>
                  <thead>
                    <tr>
                      <th className="num">Seq</th>
                      <th>When</th>
                      <th>Action</th>
                      <th>Actor</th>
                      <th>Target</th>
                      <th>Outcome</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((r) => (
                      <tr key={r.seq} className="clickable" onClick={() => setSelected(r)}>
                        <td className="num">{r.seq}</td>
                        <td className="nowrap" title={dateTime(r.at)}>{ago(r.at)}</td>
                        <td><Pill tone={actionTone(r.action)}>{r.action}</Pill></td>
                        <td>
                          {r.actor?.subject || r.actor?.id || r.actor?.kind || '—'}
                          {r.actor?.remote && <div className="cell-sub mono">{r.actor.remote}</div>}
                        </td>
                        <td className="mono wrap-anywhere">
                          {r.target_kind === 'key' ? shortHash(r.target) : r.target || '—'}
                          {r.target_kind && <div className="cell-sub">{r.target_kind}</div>}
                        </td>
                        <td>
                          <Pill tone={r.outcome === 'success' ? 'ok' : 'bad'} dot>{r.outcome}</Pill>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            {!loading && rows.length === 0 && (
              <Empty>
                <strong>{records.length === 0 ? 'Nothing recorded yet.' : 'Nothing matches that search.'}</strong>
                {records.length === 0 && 'The log fills as keys are issued, blocked and revoked.'}
              </Empty>
            )}
          </Panel>

          <div className="stack">
            <Panel title="What has been done">
              <div className="panel-body" style={{ padding: 8 }}>
                <BarList rows={byAction} empty="No actions recorded." />
              </div>
            </Panel>

            <Panel title="The chain">
              <div className="panel-body">
                <dl className="kv" style={{ gridTemplateColumns: '1fr auto', marginTop: 0 }}>
                  <dt>Sink</dt><dd>{data.sink}</dd>
                  <dt>Records held</dt><dd>{data.count}</dd>
                  <dt>Verified</dt>
                  <dd>{data.verified === undefined ? 'not checked' : data.verified ? 'yes' : 'no'}</dd>
                  {data.verified_through !== undefined && (
                    <>
                      <dt>Through sequence</dt><dd>{data.verified_through}</dd>
                    </>
                  )}
                </dl>
                <p className="hint" style={{ marginTop: 14 }}>{data.note}</p>
              </div>
            </Panel>
          </div>
        </div>
      )}

      {selected && <RecordDrawer record={selected} onClose={() => setSelected(undefined)} />}
    </>
  )
}

function RecordDrawer({ record: r, onClose }: { record: AuditRecord; onClose: () => void }) {
  const detail = Object.entries(r.detail ?? {})
  return (
    <Drawer
      title={r.action}
      subtitle={<>Sequence {r.seq} · {dateTime(r.at)}</>}
      onClose={onClose}
    >
      <div className="pill-row" style={{ marginTop: 14 }}>
        <Pill tone={r.outcome === 'success' ? 'ok' : 'bad'} dot>{r.outcome}</Pill>
        {r.target_kind && <Pill tone="neutral">{r.target_kind}</Pill>}
      </div>

      {r.error && <Notice tone="bad">{r.error}</Notice>}

      <p className="section-label">Actor</p>
      <dl className="kv">
        <dt>Kind</dt><dd>{r.actor?.kind || '—'}</dd>
        <dt>Identity</dt><dd>{r.actor?.subject || r.actor?.id || '—'}</dd>
        <dt>Remote address</dt><dd>{r.actor?.remote || '—'}</dd>
      </dl>

      <p className="section-label">Target</p>
      <dl className="kv">
        <dt>Kind</dt><dd>{r.target_kind || '—'}</dd>
        <dt>Target</dt><dd>{r.target || '—'}</dd>
      </dl>

      {detail.length > 0 && (
        <>
          <p className="section-label">Detail</p>
          <dl className="kv">
            {detail.map(([k, v]) => (
              <span key={k} style={{ display: 'contents' }}>
                <dt>{k.replace(/_/g, ' ')}</dt><dd>{v}</dd>
              </span>
            ))}
          </dl>
        </>
      )}

      <p className="section-label">Chain</p>
      <dl className="kv">
        <dt>This record</dt><dd className="wrap-anywhere">{r.hash}</dd>
        <dt>Previous</dt><dd className="wrap-anywhere">{r.prev || '— first in the chain'}</dd>
      </dl>
      <p className="hint">
        Each hash covers the record's own fields and the hash before it. Editing any record in the
        middle changes every hash after it, which is what makes a quiet deletion detectable rather
        than merely unlikely.
      </p>
    </Drawer>
  )
}
