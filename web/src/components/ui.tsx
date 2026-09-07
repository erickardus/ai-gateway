import { createContext, useCallback, useContext, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { money } from '../format'
import { Icon } from './icons'

export function Panel({ title, actions, children, note, id }: {
  title?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  note?: ReactNode
  id?: string
}) {
  return (
    <section className="panel" id={id}>
      {(title || actions) && (
        <header className="panel-head">
          <span className="panel-title">{title}</span>
          {actions}
        </header>
      )}
      {note && <p className="panel-note">{note}</p>}
      {children}
    </section>
  )
}

export function Stat({ label, value, sub, chart, tone }: {
  label: ReactNode
  value: ReactNode
  sub?: ReactNode
  chart?: ReactNode
  tone?: 'accent'
}) {
  return (
    <div className={`stat${tone ? ' ' + tone : ''}`}>
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {sub && <div className="stat-sub">{sub}</div>}
      {chart && <div className="stat-spark">{chart}</div>}
    </div>
  )
}

type Tone = 'ok' | 'warn' | 'bad' | 'neutral' | 'accent' | 'info'

export function Pill({ tone = 'neutral', children, title, dot }: {
  tone?: Tone
  children: ReactNode
  title?: string
  dot?: boolean
}) {
  return (
    <span className={`pill pill-${tone}`} title={title}>
      {dot && <span className="dot" />}
      {children}
    </span>
  )
}

/**
 * Budget renders spend against a cap.
 *
 * A key or scope with no cap gets the figure and no bar: an empty bar would
 * read as "nothing used yet" when it means "nothing is being measured".
 */
export function Budget({ spent, cap }: { spent: number; cap?: number }) {
  if (!cap) {
    return <span className="mono" title="no budget configured">{money(spent)}</span>
  }
  const fraction = Math.min(spent / cap, 1)
  const tone = fraction >= 1 ? 'bad' : fraction >= 0.8 ? 'warn' : ''
  return (
    <div style={{ minWidth: 132 }}>
      <div className="bar">
        <div className={`bar-fill ${tone}`} style={{ width: `${Math.max(fraction * 100, 1.5)}%` }} />
      </div>
      <div className="bar-label">
        {money(spent)} <span style={{ color: 'var(--faint)' }}>/ {money(cap)}</span>
      </div>
    </div>
  )
}

export function Empty({ children, icon = true }: { children: ReactNode; icon?: boolean }) {
  return (
    <div className="empty">
      {icon && <Icon.inbox size={24} />}
      <div>{children}</div>
    </div>
  )
}

export function Notice({ tone, children }: { tone?: 'bad' | 'warn' | 'ok'; children: ReactNode }) {
  const Glyph = tone === 'ok' ? Icon.check : Icon.alert
  return (
    <div className={`notice${tone ? ' ' + tone : ''}`}>
      <Glyph size={15} />
      <div>{children}</div>
    </div>
  )
}

export function PageHead({ title, children, actions }: {
  title: string
  children?: ReactNode
  actions?: ReactNode
}) {
  return (
    <div className="page-head">
      <div className="row">
        <div>
          <h1>{title}</h1>
          {children && <p className="page-note">{children}</p>}
        </div>
        <div className="spacer" />
        {actions && <div className="button-row">{actions}</div>}
      </div>
    </div>
  )
}

export function Search({ value, onChange, placeholder = 'Search…' }: {
  value: string
  onChange: (next: string) => void
  placeholder?: string
}) {
  return (
    <span className="search">
      <Icon.search size={14} />
      <input
        type="search"
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
      />
    </span>
  )
}

export function Segmented<T extends string | number>({ options, value, onChange }: {
  options: readonly { value: T; label: string; title?: string }[]
  value: T
  onChange: (next: T) => void
}) {
  return (
    <div className="segmented">
      {options.map((o) => (
        <button
          key={String(o.value)}
          type="button"
          title={o.title}
          className={o.value === value ? 'active' : ''}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

/** Skeleton stands in for a table while the first response is in flight. */
export function Skeleton({ rows = 5, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <div style={{ padding: 14 }}>
      {Array.from({ length: rows }, (_, r) => (
        <div key={r} style={{ display: 'flex', gap: 14, padding: '7px 0' }}>
          {Array.from({ length: cols }, (_, c) => (
            <div
              key={c}
              className="skeleton"
              style={{ flex: c === 0 ? 2 : 1, opacity: 1 - r * 0.13 }}
            />
          ))}
        </div>
      ))}
    </div>
  )
}

export function Drawer({ title, subtitle, onClose, children, footer }: {
  title: ReactNode
  subtitle?: ReactNode
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
}) {
  return (
    <div className="drawer-backdrop" onClick={onClose}>
      <div className="drawer" onClick={(e) => e.stopPropagation()} role="dialog" aria-modal="true">
        <div className="drawer-head">
          <div>
            <h2>{title}</h2>
            {subtitle && <p className="hint" style={{ marginTop: 4 }}>{subtitle}</p>}
          </div>
          <button className="ghost icon drawer-close" onClick={onClose} aria-label="Close">
            <Icon.close size={16} />
          </button>
        </div>
        {children}
        {footer}
      </div>
    </div>
  )
}

/** CopyButton reports back in place, so a copy needs no toast to confirm it. */
export function CopyButton({ text, label = 'Copy', className }: {
  text: string
  label?: string
  className?: string
}) {
  const [done, setDone] = useState(false)
  return (
    <button
      className={className}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text)
          setDone(true)
          setTimeout(() => setDone(false), 1600)
        } catch {
          // Clipboard access is denied outside a secure context, which a
          // gateway reached over plain http is. The text is on screen either
          // way, so this fails quietly rather than raising an error about a
          // convenience.
        }
      }}
    >
      {done ? <Icon.check size={14} /> : <Icon.copy size={14} />}
      {done ? 'Copied' : label}
    </button>
  )
}

/* Toasts ------------------------------------------------------------------ */

type Toast = { id: number; tone: 'ok' | 'bad'; message: string }
type ToastApi = { ok: (message: string) => void; bad: (message: string) => void }

const ToastContext = createContext<ToastApi>({ ok: () => {}, bad: () => {} })

export const useToast = () => useContext(ToastContext)

/**
 * ToastHost holds transient confirmations.
 *
 * They float rather than being inserted into the page because the old console
 * pushed a table down by one row's height every time a key was blocked, moving
 * the next row out from under the cursor that was about to click it.
 */
export function ToastHost({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])

  const push = useCallback((tone: 'ok' | 'bad', message: string) => {
    const id = Date.now() + Math.random()
    setToasts((list) => [...list, { id, tone, message }])
    // Failures linger: a message saying what went wrong is worth reading twice,
    // and a confirmation is not.
    setTimeout(() => setToasts((list) => list.filter((t) => t.id !== id)), tone === 'bad' ? 9000 : 4000)
  }, [])

  const api = useMemo<ToastApi>(() => ({
    ok: (message) => push('ok', message),
    bad: (message) => push('bad', message),
  }), [push])

  return (
    <ToastContext.Provider value={api}>
      {children}
      {toasts.length > 0 && (
        <div className="toasts" role="status" aria-live="polite">
          {toasts.map((t) => (
            <div className={`toast ${t.tone}`} key={t.id}>
              {t.tone === 'ok' ? <Icon.check size={15} /> : <Icon.alert size={15} />}
              <span>{t.message}</span>
              <button
                className="ghost icon sm"
                onClick={() => setToasts((list) => list.filter((x) => x.id !== t.id))}
                aria-label="Dismiss"
              >
                <Icon.close size={13} />
              </button>
            </div>
          ))}
        </div>
      )}
    </ToastContext.Provider>
  )
}
