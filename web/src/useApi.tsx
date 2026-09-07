import {
  createContext, useCallback, useContext, useEffect, useMemo, useRef, useState,
} from 'react'
import { ApiError, api } from './api'

type State<T> = {
  data: T | undefined
  error: string | undefined
  loading: boolean
  reload: () => void
}

type Refresh = {
  /** paused stops every interval on the page at once. */
  paused: boolean
  setPaused: (next: boolean) => void
  /** nonce changes to force every mounted query to re-fetch. */
  nonce: number
  refreshAll: () => void
  lastUpdated: number | undefined
  reportUpdate: () => void
}

const RefreshContext = createContext<Refresh>({
  paused: false,
  setPaused: () => {},
  nonce: 0,
  refreshAll: () => {},
  lastUpdated: undefined,
  reportUpdate: () => {},
})

export const useRefresh = () => useContext(RefreshContext)

/**
 * RefreshProvider holds the console's polling state in one place.
 *
 * Per-page intervals were fine while every page polled one endpoint, and stop
 * being fine once a page polls four: an operator reading a table wants the
 * whole page to hold still, not to hunt for the four checkboxes that would stop
 * it moving. One switch pauses everything, and one button refetches everything.
 */
export function RefreshProvider({ children }: { children: React.ReactNode }) {
  const [paused, setPausedState] = useState(false)
  const [nonce, setNonce] = useState(0)
  const [lastUpdated, setLastUpdated] = useState<number>()

  // Polling stops while the tab is in the background. A console left open on a
  // second monitor overnight would otherwise spend the night asking a gateway
  // for traffic nobody is looking at.
  useEffect(() => {
    const onVisibility = () => {
      if (document.visibilityState === 'visible') setNonce((n) => n + 1)
    }
    document.addEventListener('visibilitychange', onVisibility)
    return () => document.removeEventListener('visibilitychange', onVisibility)
  }, [])

  const value = useMemo<Refresh>(() => ({
    paused,
    setPaused: setPausedState,
    nonce,
    refreshAll: () => setNonce((n) => n + 1),
    lastUpdated,
    reportUpdate: () => setLastUpdated(Date.now()),
  }), [paused, nonce, lastUpdated])

  return <RefreshContext.Provider value={value}>{children}</RefreshContext.Provider>
}

/**
 * useApi fetches a path and optionally re-fetches on an interval.
 *
 * It is hand-rolled rather than pulled from a data-fetching library because the
 * whole app makes a dozen distinct requests, and a cache with invalidation
 * rules would be more code than the thing it manages. What it does need to get
 * right is the failure modes an admin console actually hits: a 401 when the
 * session expires, which the shell turns back into a sign-in page; a response
 * arriving after the component has moved on; and a poll that keeps the previous
 * answer on screen while the next one is in flight, so a live table does not
 * blink to empty every three seconds.
 */
export function useApi<T>(
  path: string | null,
  intervalMs = 0,
  onUnauthorized?: () => void,
): State<T> {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(path !== null)
  const [nonce, setNonce] = useState(0)
  const unauthorized = useRef(onUnauthorized)
  unauthorized.current = onUnauthorized

  const { paused, nonce: globalNonce, reportUpdate } = useRefresh()
  const report = useRef(reportUpdate)
  report.current = reportUpdate

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  useEffect(() => {
    if (path === null) return
    let live = true

    const run = async () => {
      try {
        const result = await api.get<T>(path)
        if (!live) return
        setData(result)
        setError(undefined)
        report.current()
      } catch (err) {
        if (!live) return
        if (err instanceof ApiError && err.status === 401) {
          unauthorized.current?.()
          return
        }
        setError(err instanceof Error ? err.message : String(err))
      } finally {
        if (live) setLoading(false)
      }
    }

    void run()
    if (intervalMs <= 0 || paused) return () => { live = false }
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') void run()
    }, intervalMs)
    return () => {
      live = false
      clearInterval(timer)
    }
  }, [path, intervalMs, nonce, globalNonce, paused])

  return { data, error, loading, reload }
}
