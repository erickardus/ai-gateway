import { useState } from 'react'
import { api, type Overview as OverviewData } from '../api'
import { useApi } from '../useApi'
import { Notice, PageHead, Panel, Pill } from '../components/ui'
import { count } from '../format'
import type { PageProps } from './Overview'

export function Ops({ onUnauthorized }: PageProps) {
  const { data, error, reload } = useApi<OverviewData>('/overview', 0, onUnauthorized)
  const [message, setMessage] = useState<string>()
  const [failure, setFailure] = useState<string>()
  const [confirming, setConfirming] = useState(false)

  const purge = async () => {
    setMessage(undefined)
    setFailure(undefined)
    setConfirming(false)
    try {
      await api.post('/cache/purge')
      setMessage('Response cache emptied.')
      reload()
    } catch (err) {
      setFailure(err instanceof Error ? err.message : String(err))
    }
  }

  return (
    <>
      <PageHead title="Ops">
        What this instance has turned on, and the one lever the console offers. Everything else is
        configuration: the gateway reads gateway.yaml at start, and this page does not write it.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}
      {failure && <Notice tone="bad">{failure}</Notice>}
      {message && <Notice tone="ok">{message}</Notice>}

      {data && (
        <>
          <Panel title="Features">
            <div className="panel-body">
              <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                {Object.entries(data.features).map(([name, on]) => (
                  <Pill key={name} tone={on ? 'ok' : 'neutral'}>{name.replace(/_/g, ' ')}: {on ? 'on' : 'off'}</Pill>
                ))}
              </div>
            </div>
          </Panel>

          <Panel title="Shared state">
            <div className="panel-body">
              {data.shared_state ? (
                <>
                  <Pill tone={data.shared_state.reachable ? 'ok' : 'bad'}>
                    {data.shared_state.reachable ? 'Redis reachable' : 'Redis unreachable'}
                  </Pill>{' '}
                  <span className="hint">{count(data.shared_state.degradations)} degradations since start</span>
                  <p className="hint">
                    While shared state is down every limit is counted per instance: a key's 60 rpm
                    becomes 60 per gateway. The gateway keeps serving rather than refusing traffic,
                    because enforcing a limit per instance is a smaller failure than an outage.
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

          <Panel title="Response cache">
            <div className="panel-body">
              {data.features.response_cache ? (
                <>
                  <p className="hint" style={{ marginTop: 0 }}>
                    Purging empties the cache for every caller. A cached response costs no upstream
                    call, charges no rate limit and records no spend, so purging makes the next
                    request of each kind real again.
                  </p>
                  <div className="button-row">
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

          <Panel title="Traffic buffer">
            <div className="panel-body">
              <p className="hint" style={{ margin: 0 }}>
                Holding {count(data.traffic.held)} of {count(data.traffic.capacity)} records.{' '}
                {data.traffic.note} Metadata only — no request or response bodies are retained, and
                nothing here survives a restart.
              </p>
            </div>
          </Panel>
        </>
      )}
    </>
  )
}
