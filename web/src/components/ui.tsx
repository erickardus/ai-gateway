import type { ReactNode } from 'react'
import { money } from '../format'

export function Panel({ title, actions, children, note }: {
  title?: ReactNode
  actions?: ReactNode
  children: ReactNode
  note?: ReactNode
}) {
  return (
    <section className="panel">
      {(title || actions) && (
        <header className="panel-head">
          <span className="panel-title">{title}</span>
          {actions}
        </header>
      )}
      {note && <div className="panel-body" style={{ paddingBottom: 0 }}><p className="hint" style={{ margin: 0 }}>{note}</p></div>}
      {children}
    </section>
  )
}

export function Stat({ label, value, sub }: { label: string; value: ReactNode; sub?: ReactNode }) {
  return (
    <div className="stat">
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  )
}

type Tone = 'ok' | 'warn' | 'bad' | 'neutral' | 'accent'

export function Pill({ tone = 'neutral', children, title }: { tone?: Tone; children: ReactNode; title?: string }) {
  return <span className={`pill pill-${tone}`} title={title}>{children}</span>
}

// Budget renders spend against a cap.
//
// A key or scope with no cap gets the figure and no bar: an empty bar would
// read as "nothing used yet" when it means "nothing is being measured".
export function Budget({ spent, cap }: { spent: number; cap?: number }) {
  if (!cap) {
    return <span className="mono" title="no budget configured">{money(spent)}</span>
  }
  const fraction = Math.min(spent / cap, 1)
  const tone = fraction >= 1 ? 'bad' : fraction >= 0.8 ? 'warn' : ''
  return (
    <div style={{ minWidth: 130 }}>
      <div className="bar">
        <div className={`bar-fill ${tone}`} style={{ width: `${Math.max(fraction * 100, 1.5)}%` }} />
      </div>
      <div className="bar-label">{money(spent)} / {money(cap)}</div>
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>
}

export function Notice({ tone, children }: { tone?: 'bad' | 'warn' | 'ok'; children: ReactNode }) {
  return <div className={`notice${tone ? ' ' + tone : ''}`}>{children}</div>
}

export function PageHead({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <>
      <div className="page-head"><h1>{title}</h1></div>
      {children && <p className="page-note">{children}</p>}
    </>
  )
}
