import { useState } from 'react'
import type { SpendBucket } from '../api'
import { money } from '../format'

// Trend draws daily spend as bars.
//
// Hand-drawn SVG rather than a charting library, for the reason the gateway
// carries almost no dependencies: this is one chart of one series, the whole of
// it is below, and a library would be several hundred kilobytes shipped into a
// console whose entire point is to load instantly on an operator's laptop.
//
// It answers one question — is this rising or falling — which is the question
// a single number on a page cannot answer and the reason the history exists.
export function Trend({ buckets, height = 120 }: { buckets: SpendBucket[]; height?: number }) {
  const [hover, setHover] = useState<number | null>(null)

  // Buckets arrive one per subject per day. A chart of "what did we spend"
  // wants them summed per day, and summing here rather than asking the gateway
  // for it keeps the endpoint answering one shape.
  const byDay = new Map<string, { cost: number; requests: number }>()
  for (const b of buckets) {
    const day = b.start.slice(0, 10)
    const acc = byDay.get(day) ?? { cost: 0, requests: 0 }
    acc.cost += b.cost
    acc.requests += b.requests
    byDay.set(day, acc)
  }
  const days = [...byDay.entries()].sort(([a], [b]) => a.localeCompare(b))

  if (days.length === 0) {
    return <p className="hint">No spend recorded over this range.</p>
  }

  const peak = Math.max(...days.map(([, v]) => v.cost), 0)
  const active = hover !== null ? days[hover] : null

  return (
    <div className="trend">
      <div className="trend-head">
        <span className="hint">
          {active ? (
            <>
              <strong>{active[0]}</strong> · {money(active[1].cost)} over {active[1].requests.toLocaleString()} requests
            </>
          ) : (
            <>
              {days.length} day{days.length === 1 ? '' : 's'} · peak {money(peak)}
            </>
          )}
        </span>
      </div>
      <svg
        className="trend-svg"
        viewBox={`0 0 ${Math.max(days.length * 10, 10)} ${height}`}
        preserveAspectRatio="none"
        role="img"
        aria-label="Daily spend"
        style={{ height }}
      >
        {days.map(([day, value], i) => {
          // A day with spend always gets a visible sliver: a bar rounded to
          // zero reads as a day with no traffic, which is a different fact.
          const scaled = peak > 0 ? (value.cost / peak) * (height - 4) : 0
          const barHeight = value.cost > 0 ? Math.max(scaled, 1.5) : 0
          return (
            <rect
              key={day}
              x={i * 10 + 1.5}
              y={height - barHeight}
              width={7}
              height={barHeight}
              className={hover === i ? 'trend-bar active' : 'trend-bar'}
              onMouseEnter={() => setHover(i)}
              onMouseLeave={() => setHover(null)}
            >
              <title>{`${day}: ${money(value.cost)}`}</title>
            </rect>
          )
        })}
      </svg>
      <div className="trend-axis hint">
        <span>{days[0][0]}</span>
        <span>{days[days.length - 1][0]}</span>
      </div>
    </div>
  )
}
