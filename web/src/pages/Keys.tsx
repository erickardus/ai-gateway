import { useMemo, useState } from 'react'
import { api, type Key, type KeysResponse } from '../api'
import { useApi } from '../useApi'
import { Budget, Empty, Notice, PageHead, Panel, Pill } from '../components/ui'
import { ago, dateTime, goDuration, goDurationField, shortHash } from '../format'
import type { PageProps } from './Overview'

export function Keys({ onUnauthorized }: PageProps) {
  const { data, error, reload } = useApi<KeysResponse>('/keys', 15_000, onUnauthorized)
  const [creating, setCreating] = useState(false)
  const [issued, setIssued] = useState<{ key: string; alias?: string }>()
  const [editing, setEditing] = useState<Key>()
  const [message, setMessage] = useState<string>()
  const [failure, setFailure] = useState<string>()

  const keys = data?.keys ?? []

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
    return [...bySubject.entries()].filter(([, list]) => list.length > 0)
  }, [keys])

  const act = async (fn: () => Promise<unknown>, ok: string) => {
    setMessage(undefined)
    setFailure(undefined)
    try {
      await fn()
      setMessage(ok)
      reload()
    } catch (err) {
      setFailure(err instanceof Error ? err.message : String(err))
    }
  }

  return (
    <>
      <PageHead title="Keys">
        Virtual keys authenticate callers to the gateway. A key travels in a custom header, which is
        what leaves a caller's own subscription credential untouched.
      </PageHead>

      {error && <Notice tone="bad">{error}</Notice>}
      {failure && <Notice tone="bad">{failure}</Notice>}
      {message && <Notice tone="ok">{message}</Notice>}

      <Panel
        title={`${keys.length} key${keys.length === 1 ? '' : 's'}`}
        actions={<button className="primary" onClick={() => setCreating(true)}>New key</button>}
      >
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
              {keys.map((key) => (
                <tr key={key.hash}>
                  <td>
                    {key.alias || <span className="hint">unnamed</span>}
                    {key.device && <div className="hint">{key.device}</div>}
                  </td>
                  <td className="mono" title={key.hash}>{shortHash(key.hash)}</td>
                  <td>
                    {key.models?.length
                      ? key.models.join(', ')
                      : <span className="hint">every group</span>}
                  </td>
                  <td className="mono">{key.scope || <span className="hint">—</span>}</td>
                  <td className="hint">
                    {[
                      key.rpm_limit ? `${key.rpm_limit} rpm` : null,
                      key.tpm_limit ? `${key.tpm_limit} tpm` : null,
                    ].filter(Boolean).join(' · ') || 'unlimited'}
                  </td>
                  <td>
                    <Budget spent={key.spend} cap={key.max_budget} />
                    {key.max_budget ? (
                      <div className="hint">per {goDuration(key.budget_duration)}</div>
                    ) : null}
                  </td>
                  <td className="hint" title={dateTime(key.expires_at)}>
                    {key.expires_at ? ago(key.expires_at) : 'never'}
                  </td>
                  <td>
                    <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                      {key.blocked && <Pill tone="bad">blocked</Pill>}
                      {!key.blocked && <Pill tone="ok">active</Pill>}
                      {key.allow_passthrough && (
                        <Pill tone="accent" title="may reach deployments that relay the caller's own credential upstream">
                          passthrough
                        </Pill>
                      )}
                      {key.subject && <Pill tone="neutral" title={key.subject}>sso</Pill>}
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
        {keys.length === 0 && <Empty>No keys yet. Mint one, or let a developer sign in through SSO.</Empty>}
      </Panel>

      {people.length > 0 && (
        <Panel
          title="Signed in through SSO"
          note="One person can hold several keys — one per device — and all of them share a spend subject, so a budget cannot be reset by signing in again. Revoking a person means revoking every key here."
        >
          <div className="table-scroll">
            <table>
              <thead>
                <tr><th>Identity</th><th>Devices</th><th>Keys</th><th /></tr>
              </thead>
              <tbody>
                {people.map(([subject, list]) => (
                  <tr key={subject}>
                    <td className="mono wrap-anywhere">{subject}</td>
                    <td>{list.map((k) => k.device || 'unnamed').join(', ')}</td>
                    <td className="num">{list.length}</td>
                    <td>
                      <button
                        className="link"
                        onClick={() => act(async () => {
                          for (const key of list) {
                            await api.post('/keys/update', { hash: key.hash, blocked: true })
                          }
                        }, `Blocked every key held by ${subject}.`)}
                      >
                        Block all
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
            setMessage(note)
            reload()
          }}
          onDeleted={() => {
            setEditing(undefined)
            setMessage('Key deleted. Its spend history went with it.')
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
  return (
    <>
      <div className="field">
        <label htmlFor="alias">Alias</label>
        <input id="alias" type="text" value={form.alias} onChange={(e) => set({ alias: e.target.value })} />
        <p className="hint">Shown in access logs and spend reports. This is what an operator reads instead of a hash.</p>
      </div>

      <div className="field">
        <label htmlFor="models">Model groups</label>
        <select
          id="models"
          multiple
          size={Math.min(Math.max(groups.length, 2), 5)}
          value={form.models}
          onChange={(e) => set({ models: [...e.target.selectedOptions].map((o) => o.value) })}
        >
          {groups.map((g) => <option key={g} value={g}>{g}</option>)}
        </select>
        <p className="hint">Select none to allow every group.</p>
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
          <input id="rpm" type="number" min={0} value={form.rpm_limit} onChange={(e) => set({ rpm_limit: e.target.value })} />
        </div>
        <div className="field">
          <label htmlFor="tpm">Tokens / minute</label>
          <input id="tpm" type="number" min={0} value={form.tpm_limit} onChange={(e) => set({ tpm_limit: e.target.value })} />
        </div>
      </div>

      <div className="field-row">
        <div className="field">
          <label htmlFor="budget">Max budget ($)</label>
          <input id="budget" type="number" min={0} step="0.01" value={form.max_budget} onChange={(e) => set({ max_budget: e.target.value })} />
        </div>
        <div className="field">
          <label htmlFor="window">Budget window</label>
          <input id="window" type="text" placeholder="720h" value={form.budget_duration} onChange={(e) => set({ budget_duration: e.target.value })} />
        </div>
      </div>
      <p className="hint" style={{ marginTop: -6, marginBottom: 12 }}>
        A window with no cap is refused: it would limit nothing while looking like it did. Leave the
        window empty to cap the key's whole lifetime.
      </p>

      <div className="field">
        <label htmlFor="duration">Expires after</label>
        <input id="duration" type="text" placeholder="720h — empty for never" value={form.duration} onChange={(e) => set({ duration: e.target.value })} />
      </div>

      <label className="checkbox">
        <input type="checkbox" checked={form.allow_passthrough} onChange={(e) => set({ allow_passthrough: e.target.checked })} />
        Allow passthrough deployments
      </label>
      <p className="hint" style={{ marginTop: -4 }}>
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
    <div className="drawer-backdrop" onClick={onClose}>
      <div className="drawer" onClick={(e) => e.stopPropagation()}>
        <h2>New virtual key</h2>
        <p className="hint">The key is shown once, on the next screen, and never stored in a form the gateway can recover.</p>
        {error && <div className="notice bad" style={{ marginTop: 14 }}>{error}</div>}
        <div style={{ marginTop: 18 }}>
          <FormFields form={form} set={set} groups={groups} scopes={scopes} />
        </div>
        <div className="button-row" style={{ marginTop: 18 }}>
          <button className="primary" onClick={submit} disabled={busy}>{busy ? 'Creating…' : 'Create key'}</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  )
}

// IssuedDrawer shows the plaintext exactly once, alongside the environment a
// Claude Code user needs.
//
// The snippet matters as much as the key: the whole reason this gateway exists
// is that Claude Code's virtual key has to travel in ANTHROPIC_CUSTOM_HEADERS
// rather than Authorization, and handing over a bare key without saying so
// invites the one mistake — pasting it as ANTHROPIC_API_KEY — that quietly
// bills the developer per token instead of using their subscription.
function IssuedDrawer({ issued, onClose }: { issued: { key: string; alias?: string }; onClose: () => void }) {
  const [copied, setCopied] = useState<string>()
  const snippet = [
    'export ANTHROPIC_BASE_URL="' + window.location.origin + '"',
    'export ANTHROPIC_CUSTOM_HEADERS="x-gateway-key: ' + issued.key + '"',
  ].join('\n')

  const copy = async (text: string, what: string) => {
    await navigator.clipboard.writeText(text)
    setCopied(what)
    setTimeout(() => setCopied(undefined), 1800)
  }

  return (
    <div className="drawer-backdrop">
      <div className="drawer">
        <h2>{issued.alias ? `Key for ${issued.alias}` : 'New key'}</h2>
        <div className="notice warn" style={{ marginTop: 14 }}>
          This is the only time the key is shown. Only its hash is stored, so it cannot be recovered.
        </div>

        <p className="section-label">Key</p>
        <pre className="mono wrap-anywhere" style={{ whiteSpace: 'pre-wrap', margin: '8px 0 0' }}>{issued.key}</pre>
        <div className="button-row" style={{ marginTop: 8 }}>
          <button onClick={() => copy(issued.key, 'key')}>{copied === 'key' ? 'Copied' : 'Copy key'}</button>
        </div>

        <p className="section-label">Claude Code</p>
        <pre className="mono wrap-anywhere" style={{ whiteSpace: 'pre-wrap', margin: '8px 0 0' }}>{snippet}</pre>
        <p className="hint">
          The key goes in a custom header, never in ANTHROPIC_API_KEY — that header would displace
          the subscription token and bill per token instead.
        </p>
        <div className="button-row" style={{ marginTop: 8 }}>
          <button onClick={() => copy(snippet, 'snippet')}>{copied === 'snippet' ? 'Copied' : 'Copy snippet'}</button>
        </div>

        <div className="button-row" style={{ marginTop: 24 }}>
          <button className="primary" onClick={onClose}>I've stored it</button>
        </div>
      </div>
    </div>
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
    <div className="drawer-backdrop" onClick={onClose}>
      <div className="drawer" onClick={(e) => e.stopPropagation()}>
        <h2>{keyRecord.alias || 'Key'}</h2>
        <p className="hint mono">{keyRecord.hash}</p>
        <p className="hint">
          Created {dateTime(keyRecord.created_at)}
          {keyRecord.subject ? ` · issued by SSO to ${keyRecord.subject}` : ''}
        </p>
        {error && <div className="notice bad" style={{ marginTop: 14 }}>{error}</div>}

        <div style={{ marginTop: 18 }}>
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

        <div className="button-row" style={{ marginTop: 18 }}>
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
          <p className="hint" style={{ marginTop: 8 }}>
            Deleting drops the key's spend record with it. To suspend someone while keeping what they
            spent, block the key instead.
          </p>
        )}
      </div>
    </div>
  )
}
