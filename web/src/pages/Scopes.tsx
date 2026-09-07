import type { ScopesResponse } from '../api'
import { useApi } from '../useApi'
import { Budget, Empty, Notice, PageHead, Panel, Pill, Stat } from '../components/ui'
import { BarList } from '../components/charts'
import { count, goDuration, money } from '../format'
import type { PageProps } from './Overview'

const DEPTH: Record<string, number> = { organization: 0, team: 1, project: 2 }

export function Scopes({ onUnauthorized }: PageProps) {
  const { data, error } = useApi<ScopesResponse>('/scopes', 15_000, onUnauthorized)
  const rows = data?.scopes ?? []

  const pooled = rows.reduce((sum, s) => sum + s.spend, 0)
  const capped = rows.filter((s) => s.max_budget)
  const nearCap = capped.filter((s) => s.max_budget && s.spend / s.max_budget >= 0.8)

  return (
    <>
      <PageHead title="Organisations">
        Organisations, teams and projects, with the budget each shares. A scope's budget is one pool
        every key beneath it draws from — not a copy handed to each of them.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}

      {data && !data.enabled && (
        <Notice>
          <span>
            <strong>No hierarchy is declared.</strong> Keys are flat and every scope check is
            skipped. Add an <code>rbac.organizations</code> block to gateway.yaml to pool budgets
            across a team.
          </span>
        </Notice>
      )}

      {nearCap.length > 0 && (
        <Notice tone="warn">
          <strong>{nearCap.length} scope{nearCap.length === 1 ? ' is' : 's are'} at 80% of the pool
          or beyond.</strong>{' '}
          Every key beneath {nearCap.length === 1 ? 'it' : 'them'} shares what is left:{' '}
          {nearCap.map((s) => s.id).join(', ')}.
        </Notice>
      )}

      {rows.length > 0 && (
        <div className="grid grid-4 mb tight">
          <Stat label="Scopes" value={String(rows.length)} sub={`${capped.length} with a budget`} />
          <Stat label="Pooled spend" value={money(pooled)} sub="summed across every level" />
          <Stat
            label="Keys assigned"
            value={count(rows.reduce((sum, s) => sum + s.keys, 0))}
            sub="keys naming a scope"
          />
          <Stat
            label="Blocked"
            value={String(rows.filter((s) => s.blocked).length)}
            sub="scopes refusing traffic"
          />
        </div>
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
                    <div className="cell-sub mono wrap-anywhere">{scope.id}</div>
                  </td>
                  <td><Pill tone={scope.kind === 'organization' ? 'accent' : 'neutral'}>{scope.kind}</Pill></td>
                  <td className="num">{count(scope.keys)}</td>
                  <td>
                    {scope.models?.length
                      ? <div className="pill-row">{scope.models.map((m) => <Pill key={m}>{m}</Pill>)}</div>
                      : <span className="hint" style={{ marginTop: 0 }}
                          title="empty abstains rather than granting everything: a parent's list still binds">
                          inherits
                        </span>}
                  </td>
                  <td className="num hint" style={{ marginTop: 0 }}>
                    {[scope.rpm_limit ? `${scope.rpm_limit} rpm` : null, scope.tpm_limit ? `${scope.tpm_limit} tpm` : null]
                      .filter(Boolean).join(' · ') || '—'}
                  </td>
                  <td>
                    <Budget spent={scope.spend} cap={scope.max_budget} />
                    {scope.max_budget ? <div className="cell-sub">per {goDuration(scope.budget_duration)}</div> : null}
                  </td>
                  <td>
                    {scope.blocked ? <Pill tone="bad" dot>blocked</Pill> : <Pill tone="ok" dot>active</Pill>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {rows.length === 0 && (
          <Empty>
            <strong>No organisations declared in gateway.yaml.</strong>
            Budgets are per key until one is.
          </Empty>
        )}
      </Panel>

      {rows.some((s) => s.spend > 0) && (
        <Panel title="Pooled spend by scope" note="Levels overlap on purpose: a request is charged to its project, its team and its organisation, so these bars do not sum to the gateway's total.">
          <div className="panel-body" style={{ padding: 8 }}>
            <BarList
              rows={[...rows]
                .filter((s) => s.spend > 0)
                .sort((a, b) => b.spend - a.spend)
                .map((s) => ({
                  name: s.alias || s.id,
                  value: s.spend,
                  sub: s.max_budget ? `of ${money(s.max_budget)}` : undefined,
                }))}
              format={money}
            />
          </div>
        </Panel>
      )}
    </>
  )
}
