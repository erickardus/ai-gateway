import { useMemo, useState } from 'react'
import { api, type Key, type KeysResponse } from '../api'
import { useApi } from '../useApi'
import {
  Budget, CopyButton, Drawer, Empty, Notice, PageHead, Panel, Pill, Search, Segmented, Skeleton, useToast,
} from '../components/ui'
import { Icon } from '../components/icons'
import { ago, dateTime, goDuration, goDurationField, money, shortHash } from '../format'
import type { PageProps } from './Overview'

const FILTERS = [
  { value: 'all', label: 'All' },
  { value: 'active', label: 'Active' },
  { value: 'blocked', label: 'Blocked' },
  { value: 'sso', label: 'SSO' },
] as const

type Filter = (typeof FILTERS)[number]['value']

export function Keys({ onUnauthorized }: PageProps) {
  const { data, error, loading, reload } = useApi<KeysResponse>('/keys', 15_000, onUnauthorized)
  const [creating, setCreating] = useState(false)
  const [issued, setIssued] = useState<{ key: string; alias?: string }>()
  const [editing, setEditing] = useState<Key>()
  const [needle, setNeedle] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const toast = useToast()

  const keys = useMemo(() => data?.keys ?? [], [data])

  const expired = (k: Key) => !!k.expires_at && new Date(k.expires_at).getTime() < Date.now()

  const rows = useMemo(() => {
    const term = needle.trim().toLowerCase()
    return keys.filter((k) => {
      if (filter === 'active' && (k.blocked || expired(k))) return false
      if (filter === 'blocked' && !k.blocked) return false
      if (filter === 'sso' && !k.subject) return false
      if (!term) return true
      return [k.alias, k.hash, k.subject, k.device, k.scope, ...(k.models ?? [])]
        .some((field) => field?.toLowerCase().includes(term))
    })
  }, [keys, needle, filter])

  // SSO-issued keys are grouped by the person rather than listed flat, because
  // one developer signing in from a laptop and a desktop holds two keys and
  // offboarding them means revoking both. A flat list makes that a search
  // problem; grouping makes it visible.
  const people = useMemo(() => {
    const bySubject = new Map<string, Key[]>()
    for (const key of keys) {
      if (!key.subject) continue
      const list = bySubject.get(key.subject) ?? []
      list.push(key)
      bySubject.set(key.subject, list)
    }
    return [...bySubject.entries()]
  }, [keys])

  const act = async (fn: () => Promise<unknown>, ok: string) => {
    try {
      await fn()
      toast.ok(ok)
      reload()
    } catch (err) {
      toast.bad(err instanceof Error ? err.message : String(err))
    }
  }

  const spending = keys.filter((k) => k.spend > 0).length
  const overBudget = keys.filter((k) => k.max_budget && k.spend >= k.max_budget).length

  return (
    <>
      <PageHead
        title="Keys"
        actions={
          <button className="primary" onClick={() => setCreating(true)}>
            <Icon.plus size={15} /> New key
          </button>
        }
      >
        Virtual keys authenticate callers to the gateway. A key travels in a custom header, which is
        what leaves a caller's own subscription credential untouched.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}
      {overBudget > 0 && (
        <Notice tone="warn">
          <strong>{overBudget} key{overBudget === 1 ? ' has' : 's have'} reached the budget cap.</strong>{' '}
          Requests from {overBudget === 1 ? 'it' : 'them'} are being refused until the window rolls
          over or the cap is raised.
        </Notice>
      )}

      <Panel
        title={
          <>
            {rows.length} key{rows.length === 1 ? '' : 's'}
            {rows.length !== keys.length && (
              <span className="hint" style={{ marginTop: 0 }}>of {keys.length}</span>
            )}
          </>
        }
        actions={
          <div className="toolbar">
            <Search value={needle} onChange={setNeedle} placeholder="Alias, hash, person, scope…" />
            <Segmented options={FILTERS} value={filter} onChange={setFilter} />
          </div>
        }
      >
        {loading && keys.length === 0 ? <Skeleton rows={5} cols={6} /> : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Alias</th>
                  <th>Hash</th>
                  <th>Models</th>
                  <th>Scope</th>
                  <th>Limits</th>
                  <th>Budget</th>
                  <th>Expires</th>
                  <th>State</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((key) => (
                  <tr key={key.hash} className={key.blocked ? 'dim' : undefined}>
                    <td>
                      {key.alias || <span className="hint">unnamed</span>}
                      {key.device && <div className="cell-sub">{key.device}</div>}
                    </td>
                    <td className="mono" title={key.hash}>{shortHash(key.hash)}</td>
                    <td>
                      {key.models?.length
                        ? <div className="pill-row">{key.models.map((m) => <Pill key={m}>{m}</Pill>)}</div>
                        : <span className="hint">every group</span>}
                    </td>
                    <td className="mono">{key.scope || <span className="hint">—</span>}</td>
                    <td className="hint" style={{ marginTop: 0 }}>
                      {[
                        key.rpm_limit ? `${key.rpm_limit} rpm` : null,
                        key.tpm_limit ? `${key.tpm_limit} tpm` : null,
                      ].filter(Boolean).join(' · ') || 'unlimited'}
                    </td>
                    <td>
                      <Budget spent={key.spend} cap={key.max_budget} />
                      {key.max_budget ? (
                        <div className="cell-sub">per {goDuration(key.budget_duration)}</div>
                      ) : null}
                    </td>
                    <td className="hint nowrap" style={{ marginTop: 0 }} title={dateTime(key.expires_at)}>
                      {key.expires_at ? ago(key.expires_at) : 'never'}
                    </td>
                    <td>
                      <div className="pill-row">
                        {key.blocked
                          ? <Pill tone="bad" dot>blocked</Pill>
                          : expired(key)
                            ? <Pill tone="warn" dot>expired</Pill>
                            : <Pill tone="ok" dot>active</Pill>}
                        {key.allow_passthrough && (
                          <Pill tone="accent" title="may reach deployments that relay the caller's own credential upstream">
                            passthrough
                          </Pill>
                        )}
                        {key.subject && <Pill tone="info" title={key.subject}>sso</Pill>}
                      </div>
                    </td>
                    <td>
                      <div className="button-row">
                        <button className="link" onClick={() => setEditing(key)}>Edit</button>
                        <button
                          className="link"
                          onClick={() => act(
                            () => api.post('/keys/update', { hash: key.hash, blocked: !key.blocked }),
                            key.blocked ? 'Key unblocked.' : 'Key blocked. Its spend history is kept.',
                          )}
                        >
                          {key.blocked ? 'Unblock' : 'Block'}
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {!loading && rows.length === 0 && (
          <Empty>
            <strong>{keys.length === 0 ? 'No keys yet.' : 'Nothing matches that filter.'}</strong>
            {keys.length === 0
              ? 'Mint one, or let a developer sign in through SSO.'
              : 'Clear the search to see every key.'}
          </Empty>
        )}
      </Panel>

      {keys.length > 0 && (
        <p className="hint" style={{ marginTop: -6, marginBottom: 18 }}>
          {spending} of {keys.length} keys have spent anything in their current window; the ledger
          totals {money(keys.reduce((sum, k) => sum + k.spend, 0))}.
        </p>
      )}

      {people.length > 0 && (
        <Panel
          title="Signed in through SSO"
          note="One person can hold several keys — one per device — and all of them share a spend subject, so a budget cannot be reset by signing in again. Revoking a person means revoking every key here."
        >
          <div className="table-scroll">
            <table>
              <thead>
                <tr><th>Identity</th><th>Devices</th><th className="num">Keys</th><th className="num">Spent</th><th /></tr>
              </thead>
              <tbody>
                {people.map(([subject, list]) => (
                  <tr key={subject}>
                    <td className="mono wrap-anywhere">{subject}</td>
                    <td>
                      <div className="pill-row">
                        {list.map((k) => <Pill key={k.hash}>{k.device || 'unnamed'}</Pill>)}
                      </div>
                    </td>
                    <td className="num">{list.length}</td>
                    <td className="num">{money(list[0]?.spend ?? 0)}</td>
                    <td>
                      <button
                        className="link"
                        disabled={list.every((k) => k.blocked)}
                        onClick={() => act(async () => {
                          for (const key of list) {
                            await api.post('/keys/update', { hash: key.hash, blocked: true })
                          }
                        }, `Blocked every key held by ${subject}.`)}
                      >
                        {list.every((k) => k.blocked) ? 'All blocked' : 'Block all'}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Panel>
      )}

      {creating && (
        <CreateDrawer
          groups={data?.groups ?? []}
          scopes={data?.scopes ?? []}
          onClose={() => setCreating(false)}
          onCreated={(plaintext, alias) => {
            setCreating(false)
            setIssued({ key: plaintext, alias })
            reload()
          }}
        />
      )}

      {issued && <IssuedDrawer issued={issued} onClose={() => setIssued(undefined)} />}

      {editing && (
        <EditDrawer
          keyRecord={editing}
          scopes={data?.scopes ?? []}
          groups={data?.groups ?? []}
          onClose={() => setEditing(undefined)}
          onSaved={(note) => {
            setEditing(undefined)
            toast.ok(note)
            reload()
          }}
          onDeleted={() => {
            setEditing(undefined)
            toast.ok('Key deleted. Its spend history went with it.')
            reload()
          }}
        />
      )}
    </>
  )
}

type Form = {
  alias: string
  models: string[]
  scope: string
  rpm_limit: string
  tpm_limit: string
  max_budget: string
  budget_duration: string
  duration: string
  allow_passthrough: boolean
}

const emptyForm: Form = {
  alias: '', models: [], scope: '', rpm_limit: '', tpm_limit: '',
  max_budget: '', budget_duration: '', duration: '', allow_passthrough: false,
}

function FormFields({ form, set, groups, scopes }: {
  form: Form
  set: (patch: Partial<Form>) => void
  groups: string[]
  scopes: string[]
}) {
  const toggle = (group: string) => {
    set({
      models: form.models.includes(group)
        ? form.models.filter((m) => m !== group)
        : [...form.models, group],
    })
  }

  return (
    <>
      <div className="field">
        <label htmlFor="alias">Alias</label>
        <input id="alias" type="text" placeholder="dev-laptop" value={form.alias}
          onChange={(e) => set({ alias: e.target.value })} />
        <p className="hint">Shown in access logs and spend reports. This is what an operator reads instead of a hash.</p>
      </div>

      <div className="field">
        <label>Model groups</label>
        {/* A row of toggles rather than a multi-select. The native control needs
            a modifier key to pick two, which is the sort of thing that quietly
            issues a key with one group when the operator meant three. */}
        <div className="pill-row" style={{ gap: 6 }}>
          {groups.map((g) => (
            <button
              type="button"
              key={g}
              className={form.models.includes(g) ? 'primary sm' : 'sm'}
              onClick={() => toggle(g)}
            >
              {form.models.includes(g) && <Icon.check size={12} />}
              {g}
            </button>
          ))}
        </div>
        <p className="hint">
          {form.models.length === 0
            ? 'None selected — this key may reach every group.'
            : `Restricted to ${form.models.length} group${form.models.length === 1 ? '' : 's'}.`}
        </p>
      </div>

      {scopes.length > 0 && (
        <div className="field">
          <label htmlFor="scope">Scope</label>
          <select id="scope" value={form.scope} onChange={(e) => set({ scope: e.target.value })}>
            <option value="">No hierarchy</option>
            {scopes.map((s) => <option key={s} value={s}>{s}</option>)}
          </select>
          <p className="hint">The key's spend draws down this scope's shared pool, and the scope's limits bind on top of the key's own.</p>
        </div>
      )}

      <div className="field-row">
        <div className="field">
          <label htmlFor="rpm">Requests / minute</label>
          <input id="rpm" type="number" min={0} placeholder="unlimited" value={form.rpm_limit}
            onChange={(e) => set({ rpm_limit: e.target.value })} />
        </div>
        <div className="field">
          <label htmlFor="tpm">Tokens / minute</label>
          <input id="tpm" type="number" min={0} placeholder="unlimited" value={form.tpm_limit}
            onChange={(e) => set({ tpm_limit: e.target.value })} />
        </div>
      </div>

      <div className="field-row">
        <div className="field">
          <label htmlFor="budget">Max budget ($)</label>
          <input id="budget" type="number" min={0} step="0.01" placeholder="none" value={form.max_budget}
            onChange={(e) => set({ max_budget: e.target.value })} />
        </div>
        <div className="field">
          <label htmlFor="window">Budget window</label>
          <input id="window" type="text" placeholder="720h" value={form.budget_duration}
            onChange={(e) => set({ budget_duration: e.target.value })} />
        </div>
      </div>
      <p className="hint" style={{ marginTop: -8, marginBottom: 14 }}>
        A window with no cap is refused: it would limit nothing while looking like it did. Leave the
        window empty to cap the key's whole lifetime.
      </p>

      <div className="field">
        <label htmlFor="duration">Expires after</label>
        <input id="duration" type="text" placeholder="720h — empty for never" value={form.duration}
          onChange={(e) => set({ duration: e.target.value })} />
      </div>

      <label className="checkbox">
        <input type="checkbox" checked={form.allow_passthrough}
          onChange={(e) => set({ allow_passthrough: e.target.checked })} />
        Allow passthrough deployments
      </label>
      <p className="hint">
        Lets this key reach deployments that relay the caller's own credential upstream — which is
        how a claude.ai subscription keeps working through the gateway. Those requests carry usage
        but no cost, because the caller's account is billed rather than yours.
      </p>
    </>
  )
}

// numeric turns a form field into the JSON value the API expects: an empty
// field means "no limit", which is zero, not undefined.
const numeric = (raw: string) => (raw.trim() === '' ? 0 : Number(raw))

function CreateDrawer({ groups, scopes, onClose, onCreated }: {
  groups: string[]
  scopes: string[]
  onClose: () => void
  onCreated: (plaintext: string, alias?: string) => void
}) {
  const [form, setForm] = useState<Form>(emptyForm)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const set = (patch: Partial<Form>) => setForm((f) => ({ ...f, ...patch }))

  const submit = async () => {
    setBusy(true)
    setError(undefined)
    try {
      const response = await api.post<{ key: string }>('/keys/generate', {
        alias: form.alias,
        models: form.models,
        scope: form.scope,
        rpm_limit: numeric(form.rpm_limit),
        tpm_limit: numeric(form.tpm_limit),
        max_budget: numeric(form.max_budget),
        budget_duration: form.budget_duration.trim(),
        duration: form.duration.trim(),
        allow_passthrough: form.allow_passthrough,
      })
      onCreated(response.key, form.alias || undefined)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Drawer
      title="New virtual key"
      subtitle="The key is shown once, on the next screen, and never stored in a form the gateway can recover."
      onClose={onClose}
      footer={
        <div className="button-row" style={{ marginTop: 20 }}>
          <button className="primary" onClick={submit} disabled={busy}>{busy ? 'Creating…' : 'Create key'}</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      }
    >
      {error && <Notice tone="bad">{error}</Notice>}
      <div style={{ marginTop: 18 }}>
        <FormFields form={form} set={set} groups={groups} scopes={scopes} />
      </div>
    </Drawer>
  )
}

/**
 * IssuedDrawer shows the plaintext exactly once, alongside the environment a
 * Claude Code user needs.
 *
 * The snippet matters as much as the key: the whole reason this gateway exists
 * is that Claude Code's virtual key has to travel in ANTHROPIC_CUSTOM_HEADERS
 * rather than Authorization, and handing over a bare key without saying so
 * invites the one mistake — pasting it as ANTHROPIC_API_KEY — that quietly
 * bills the developer per token instead of using their subscription.
 */
function IssuedDrawer({ issued, onClose }: { issued: { key: string; alias?: string }; onClose: () => void }) {
  const snippet = [
    'export ANTHROPIC_BASE_URL="' + window.location.origin + '"',
    'export ANTHROPIC_CUSTOM_HEADERS="x-gateway-key: ' + issued.key + '"',
  ].join('\n')

  return (
    <Drawer
      title={issued.alias ? `Key for ${issued.alias}` : 'New key'}
      onClose={onClose}
      footer={
        <div className="button-row" style={{ marginTop: 26 }}>
          <button className="primary" onClick={onClose}>I've stored it</button>
        </div>
      }
    >
      <Notice tone="warn">
        This is the only time the key is shown. Only its hash is stored, so it cannot be recovered.
      </Notice>

      <p className="section-label">Key</p>
      <pre className="code">{issued.key}</pre>
      <div className="button-row" style={{ marginTop: 8 }}>
        <CopyButton text={issued.key} label="Copy key" />
      </div>

      <p className="section-label">Claude Code</p>
      <pre className="code">{snippet}</pre>
      <p className="hint">
        The key goes in a custom header, never in <code>ANTHROPIC_API_KEY</code> — that header would
        displace the subscription token and bill per token instead.
      </p>
      <div className="button-row" style={{ marginTop: 8 }}>
        <CopyButton text={snippet} label="Copy snippet" />
      </div>
    </Drawer>
  )
}

function EditDrawer({ keyRecord, groups, scopes, onClose, onSaved, onDeleted }: {
  keyRecord: Key
  groups: string[]
  scopes: string[]
  onClose: () => void
  onSaved: (note: string) => void
  onDeleted: () => void
}) {
  const [form, setForm] = useState<Form>({
    alias: keyRecord.alias ?? '',
    models: keyRecord.models ?? [],
    scope: keyRecord.scope ?? '',
    rpm_limit: keyRecord.rpm_limit ? String(keyRecord.rpm_limit) : '',
    tpm_limit: keyRecord.tpm_limit ? String(keyRecord.tpm_limit) : '',
    max_budget: keyRecord.max_budget ? String(keyRecord.max_budget) : '',
    budget_duration: goDurationField(keyRecord.budget_duration),
    duration: '',
    allow_passthrough: keyRecord.allow_passthrough ?? false,
  })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const [confirmDelete, setConfirmDelete] = useState(false)
  const set = (patch: Partial<Form>) => setForm((f) => ({ ...f, ...patch }))

  const save = async () => {
    setBusy(true)
    setError(undefined)
    try {
      // duration is sent only when the operator typed one, because an empty
      // string here means "clear the expiry" and sending it on every save would
      // silently make every edited key permanent.
      const body: Record<string, unknown> = {
        hash: keyRecord.hash,
        alias: form.alias,
        models: form.models,
        scope: form.scope,
        rpm_limit: numeric(form.rpm_limit),
        tpm_limit: numeric(form.tpm_limit),
        max_budget: numeric(form.max_budget),
        budget_duration: form.budget_duration.trim(),
        allow_passthrough: form.allow_passthrough,
      }
      if (form.duration.trim() !== '') body.duration = form.duration.trim()
      await api.post('/keys/update', body)
      onSaved('Key updated. Its hash, spend history and budget window are unchanged.')
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setBusy(false)
    }
  }

  return (
    <Drawer
      title={keyRecord.alias || 'Key'}
      subtitle={
        <>
          <span className="mono">{keyRecord.hash}</span>
          <br />
          Created {dateTime(keyRecord.created_at)}
          {keyRecord.subject ? ` · issued by SSO to ${keyRecord.subject}` : ''}
        </>
      }
      onClose={onClose}
      footer={
        <>
          <div className="button-row" style={{ marginTop: 20 }}>
            <button className="primary" onClick={save} disabled={busy}>{busy ? 'Saving…' : 'Save'}</button>
            <button onClick={onClose}>Cancel</button>
            <span className="spacer" />
            {confirmDelete ? (
              <button
                className="danger"
                onClick={async () => {
                  try {
                    await api.post('/keys/delete', { hash: keyRecord.hash })
                    onDeleted()
                  } catch (err) {
                    setError(err instanceof Error ? err.message : String(err))
                  }
                }}
              >
                Really delete
              </button>
            ) : (
              <button className="danger" onClick={() => setConfirmDelete(true)}>Delete</button>
            )}
          </div>
          {confirmDelete && (
            <p className="hint">
              Deleting drops the key's spend record with it. To suspend someone while keeping what
              they spent, block the key instead.
            </p>
          )}
        </>
      }
    >
      {error && <Notice tone="bad">{error}</Notice>}

      <div style={{ marginTop: 18 }}>
        <div className="field">
          <label>Spent this window</label>
          <Budget spent={keyRecord.spend} cap={keyRecord.max_budget} />
        </div>
        <FormFields form={form} set={set} groups={groups} scopes={scopes} />
      </div>

      <div className="field">
        <label htmlFor="renew">Extend expiry by</label>
        <input
          id="renew"
          type="text"
          placeholder={keyRecord.expires_at ? `currently ${dateTime(keyRecord.expires_at)}` : 'never expires'}
          value={form.duration}
          onChange={(e) => set({ duration: e.target.value })}
        />
        <p className="hint">Measured from now, not from the key's creation. Leave blank to keep the current expiry.</p>
      </div>
    </Drawer>
  )
}
