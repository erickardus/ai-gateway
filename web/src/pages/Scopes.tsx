import type { ScopesResponse } from '../api'
import { useApi } from '../useApi'
import { Budget, Empty, Notice, PageHead, Panel, Pill } from '../components/ui'
import { count, goDuration } from '../format'
import type { PageProps } from './Overview'

const DEPTH: Record<string, number> = { organization: 0, team: 1, project: 2 }

export function Scopes({ onUnauthorized }: PageProps) {
  const { data, error } = useApi<ScopesResponse>('/scopes', 15_000, onUnauthorized)
  const rows = data?.scopes ?? []

  return (
    <>
      <PageHead title="Organisations">
        Organisations, teams and projects, with the budget each shares. A scope's budget is one pool
        every key beneath it draws from — not a copy handed to each of them.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      {data && !data.enabled && (
        <Notice>
          No hierarchy is declared. Keys are flat and every scope check is skipped. Add an{' '}
          <code>rbac.organizations</code> block to gateway.yaml to pool budgets across a team.
        </Notice>
      )}

      <Panel title={`${rows.length} scope${rows.length === 1 ? '' : 's'}`} note={data?.note}>
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Scope</th>
                <th>Kind</th>
                <th className="num">Keys</th>
                <th>Models</th>
                <th className="num">Shared limits</th>
                <th>Pooled budget</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((scope) => (
                <tr key={scope.id}>
                  <td className={`tree-indent-${DEPTH[scope.kind] ?? 0}`}>
                    <div>{scope.alias || scope.id.split('/').pop()}</div>
                    <div className="hint mono wrap-anywhere">{scope.id}</div>
                  </td>
                  <td><Pill tone="neutral">{scope.kind}</Pill></td>
                  <td className="num">{count(scope.keys)}</td>
                  <td>
                    {scope.models?.length
                      ? scope.models.join(', ')
                      : <span className="hint" title="empty abstains rather than granting everything: a parent's list still binds">inherits</span>}
                  </td>
                  <td className="num hint">
                    {[scope.rpm_limit ? `${scope.rpm_limit} rpm` : null, scope.tpm_limit ? `${scope.tpm_limit} tpm` : null]
                      .filter(Boolean).join(' · ') || '—'}
                  </td>
                  <td>
                    <Budget spent={scope.spend} cap={scope.max_budget} />
                    {scope.max_budget ? <div className="hint">per {goDuration(scope.budget_duration)}</div> : null}
                  </td>
                  <td>{scope.blocked ? <Pill tone="bad">blocked</Pill> : <Pill tone="ok">active</Pill>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {rows.length === 0 && <Empty>No organisations declared in gateway.yaml.</Empty>}
      </Panel>
    </>
  )
}
