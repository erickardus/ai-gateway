import { useCallback, useEffect, useId, useRef, useState } from 'react'
import type { ReactNode } from 'react'

// Charts, drawn by hand in SVG.
//
// The same reason the gateway carries almost no dependencies applies here with
// more force: this console's whole point is to open instantly on an operator's
// laptop, and the charting libraries that would draw these five shapes ship
// several hundred kilobytes to do it. What is below is under four hundred lines
// and covers every form the console needs.
//
// Colour is assigned by the job it does, not by taste. A single series is drawn
// in the console's own clay; several series that are compared side by side take
// the validated categorical slots in fixed order; anything reporting a *state*
// — an outcome, a health — takes the status colours, which are never reused as
// a series so that a colour on this page means one thing or the other and never
// both. Every chart with more than one series carries a legend, because
// identity must never rest on colour alone.

export const SERIES = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)'] as const
export const ACCENT = 'var(--chart-accent)'

// useWidth measures the element rather than relying on a scaled viewBox.
//
// A viewBox with preserveAspectRatio="none" is the cheap way to make an SVG
// responsive, and it stretches every stroke and every glyph with the box. Real
// pixel coordinates cost this hook and keep a 2px line 2px wide at any width.
//
// It measures from a *callback ref* rather than from a mount-time effect, and
// that is the whole reason it works. Every chart below returns an early
// placeholder while its data is still in flight, so on the first render the
// measured element does not exist; an effect with an empty dependency list runs
// once against a null ref, gives up, and never runs again — leaving a chart that
// stays blank forever precisely because it had no data for a moment. A callback
// ref fires again when the real node attaches.
function useWidth<T extends HTMLElement>(): [(node: T | null) => void, number] {
  const [width, setWidth] = useState(0)
  const observer = useRef<ResizeObserver | null>(null)

  const ref = useCallback((node: T | null) => {
    observer.current?.disconnect()
    if (!node) return
    setWidth(node.clientWidth)
    observer.current = new ResizeObserver(([entry]) => setWidth(entry.contentRect.width))
    observer.current.observe(node)
  }, [])

  return [ref, width]
}

export type Series = {
  key: string
  label: string
  color: string
  // format renders this series' value in the tooltip and the legend. Money,
  // token counts and durations all read differently and a chart that formats
  // them alike reports "0.0012 requests".
  format?: (value: number) => string
}

export type Point = { start: string } & Record<string, number | string | undefined>

const PAD = { top: 10, right: 8, bottom: 20, left: 46 }

// niceMax rounds an axis top up to a round number, so the gridline an operator
// reads against is 400 rather than 383.
function niceMax(value: number): number {
  if (value <= 0) return 1
  const magnitude = Math.pow(10, Math.floor(Math.log10(value)))
  const scaled = value / magnitude
  const step = scaled <= 1 ? 1 : scaled <= 2 ? 2 : scaled <= 2.5 ? 2.5 : scaled <= 5 ? 5 : 10
  return step * magnitude
}

function shortNum(n: number): string {
  const abs = Math.abs(n)
  if (abs >= 1_000_000) return (n / 1_000_000).toFixed(abs >= 10_000_000 ? 0 : 1) + 'M'
  if (abs >= 1_000) return (n / 1_000).toFixed(abs >= 10_000 ? 0 : 1) + 'K'
  // A whole number is printed whole. This formatter is the default for every
  // axis and legend, and most of what it renders is a count of requests: "8.0
  // sign-ins" is not a number anybody writes.
  if (Number.isInteger(n)) return String(n)
  if (abs >= 1) return n.toFixed(1)
  return n.toFixed(abs < 0.01 ? 4 : 2)
}

function clockLabel(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
}

function fullLabel(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleString([], {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false,
  })
}

// roundedTopPath is a bar anchored to the baseline with only its data end
// rounded. Rounding all four corners detaches a bar from the axis it is
// measured against; rounding none makes a dense series read as a solid block.
function roundedTopPath(x: number, y: number, w: number, h: number, r: number): string {
  const radius = Math.max(0, Math.min(r, w / 2, h))
  if (h <= 0) return ''
  return [
    `M${x},${y + h}`,
    `V${y + radius}`,
    `Q${x},${y} ${x + radius},${y}`,
    `H${x + w - radius}`,
    `Q${x + w},${y} ${x + w},${y + radius}`,
    `V${y + h}`,
    'Z',
  ].join(' ')
}

type TooltipState = { index: number; x: number } | null

/** TimeSeries draws one or more measures over contiguous time buckets. */
export function TimeSeries({
  points,
  series,
  mode = 'area',
  height = 180,
  yFormat = shortNum,
  empty = 'No traffic in this window.',
  showLegend = true,
}: {
  points: Point[]
  series: Series[]
  /** `area` overlays filled lines, `stacked` sums them, `bars` draws columns. */
  mode?: 'area' | 'stacked' | 'bars'
  height?: number
  yFormat?: (value: number) => string
  empty?: ReactNode
  showLegend?: boolean
}) {
  const [ref, width] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<TooltipState>(null)
  const gradientId = useId()

  const plotW = Math.max(width - PAD.left - PAD.right, 10)
  const plotH = Math.max(height - PAD.top - PAD.bottom, 10)

  const value = (p: Point, key: string) => Number(p[key] ?? 0)

  const peak = points.reduce((acc, p) => {
    const total = mode === 'stacked'
      ? series.reduce((sum, s) => sum + value(p, s.key), 0)
      : Math.max(...series.map((s) => value(p, s.key)))
    return Math.max(acc, total)
  }, 0)
  const top = niceMax(peak)

  const x = (i: number) =>
    points.length <= 1 ? plotW / 2 : (i / (points.length - 1)) * plotW
  const y = (v: number) => plotH - (v / top) * plotH

  const onMove = useCallback((event: React.MouseEvent<SVGRectElement>) => {
    const box = event.currentTarget.getBoundingClientRect()
    const local = event.clientX - box.left
    const index = points.length <= 1
      ? 0
      : Math.round((local / Math.max(box.width, 1)) * (points.length - 1))
    setHover({ index: Math.max(0, Math.min(points.length - 1, index)), x: local })
  }, [points.length])

  if (points.length === 0) {
    return <div className="chart-empty" style={{ height }}>{empty}</div>
  }

  const ticks = [0, 0.5, 1].map((f) => ({ v: top * f, y: y(top * f) }))
  const active = hover ? points[hover.index] : null

  // Bar width leaves a 2px surface gap between neighbours so a dense series
  // reads as columns rather than as one filled region.
  const band = plotW / Math.max(points.length, 1)
  const barW = Math.max(band - 2, 1)

  return (
    <div className="chart" ref={ref}>
      {width > 0 && (
        <svg height={height} width={width} role="img"
          aria-label={series.map((s) => s.label).join(', ') + ' over time'}>
          <defs>
            {series.map((s, i) => (
              <linearGradient key={s.key} id={`${gradientId}-${i}`} x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor={s.color} stopOpacity={mode === 'stacked' ? 0.9 : 0.28} />
                <stop offset="100%" stopColor={s.color} stopOpacity={mode === 'stacked' ? 0.9 : 0.02} />
              </linearGradient>
            ))}
          </defs>

          <g transform={`translate(${PAD.left},${PAD.top})`}>
            <g className="chart-grid">
              {ticks.map((t) => (
                <line key={t.v} x1={0} x2={plotW} y1={t.y} y2={t.y} />
              ))}
            </g>
            {ticks.map((t) => (
              <text key={t.v} className="chart-tick" x={-8} y={t.y + 3.5} textAnchor="end">
                {yFormat(t.v)}
              </text>
            ))}

            {mode === 'bars' && series.map((s, si) => {
              // Bars stack when several series are asked for, because two sets
              // of columns at the same x would overplot.
              return points.map((p, i) => {
                const below = series.slice(0, si).reduce((sum, o) => sum + value(p, o.key), 0)
                const v = value(p, s.key)
                if (v <= 0) return null
                const yTop = y(below + v)
                const h = y(below) - yTop
                return (
                  <path
                    key={`${s.key}-${i}`}
                    d={roundedTopPath(i * band + 1, yTop, barW, h, 3)}
                    fill={s.color}
                    opacity={hover && hover.index !== i ? 0.55 : 1}
                  />
                )
              })
            })}

            {mode !== 'bars' && series.map((s, si) => {
              // below is this band's own baseline: the sum of every series
              // stacked underneath it at the same instant, and zero when the
              // series are drawn overlaid rather than stacked.
              const below = (p: Point) =>
                mode === 'stacked'
                  ? series.slice(0, si).reduce((sum, o) => sum + value(p, o.key), 0)
                  : 0

              const line = points
                .map((p, i) => `${i === 0 ? 'M' : 'L'}${x(i)},${y(below(p) + value(p, s.key))}`)
                .join(' ')

              // The closing edge walks back along the band's own baseline. It
              // has to visit the points in reverse while still reading each
              // point's own baseline — pairing a reversed x with a forward
              // value mirrors the floor, which fills the whole plot with the
              // topmost band and hides every series under it.
              const floor = mode === 'stacked'
                ? points
                    .map((_, k) => points.length - 1 - k)
                    .map((i) => `L${x(i)},${y(below(points[i]))}`)
                    .join(' ')
                : `L${x(points.length - 1)},${plotH} L${x(0)},${plotH}`

              // A band that is zero everywhere is left undrawn. Stacked, its
              // fill has no area and its line would sit exactly on top of the
              // band below, reading as that band's colour; the legend still
              // lists it, marked absent, so the measure is not silently
              // dropped from the chart's vocabulary.
              const present = points.some((p) => value(p, s.key) > 0)
              if (!present && mode === 'stacked') return null

              return (
                <g key={s.key}>
                  <path
                    d={`${line} ${floor} Z`}
                    fill={mode === 'stacked' ? s.color : `url(#${gradientId}-${si})`}
                    fillOpacity={mode === 'stacked' ? 0.85 : 1}
                    className={mode === 'stacked' ? 'chart-mark' : undefined}
                  />
                  {mode !== 'stacked' && <path className="chart-line" d={line} stroke={s.color} />}
                </g>
              )
            })}

            <g className="chart-axis">
              <line x1={0} x2={plotW} y1={plotH} y2={plotH} />
            </g>

            {hover && (
              <g>
                <line className="chart-crosshair"
                  x1={mode === 'bars' ? hover.index * band + band / 2 : x(hover.index)}
                  x2={mode === 'bars' ? hover.index * band + band / 2 : x(hover.index)}
                  y1={0} y2={plotH} />
                {mode !== 'bars' && series.map((s, si) => {
                  const p = points[hover.index]
                  const below = mode === 'stacked'
                    ? series.slice(0, si).reduce((sum, o) => sum + value(p, o.key), 0)
                    : 0
                  return (
                    <circle key={s.key} cx={x(hover.index)} cy={y(below + value(p, s.key))}
                      r={4} fill={s.color} stroke="var(--surface)" strokeWidth={2} />
                  )
                })}
              </g>
            )}

            {/* Time labels are dropped as the plot narrows rather than left to
                overlap. Three timestamps need about 220px between them to stay
                apart, and two colliding labels are less use than one. */}
            {plotW >= 120 && (
              <text className="chart-tick" x={0} y={plotH + 14}>{clockLabel(points[0].start)}</text>
            )}
            {points.length > 2 && plotW >= 220 && (
              <text className="chart-tick" x={plotW / 2} y={plotH + 14} textAnchor="middle">
                {clockLabel(points[Math.floor(points.length / 2)].start)}
              </text>
            )}
            <text className="chart-tick" x={plotW} y={plotH + 14} textAnchor="end">
              {clockLabel(points[points.length - 1].start)}
            </text>

            <rect className="chart-hit" x={0} y={0} width={plotW} height={plotH}
              onMouseMove={onMove} onMouseLeave={() => setHover(null)} />
          </g>
        </svg>
      )}

      {active && hover && (
        <div className="chart-tooltip" style={{
          left: Math.min(Math.max(hover.x + PAD.left - 70, 0), Math.max(width - 170, 0)),
          top: 4,
        }}>
          <div className="t-head">{fullLabel(active.start)}</div>
          {series.map((s) => (
            <div className="t-row" key={s.key}>
              <span className="swatch" style={{ background: s.color }} />
              <span className="name">{s.label}</span>
              <span className="val">{(s.format ?? shortNum)(Number(active[s.key] ?? 0))}</span>
            </div>
          ))}
        </div>
      )}

      {showLegend && series.length > 1 && (
        <div className="legend" style={{ marginTop: 10 }}>
          {series.map((s) => {
            const total = points.reduce((sum, p) => sum + value(p, s.key), 0)
            return (
              <span
                className="item"
                key={s.key}
                style={total > 0 ? undefined : { opacity: 0.45 }}
                title={total > 0 ? undefined : 'none recorded in this window'}
              >
                <span className="swatch" style={{ background: s.color }} />
                {s.label}
                {total === 0 && <span className="val">none</span>}
              </span>
            )
          })}
        </div>
      )}
    </div>
  )
}

/** Sparkline is one series with no axes, sized to sit inside a stat card. */
export function Sparkline({
  values,
  color = ACCENT,
  height = 28,
  label,
}: {
  values: number[]
  color?: string
  height?: number
  label?: string
}) {
  const [ref, width] = useWidth<HTMLDivElement>()
  const gradientId = useId()
  const peak = Math.max(...values, 0)

  if (values.length < 2) return <div style={{ height }} />

  const x = (i: number) => (i / (values.length - 1)) * Math.max(width, 1)
  // A flat series is drawn along the middle rather than along the floor, so
  // "steady" and "nothing at all" do not look the same.
  const y = (v: number) => (peak > 0 ? height - 2 - (v / peak) * (height - 4) : height / 2)
  const line = values.map((v, i) => `${i === 0 ? 'M' : 'L'}${x(i)},${y(v)}`).join(' ')

  return (
    <div className="chart" ref={ref} aria-label={label} role="img">
      {width > 0 && (
        <svg height={height} width={width}>
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={color} stopOpacity={0.3} />
              <stop offset="100%" stopColor={color} stopOpacity={0} />
            </linearGradient>
          </defs>
          <path d={`${line} L${x(values.length - 1)},${height} L0,${height} Z`} fill={`url(#${gradientId})`} />
          <path className="chart-line" d={line} stroke={color} strokeWidth={1.75} />
        </svg>
      )}
    </div>
  )
}

export type Share = { label: string; value: number; color: string; hint?: string }

/**
 * ShareBar splits one total into its parts along a single bar.
 *
 * A bar rather than a donut: the parts here are compared against each other and
 * against the whole, and a length on a common baseline is read accurately where
 * an angle is not.
 */
export function ShareBar({ parts, total, format = shortNum }: {
  parts: Share[]
  total?: number
  format?: (value: number) => string
}) {
  const sum = total ?? parts.reduce((acc, p) => acc + p.value, 0)
  const present = parts.filter((p) => p.value > 0)

  if (sum <= 0) {
    return <p className="hint" style={{ margin: 0 }}>Nothing recorded yet.</p>
  }

  return (
    <div>
      <div style={{ display: 'flex', gap: 2, height: 10, marginBottom: 12 }}>
        {present.map((p) => (
          <div
            key={p.label}
            title={`${p.label}: ${format(p.value)}`}
            style={{
              width: `${(p.value / sum) * 100}%`,
              background: p.color,
              borderRadius: 3,
              minWidth: 3,
            }}
          />
        ))}
      </div>
      <div className="legend">
        {present.map((p) => (
          <span className="item" key={p.label} title={p.hint}>
            <span className="swatch" style={{ background: p.color }} />
            {p.label}
            <span className="val">{format(p.value)}</span>
          </span>
        ))}
      </div>
    </div>
  )
}

export type BarRow = { name: string; value: number; sub?: ReactNode; onClick?: () => void }

/** BarList ranks a dimension, with the bar behind the label rather than beside it. */
export function BarList({ rows, format = shortNum, empty = 'Nothing yet.' }: {
  rows: BarRow[]
  format?: (value: number) => string
  empty?: ReactNode
}) {
  if (rows.length === 0) return <p className="hint" style={{ margin: 0 }}>{empty}</p>
  const peak = Math.max(...rows.map((r) => r.value), 0)
  return (
    <div className="barlist">
      {rows.map((row) => (
        <div
          className="barlist-row"
          key={row.name}
          onClick={row.onClick}
          style={row.onClick ? { cursor: 'pointer' } : undefined}
        >
          <span className="barlist-fill"
            style={{ width: `${peak > 0 ? Math.max((row.value / peak) * 100, 1.5) : 0}%` }} />
          <span className="barlist-name" title={row.name}>{row.name}</span>
          {row.sub && <span className="barlist-sub">{row.sub}</span>}
          <span className="barlist-val">{format(row.value)}</span>
        </div>
      ))}
    </div>
  )
}

/**
 * Percentiles draws p50/p95/p99 on one axis.
 *
 * Three numbers in a row would be smaller and would not answer the question the
 * operator has, which is how far the tail runs past the middle. Placing them on
 * a shared scale makes a p99 five times p50 look like one.
 */
export function Percentiles({ marks, max, format }: {
  marks: { label: string; value: number; tone?: 'accent' | 'warn' | 'bad' }[]
  max?: number
  format: (value: number) => string
}) {
  const top = niceMax(max ?? Math.max(...marks.map((m) => m.value), 0))
  const colour = (tone?: string) =>
    tone === 'bad' ? 'var(--bad-mark)' : tone === 'warn' ? 'var(--warn-mark)' : ACCENT

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 9 }}>
      {marks.map((m) => (
        <div key={m.label} style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span className="mono" style={{ width: 34, color: 'var(--muted)', fontSize: 11.5 }}>{m.label}</span>
          <span style={{ flex: 1, height: 8, background: 'var(--surface-3)', borderRadius: 4, overflow: 'hidden' }}>
            <span style={{
              display: 'block',
              height: '100%',
              borderRadius: 4,
              width: `${top > 0 ? Math.max((m.value / top) * 100, m.value > 0 ? 1.5 : 0) : 0}%`,
              background: colour(m.tone),
            }} />
          </span>
          <span className="num" style={{ width: 64 }}>{format(m.value)}</span>
        </div>
      ))}
    </div>
  )
}

/** Delta reports a change against the previous comparable period. */
export function Delta({ current, previous, invert = false, format = shortNum }: {
  current: number
  previous: number
  /** Set where up is bad — an error count, a latency. */
  invert?: boolean
  format?: (value: number) => string
}) {
  if (previous === 0 && current === 0) return null
  const change = previous === 0 ? 1 : (current - previous) / Math.abs(previous)
  if (Math.abs(change) < 0.005) {
    return <span className="hint" style={{ marginTop: 0 }}>flat vs previous</span>
  }
  const up = change > 0
  const good = invert ? !up : up
  return (
    <span
      className="hint"
      style={{ marginTop: 0, color: good ? 'var(--ok)' : 'var(--bad)', fontWeight: 550 }}
      title={`previous period: ${format(previous)}`}
    >
      {up ? '▲' : '▼'} {Math.abs(change * 100).toFixed(change > 10 ? 0 : 1)}% vs previous
    </span>
  )
}

// useKey binds a global shortcut. Kept here because the palette and the drawers
// are the only things that need one, and both are chart-adjacent chrome.
export function useKey(match: (e: KeyboardEvent) => boolean, fn: () => void) {
  const handler = useRef(fn)
  handler.current = fn
  useEffect(() => {
    const listener = (e: KeyboardEvent) => {
      if (match(e)) {
        e.preventDefault()
        handler.current()
      }
    }
    window.addEventListener('keydown', listener)
    return () => window.removeEventListener('keydown', listener)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
}

export { shortNum }
