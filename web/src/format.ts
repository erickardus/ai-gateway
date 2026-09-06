// Formatting helpers shared by every table, kept in one place so a cost renders
// identically wherever it appears.

// Money is rendered to four decimals below a cent, because per-request costs at
// current token prices are routinely $0.0003 and rounding them to two decimals
// would show a busy gateway spending nothing.
export function money(value: number): string {
  if (value === 0) return '$0.00'
  const abs = Math.abs(value)
  const digits = abs < 0.01 ? 4 : 2
  return (value < 0 ? '-$' : '$') + abs.toFixed(digits)
}

export function count(value: number | undefined): string {
  return (value ?? 0).toLocaleString()
}

// Token counts are abbreviated because the interesting comparison is order of
// magnitude — 1.2M against 45K — and eight digits of precision in a table cell
// is noise nobody reads.
export function tokens(value: number | undefined): string {
  const n = value ?? 0
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1) + 'M'
  if (n >= 1_000) return (n / 1_000).toFixed(n >= 10_000 ? 0 : 1) + 'K'
  return String(n)
}

export function ms(value: number | undefined): string {
  if (!value) return '—'
  if (value >= 10_000) return (value / 1000).toFixed(1) + 's'
  if (value >= 1000) return (value / 1000).toFixed(2) + 's'
  return Math.round(value) + 'ms'
}

// Durations arrive from Go as nanoseconds, which is what time.Duration marshals
// to. Rendering them means dividing by a billion, and forgetting to is the most
// likely wrong number on the page.
// goDurationField spells a window the way Go can parse it back.
//
// It exists because goDuration below is for reading — "30d" is what an operator
// wants in a table — and Go's time.ParseDuration has no day unit. Prefilling an
// edit form with the readable spelling made every save of such a key fail with
// "must be a positive Go duration string", including a save that only changed
// the alias.
export function goDurationField(nanos: number | undefined): string {
  if (!nanos) return ''
  const seconds = nanos / 1e9
  if (seconds % 3600 === 0) return seconds / 3600 + 'h'
  if (seconds % 60 === 0) return seconds / 60 + 'm'
  return seconds + 's'
}

export function goDuration(nanos: number | undefined): string {
  if (!nanos) return 'lifetime'
  const seconds = nanos / 1e9
  if (seconds % 86400 === 0) return seconds / 86400 + 'd'
  if (seconds % 3600 === 0) return seconds / 3600 + 'h'
  if (seconds % 60 === 0) return seconds / 60 + 'm'
  return seconds + 's'
}

export function timestamp(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleTimeString([], { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0')
}

export function dateTime(iso: string | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString([], {
    year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false,
  })
}

export function ago(iso: string | undefined): string {
  if (!iso) return '—'
  const seconds = (Date.now() - new Date(iso).getTime()) / 1000
  const future = seconds < 0
  const s = Math.abs(seconds)
  const say = (n: number, unit: string) => `${Math.round(n)}${unit}${future ? ' from now' : ' ago'}`
  if (s < 60) return say(s, 's')
  if (s < 3600) return say(s / 60, 'm')
  if (s < 86400) return say(s / 3600, 'h')
  return say(s / 86400, 'd')
}

export function shortHash(hash: string | undefined): string {
  return hash ? hash.slice(0, 10) : '—'
}
