import { useCallback, useEffect, useMemo, useState } from 'react'
import { NavLink, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { ApiError, api, type Overview as OverviewData, type Session } from './api'
import { RefreshProvider, useApi, useRefresh } from './useApi'
import { useTheme } from './theme'
import { SignIn } from './components/SignIn'
import { Icon } from './components/icons'
import { ToastHost, useToast } from './components/ui'
import { useKey } from './components/charts'
import { ago } from './format'
import { Overview } from './pages/Overview'
import { Analytics } from './pages/Analytics'
import { Traffic } from './pages/Traffic'
import { Keys } from './pages/Keys'
import { Spend } from './pages/Spend'
import { Routing } from './pages/Routing'
import { Scopes } from './pages/Scopes'
import { Audit } from './pages/Audit'
import { Ops } from './pages/Ops'

type Auth = 'checking' | 'in' | 'out'

type NavItem = {
  to: string
  label: string
  icon: (p: { size?: number }) => React.ReactElement
  section: string
  hint: string
}

const NAV: NavItem[] = [
  { to: '/overview', label: 'Overview', icon: Icon.overview, section: 'Observe', hint: 'Health, spend and traffic at a glance' },
  { to: '/analytics', label: 'Analytics', icon: Icon.analytics, section: 'Observe', hint: 'Throughput, latency and errors over time' },
  { to: '/traffic', label: 'Traffic', icon: Icon.traffic, section: 'Observe', hint: 'The last requests this instance served' },
  { to: '/spend', label: 'Spend', icon: Icon.spend, section: 'Observe', hint: 'Cost by key, team and deployment' },
  { to: '/keys', label: 'Keys', icon: Icon.keys, section: 'Govern', hint: 'Issue, budget, block and revoke virtual keys' },
  { to: '/organisations', label: 'Organisations', icon: Icon.scopes, section: 'Govern', hint: 'Teams and the budgets they pool' },
  { to: '/audit', label: 'Audit', icon: Icon.audit, section: 'Govern', hint: 'The tamper-evident record of who administered what' },
  { to: '/routing', label: 'Routing', icon: Icon.routing, section: 'Operate', hint: 'Model groups, upstreams and fallback chains' },
  { to: '/ops', label: 'Ops', icon: Icon.ops, section: 'Operate', hint: 'What this instance has enabled, and the cache lever' },
]

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

  if (auth === 'checking') return <div className="signin" />
  if (auth === 'out') return <SignIn onSignedIn={() => setAuth('in')} />

  return (
    <RefreshProvider>
      <ToastHost>
        <Console onSignedOut={() => setAuth('out')} />
      </ToastHost>
    </RefreshProvider>
  )
}

function Console({ onSignedOut }: { onSignedOut: () => void }) {
  const [menuOpen, setMenuOpen] = useState(false)
  const [paletteOpen, setPaletteOpen] = useState(false)
  const location = useLocation()
  const toast = useToast()

  // Every page hands this down so that a session expiring mid-session — the
  // ordinary case, since the TTL is hours and the console is left open — drops
  // straight back to sign-in instead of filling the page with 401s.
  const signedOut = useCallback(() => onSignedOut(), [onSignedOut])

  // The overview is fetched by the shell as well as by its own page, because
  // the sidebar reports the one thing an operator wants without navigating:
  // whether anything is currently unhealthy.
  const { data: overview } = useApi<OverviewData>('/overview', 20_000, signedOut)

  useKey((e) => (e.metaKey || e.ctrlKey) && e.key === 'k', () => setPaletteOpen((o) => !o))
  useEffect(() => { setMenuOpen(false) }, [location.pathname])

  const current = NAV.find((n) => n.to === location.pathname)

  return (
    <div className="shell">
      {menuOpen && (
        <div
          className="drawer-backdrop"
          style={{ zIndex: 44 }}
          onClick={() => setMenuOpen(false)}
        />
      )}
      <nav className={`sidebar${menuOpen ? ' open' : ''}`}>
        <div className="brand">
          <span className="brand-mark"><Icon.bolt size={16} /></span>
          <div>
            <div className="brand-name">ai-gateway</div>
            <div className="brand-sub">operator console</div>
          </div>
        </div>

        <div className="nav">
          {NAV.map((item, i) => (
            <div key={item.to}>
              {NAV[i - 1]?.section !== item.section && (
                <div className="nav-section">{item.section}</div>
              )}
              <NavLink to={item.to} className="nav-link" title={item.hint}>
                <item.icon size={16} />
                {item.label}
                <NavCount item={item} overview={overview} />
              </NavLink>
            </div>
          ))}
        </div>

        <div className="sidebar-foot">
          <div className="who">
            {overview ? `${overview.healthy_deployments}/${overview.total_deployments} deployments ready` : '—'}
          </div>
          <button
            className="ghost"
            style={{ justifyContent: 'flex-start', padding: '5px 0' }}
            onClick={async () => {
              try {
                await api.post('/session/delete')
              } catch {
                // The cookie is cleared server-side on any outcome worth having;
                // a failure here still means the operator wanted out.
              }
              onSignedOut()
            }}
          >
            <Icon.signOut size={15} /> Sign out
          </button>
        </div>
      </nav>

      <div>
        <header className="topbar">
          <button
            className="ghost icon sidebar-toggle"
            onClick={() => setMenuOpen((o) => !o)}
            aria-label="Menu"
          >
            <Icon.menu size={17} />
          </button>
          <span className="crumb">
            <strong>{current?.label ?? 'Console'}</strong>
          </span>
          <span className="spacer" />
          <button className="ghost sm" onClick={() => setPaletteOpen(true)}>
            <Icon.search size={14} /> Search <kbd>⌘K</kbd>
          </button>
          <LiveControl />
          <ThemeToggle />
        </header>

        <main className="main">
          <Routes>
            <Route path="/" element={<Navigate to="/overview" replace />} />
            <Route path="/overview" element={<Overview onUnauthorized={signedOut} />} />
            <Route path="/analytics" element={<Analytics onUnauthorized={signedOut} />} />
            <Route path="/traffic" element={<Traffic onUnauthorized={signedOut} />} />
            <Route path="/keys" element={<Keys onUnauthorized={signedOut} />} />
            <Route path="/spend" element={<Spend onUnauthorized={signedOut} />} />
            <Route path="/routing" element={<Routing onUnauthorized={signedOut} />} />
            <Route path="/deployments" element={<Navigate to="/routing" replace />} />
            <Route path="/organisations" element={<Scopes onUnauthorized={signedOut} />} />
            <Route path="/audit" element={<Audit onUnauthorized={signedOut} />} />
            <Route path="/ops" element={<Ops onUnauthorized={signedOut} />} />
            <Route path="*" element={<Navigate to="/overview" replace />} />
          </Routes>
        </main>
      </div>

      {paletteOpen && (
        <CommandPalette
          onClose={() => setPaletteOpen(false)}
          onCopyCurl={() => toast.ok('Not implemented here — see the Keys page for a ready-made snippet.')}
        />
      )}
    </div>
  )
}

/**
 * NavCount puts the one number worth carrying in the nav beside its page.
 *
 * Only two qualify. A count of keys tells an operator whether the page is worth
 * opening; a cooling-down deployment is the thing they came to find. Everything
 * else would be decoration that has to be kept correct.
 */
function NavCount({ item, overview }: { item: NavItem; overview: OverviewData | undefined }) {
  if (!overview) return null
  if (item.to === '/keys' && overview.keys?.total) {
    return <span className="nav-badge">{overview.keys.total}</span>
  }
  if (item.to === '/routing') {
    const down = overview.total_deployments - overview.healthy_deployments
    if (down > 0) return <span className="nav-badge" style={{ color: 'var(--bad)' }}>{down} down</span>
    return <span className="nav-dot ok" title="every deployment is ready" />
  }
  return null
}

function LiveControl() {
  const { paused, setPaused, refreshAll, lastUpdated } = useRefresh()
  const [, force] = useState(0)

  // The "updated 12s ago" label is a clock, and a clock that only advances when
  // something else re-renders is a stopped clock most of the time.
  useEffect(() => {
    const timer = setInterval(() => force((n) => n + 1), 5_000)
    return () => clearInterval(timer)
  }, [])

  return (
    <>
      <button
        className="ghost sm"
        onClick={() => setPaused(!paused)}
        title={paused ? 'Resume automatic refresh' : 'Pause automatic refresh'}
      >
        <span className={`live-dot${paused ? ' paused' : ''}`} />
        {paused ? 'Paused' : 'Live'}
      </button>
      <button className="ghost icon" onClick={refreshAll} title={
        lastUpdated ? `Refresh — last updated ${ago(new Date(lastUpdated).toISOString())}` : 'Refresh'
      } aria-label="Refresh">
        <Icon.refresh size={15} />
      </button>
    </>
  )
}

function ThemeToggle() {
  const [theme, , cycle] = useTheme()
  const Glyph = theme === 'light' ? Icon.sun : theme === 'dark' ? Icon.moon : Icon.monitor
  return (
    <button
      className="ghost icon"
      onClick={cycle}
      title={`Theme: ${theme}. Click for ${theme === 'system' ? 'light' : theme === 'light' ? 'dark' : 'system'}.`}
      aria-label={`Theme: ${theme}`}
    >
      <Glyph size={15} />
    </button>
  )
}

/**
 * CommandPalette is navigation for someone who already knows where they are
 * going, which after a week is every operator who uses this console.
 */
function CommandPalette({ onClose }: { onClose: () => void; onCopyCurl: () => void }) {
  const navigate = useNavigate()
  const [query, setQuery] = useState('')
  const [cursor, setCursor] = useState(0)

  const matches = useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (!needle) return NAV
    return NAV.filter((n) =>
      n.label.toLowerCase().includes(needle) ||
      n.section.toLowerCase().includes(needle) ||
      n.hint.toLowerCase().includes(needle))
  }, [query])

  useEffect(() => { setCursor(0) }, [query])

  const go = (item: NavItem | undefined) => {
    if (!item) return
    navigate(item.to)
    onClose()
  }

  return (
    <div className="palette-backdrop" onClick={onClose}>
      <div className="palette" onClick={(e) => e.stopPropagation()} role="dialog" aria-modal="true">
        <input
          autoFocus
          type="text"
          placeholder="Go to…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') onClose()
            if (e.key === 'ArrowDown') { e.preventDefault(); setCursor((c) => Math.min(c + 1, matches.length - 1)) }
            if (e.key === 'ArrowUp') { e.preventDefault(); setCursor((c) => Math.max(c - 1, 0)) }
            if (e.key === 'Enter') { e.preventDefault(); go(matches[cursor]) }
          }}
        />
        <div className="palette-list">
          {matches.length === 0 && <div className="palette-item">No match.</div>}
          {matches.map((item, i) => (
            <div
              key={item.to}
              className={`palette-item${i === cursor ? ' active' : ''}`}
              onMouseEnter={() => setCursor(i)}
              onClick={() => go(item)}
            >
              <item.icon size={15} />
              {item.label}
              <span className="desc">{item.hint}</span>
            </div>
          ))}
        </div>
        <div className="palette-foot">
          <span><kbd>↑</kbd> <kbd>↓</kbd> move</span>
          <span><kbd>⏎</kbd> open</span>
          <span><kbd>esc</kbd> close</span>
        </div>
      </div>
    </div>
  )
}
