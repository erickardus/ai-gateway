import { useState } from 'react'
import { api, type ConfigResponse, type Overview as OverviewData } from '../api'
import { useApi } from '../useApi'
import { Notice, PageHead, Panel, Pill, useToast } from '../components/ui'
import { bytes, count, seconds } from '../format'
import type { PageProps } from './Overview'

export function Ops({ onUnauthorized }: PageProps) {
  const { data, error, reload } = useApi<OverviewData>('/overview', 15_000, onUnauthorized)
  const config = useApi<ConfigResponse>('/config', 60_000, onUnauthorized)
  const [confirming, setConfirming] = useState(false)
  const toast = useToast()

  const c = config.data

  const purge = async () => {
    setConfirming(false)
    try {
      await api.post('/cache/purge')
      toast.ok('Response cache emptied. The next request of each kind is real again.')
      reload()
      config.reload()
    } catch (err) {
      toast.bad(err instanceof Error ? err.message : String(err))
    }
  }

  return (
    <>
      <PageHead title="Ops">
        What this instance has turned on, and the one lever the console offers. Everything else is
        configuration: the gateway reads gateway.yaml at start, and this page does not write it.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      {data && (
        <Panel title="Features">
          <div className="panel-body">
            <div className="pill-row" style={{ gap: 7 }}>
              {Object.entries(data.features).map(([name, on]) => (
                <Pill key={name} tone={on ? 'ok' : 'neutral'} dot>
                  {name.replace(/_/g, ' ')}
                </Pill>
              ))}
            </div>
            <p className="hint" style={{ marginTop: 12 }}>
              A dimmed feature is off in this instance's configuration, not broken. Every one of them
              is a decision made in gateway.yaml and read once, at start.
            </p>
          </div>
        </Panel>
      )}

      <div className="split mb">
        <Panel title="Response cache">
          <div className="panel-body">
            {data?.features.response_cache ? (
              <>
                <dl className="kv" style={{ gridTemplateColumns: '1fr auto', marginTop: 0 }}>
                  <dt>Scope</dt><dd>{c?.response_cache.scope ?? '—'}</dd>
                  <dt>Entry lifetime</dt><dd>{seconds(c?.response_cache.ttl_seconds)}</dd>
                  <dt>Capacity</dt><dd>{count(c?.response_cache.max_entries)} entries</dd>
                  {c?.response_cache.entries !== undefined && (
                    <>
                      <dt>Held now</dt><dd>{count(c.response_cache.entries)}</dd>
                    </>
                  )}
                  {c?.response_cache.shared !== undefined && (
                    <>
                      <dt>Shared across instances</dt><dd>{c.response_cache.shared ? 'yes' : 'no'}</dd>
                    </>
                  )}
                </dl>
                <p className="hint" style={{ marginTop: 14 }}>
                  Purging empties the cache for every caller. A cached response costs no upstream
                  call, charges no rate limit and records no spend, so purging makes the next request
                  of each kind real again.
                </p>
                <div className="button-row" style={{ marginTop: 12 }}>
                  {confirming ? (
                    <>
                      <button className="danger" onClick={purge}>Really purge</button>
                      <button onClick={() => setConfirming(false)}>Cancel</button>
                    </>
                  ) : (
                    <button onClick={() => setConfirming(true)}>Purge response cache</button>
                  )}
                </div>
              </>
            ) : (
              <p className="hint" style={{ margin: 0 }}>Response caching is disabled.</p>
            )}
          </div>
        </Panel>

        <Panel title="Shared state">
          <div className="panel-body">
            {data?.shared_state ? (
              <>
                <Pill tone={data.shared_state.reachable ? 'ok' : 'bad'} dot>
                  {data.shared_state.reachable ? 'Redis reachable' : 'Redis unreachable'}
                </Pill>
                <p className="hint" style={{ marginTop: 10 }}>
                  {count(data.shared_state.degradations)} degradations since start. While shared
                  state is down every limit is counted per instance: a key's 60 rpm becomes 60 per
                  gateway. The gateway keeps serving rather than refusing traffic, because enforcing
                  a limit per instance is a smaller failure than an outage.
                </p>
              </>
            ) : (
              <p className="hint" style={{ margin: 0 }}>
                Running single-instance. Every rate limit and budget is counted in this process,
                which is exact for one gateway and multiplied by the replica count for more than one.
              </p>
            )}
          </div>
        </Panel>
      </div>

      <div className="split mb">
        <Panel title="Persistence">
          <div className="panel-body">
            <dl className="kv" style={{ gridTemplateColumns: '1fr auto', marginTop: 0 }}>
              <dt>Key store</dt>
              <dd>{c?.keys.store_kind ?? '—'}{c?.keys.store_target ? ` · ${c.keys.store_target}` : ''}</dd>
              <dt>Spend ledger</dt><dd>{c?.spend.ledger ? 'on' : 'off'}</dd>
              <dt>Spend history</dt>
              <dd>{c?.spend.history ? (c.spend.history_target ?? 'on') : 'not kept'}</dd>
              <dt>Audit sink</dt>
              <dd>{c?.audit.sink ?? '—'}{c?.audit.path ? ` · ${c.audit.path}` : c?.audit.target ? ` · ${c.audit.target}` : ''}</dd>
              <dt>Master key set</dt><dd>{c?.keys.master_key_set ? 'yes' : 'no'}</dd>
              <dt>Key headers</dt><dd>{c?.keys.header_names?.join(', ') || '—'}</dd>
            </dl>
            {c && !c.spend.history && (
              <p className="hint" style={{ marginTop: 14 }}>
                Without a history the gateway still enforces every budget and simply cannot say what
                was spent last month — the ledger holds the current window and nothing before it.
              </p>
            )}
          </div>
        </Panel>

        <Panel title="Limits and buffers">
          <div className="panel-body">
            <dl className="kv" style={{ gridTemplateColumns: '1fr auto', marginTop: 0 }}>
              <dt>Max request body</dt><dd>{bytes(c?.limits.max_body_bytes)}</dd>
              <dt>Traffic buffer</dt>
              <dd>{count(data?.traffic.held)} / {count(data?.traffic.capacity)}</dd>
              <dt>Identity provider</dt><dd>{c?.sso.enabled ? (c.sso.issuer || 'configured') : 'off'}</dd>
              <dt>Hierarchy</dt>
              <dd>
                {c?.rbac.enabled
                  ? `${c.rbac.organizations ?? 0} org · ${c.rbac.teams ?? 0} team · ${c.rbac.projects ?? 0} project`
                  : 'flat'}
              </dd>
            </dl>
            <p className="hint" style={{ marginTop: 14 }}>
              {data?.traffic.note} Metadata only — no request or response bodies are retained, and
              nothing here survives a restart.
            </p>
          </div>
        </Panel>
      </div>

      {c?.note && <Notice>{c.note}</Notice>}
    </>
  )
}
