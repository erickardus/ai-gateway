import { useState } from 'react'
import { api } from '../api'

// SignIn exchanges the master key for a session.
//
// The key is held in component state for exactly as long as the request takes
// and is never written to localStorage or a query string. What the browser
// keeps afterwards is the httpOnly cookie the gateway sets, which JavaScript —
// including anything injected into this page — cannot read back.
export function SignIn({ onSignedIn }: { onSignedIn: () => void }) {
  const [key, setKey] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      await api.post('/session', { master_key: key })
      setKey('')
      onSignedIn()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="signin">
      <form className="signin-card" onSubmit={submit}>
        <h1>ai-gateway</h1>
        <p>Sign in with the gateway's master key to open the operator console.</p>
        {error && <div className="notice bad">{error}</div>}
        <div className="field">
          <label htmlFor="master-key">Master key</label>
          <input
            id="master-key"
            type="password"
            autoComplete="off"
            autoFocus
            placeholder="sk-master-…"
            value={key}
            onChange={(e) => setKey(e.target.value)}
          />
          <p className="hint">
            The session lasts as long as <code>ui.session_ttl</code> and is scoped to /ui — it cannot
            authenticate an inference request.
          </p>
        </div>
        <button className="primary" type="submit" disabled={busy || key.trim() === ''}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </div>
  )
}
