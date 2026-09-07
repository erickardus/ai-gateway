import { useCallback, useEffect, useState } from 'react'

export type Theme = 'light' | 'dark' | 'system'

const STORAGE_KEY = 'ai-gateway.theme'

function read(): Theme {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw === 'light' || raw === 'dark' || raw === 'system') return raw
  } catch {
    // A browser with site data blocked still gets a working console; it simply
    // follows the OS and forgets the choice on reload.
  }
  return 'system'
}

// apply stamps the root element, which is what the stylesheet's [data-theme]
// blocks key off. "system" removes the attribute rather than resolving it here,
// so prefers-color-scheme keeps working live — an operator whose laptop dims at
// sunset gets the dark console without reloading.
function apply(theme: Theme) {
  const root = document.documentElement
  if (theme === 'system') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', theme)
}

/** useTheme persists the operator's choice between light, dark and the OS. */
export function useTheme(): [Theme, (next: Theme) => void, () => void] {
  const [theme, setTheme] = useState<Theme>(read)

  useEffect(() => { apply(theme) }, [theme])

  const set = useCallback((next: Theme) => {
    setTheme(next)
    try {
      localStorage.setItem(STORAGE_KEY, next)
    } catch {
      // Ignored for the reason above.
    }
  }, [])

  // cycle is what the single toolbar button does: three states behind one
  // control, because a console that only toggles two can never be handed back
  // to the OS once it has been touched.
  const cycle = useCallback(() => {
    set(theme === 'system' ? 'light' : theme === 'light' ? 'dark' : 'system')
  }, [theme, set])

  return [theme, set, cycle]
}

// Applied before React mounts, so the first paint is already the right colour
// rather than a white flash the operator watches resolve.
apply(read())
