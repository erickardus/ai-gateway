import { useCallback, useEffect, useState } from 'react'
import { NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { ApiError, api, type Session } from './api'
import { SignIn } from './components/SignIn'
import { Overview } from './pages/Overview'
import { Traffic } from './pages/Traffic'
import { Keys } from './pages/Keys'
import { Spend } from './pages/Spend'
import { Deployments } from './pages/Deployments'
import { Scopes } from './pages/Scopes'
import { Ops } from './pages/Ops'

type Auth = 'checking' | 'in' | 'out'

export function App() {
  const [auth, setAuth] = useState<Auth>('checking')

  const check = useCallback(async () => {
    try {
      await api.get<Session>('/session')
      setAuth('in')
    } catch (err) {
      // Anything other than a 401 is still a signed-out state as far as this
      // component is concerned: the console cannot render without the API, and
      // the sign-in page carries the error the next attempt will surface.
      setAuth(err instanceof ApiError && err.status === 401 ? 'out' : 'out')
    }
  }, [])

  useEffect(() => { void check() }, [check])

  // Every page hands this down so that a session expiring mid-session — the
  // ordinary case, since the TTL is hours and the console is left open — drops
  // straight back to sign-in instead of filling the page with 401s.
  const signedOut = useCallback(() => setAuth('out'), [])

  if (auth === 'checking') return <div className="signin" />
  if (auth === 'out') return <SignIn onSignedIn={() => setAuth('in')} />

  return (
    <div className="shell">
      <nav className="sidebar">
        <div className="brand">
          <div className="brand-name">ai-gateway</div>
          <div className="brand-sub">operator console</div>
        </div>
        <NavLink to="/overview" className="nav-link">Overview</NavLink>
        <NavLink to="/traffic" className="nav-link">Traffic</NavLink>
        <NavLink to="/keys" className="nav-link">Keys</NavLink>
        <NavLink to="/spend" className="nav-link">Spend</NavLink>
        <NavLink to="/deployments" className="nav-link">Deployments</NavLink>
        <NavLink to="/organisations" className="nav-link">Organisations</NavLink>
        <NavLink to="/ops" className="nav-link">Ops</NavLink>
        <div className="sidebar-foot">
          <button
            className="link"
            style={{ padding: 0 }}
            onClick={async () => {
              await api.post('/session/delete')
              setAuth('out')
            }}
          >
            Sign out
          </button>
        </div>
      </nav>
      <main className="main">
        <Routes>
          <Route path="/" element={<Navigate to="/overview" replace />} />
          <Route path="/overview" element={<Overview onUnauthorized={signedOut} />} />
          <Route path="/traffic" element={<Traffic onUnauthorized={signedOut} />} />
          <Route path="/keys" element={<Keys onUnauthorized={signedOut} />} />
          <Route path="/spend" element={<Spend onUnauthorized={signedOut} />} />
          <Route path="/deployments" element={<Deployments onUnauthorized={signedOut} />} />
          <Route path="/organisations" element={<Scopes onUnauthorized={signedOut} />} />
          <Route path="/ops" element={<Ops onUnauthorized={signedOut} />} />
          <Route path="*" element={<Navigate to="/overview" replace />} />
        </Routes>
      </main>
    </div>
  )
}
