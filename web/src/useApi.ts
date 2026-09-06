import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, api } from './api'

type State<T> = {
  data: T | undefined
  error: string | undefined
  loading: boolean
  reload: () => void
}

// useApi fetches a path and optionally re-fetches on an interval.
//
// It is hand-rolled rather than pulled from a data-fetching library because the
// whole app makes eight distinct requests, and a cache with invalidation rules
// would be more code than the thing it manages. What it does need to get right
// is the two failure modes an admin console actually hits: a 401 when the
// session expires, which the shell turns back into a sign-in page, and a
// response arriving after the component has moved on.
export function useApi<T>(path: string | null, intervalMs = 0, onUnauthorized?: () => void): State<T> {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(path !== null)
  const [nonce, setNonce] = useState(0)
  const unauthorized = useRef(onUnauthorized)
  unauthorized.current = onUnauthorized

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
    if (intervalMs <= 0) return () => { live = false }
    const timer = setInterval(() => void run(), intervalMs)
    return () => {
      live = false
      clearInterval(timer)
    }
  }, [path, intervalMs, nonce])

  return { data, error, loading, reload }
}
