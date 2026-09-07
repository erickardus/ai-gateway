// Inline icons.
//
// Drawn here rather than pulled from an icon package for the same reason the
// charts are: fourteen glyphs is not worth a dependency, and an inline path
// inherits currentColor, which is what lets one nav item recolour on hover
// without a second asset for the dark theme.
//
// All are 16×16 on a 24-unit grid, stroked rather than filled, so they sit at
// the weight of the text beside them.

type Props = { size?: number; className?: string }

function Svg({ size = 16, className, children }: Props & { children: React.ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
    >
      {children}
    </svg>
  )
}

export const Icon = {
  overview: (p: Props) => (
    <Svg {...p}><rect x="3" y="3" width="7" height="9" rx="1.5" /><rect x="14" y="3" width="7" height="5" rx="1.5" /><rect x="14" y="12" width="7" height="9" rx="1.5" /><rect x="3" y="16" width="7" height="5" rx="1.5" /></Svg>
  ),
  analytics: (p: Props) => (
    <Svg {...p}><path d="M3 3v16.5A1.5 1.5 0 0 0 4.5 21H21" /><path d="M7 15l3.5-4.5 3 2.5L20 6" /></Svg>
  ),
  traffic: (p: Props) => (
    <Svg {...p}><path d="M3 12h4l2.5-7 5 14 2.5-7h4" /></Svg>
  ),
  keys: (p: Props) => (
    <Svg {...p}><circle cx="8" cy="15" r="4" /><path d="M10.8 12.2 21 2m-4 4 2.5 2.5M14 9l2.5 2.5" /></Svg>
  ),
  spend: (p: Props) => (
    <Svg {...p}><circle cx="12" cy="12" r="9" /><path d="M12 7v10M14.5 9.5a2.5 2.5 0 0 0-5 .3c0 2.7 5 1.5 5 4.2a2.5 2.5 0 0 1-5 .3" /></Svg>
  ),
  routing: (p: Props) => (
    <Svg {...p}><circle cx="5" cy="12" r="2.5" /><circle cx="19" cy="5.5" r="2.5" /><circle cx="19" cy="18.5" r="2.5" /><path d="M7.4 11 16.6 6.5M7.4 13l9.2 4.5" /></Svg>
  ),
  scopes: (p: Props) => (
    <Svg {...p}><rect x="8.5" y="3" width="7" height="5" rx="1.2" /><rect x="2.5" y="16" width="7" height="5" rx="1.2" /><rect x="14.5" y="16" width="7" height="5" rx="1.2" /><path d="M12 8v4M6 16v-2.5h12V16" /></Svg>
  ),
  audit: (p: Props) => (
    <Svg {...p}><path d="M14 3H6.5A1.5 1.5 0 0 0 5 4.5v15A1.5 1.5 0 0 0 6.5 21h11a1.5 1.5 0 0 0 1.5-1.5V8z" /><path d="M14 3v5h5M9 13h6M9 17h4" /></Svg>
  ),
  ops: (p: Props) => (
    <Svg {...p}><circle cx="12" cy="12" r="3" /><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-2.9 1.2 2 2 0 1 1-4 0 1.7 1.7 0 0 0-2.9-1.2l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.7 1.7 0 0 0 3 15a2 2 0 1 1 0-4 1.7 1.7 0 0 0 1.2-2.9l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.7 1.7 0 0 0 10 4.2a2 2 0 1 1 4 0 1.7 1.7 0 0 0 2.9 1.2l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1A1.7 1.7 0 0 0 21 11a2 2 0 1 1 0 4z" /></Svg>
  ),
  search: (p: Props) => (
    <Svg {...p}><circle cx="11" cy="11" r="7" /><path d="m20 20-3.9-3.9" /></Svg>
  ),
  refresh: (p: Props) => (
    <Svg {...p}><path d="M21 12a9 9 0 1 1-2.6-6.4M21 4v5h-5" /></Svg>
  ),
  sun: (p: Props) => (
    <Svg {...p}><circle cx="12" cy="12" r="4" /><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" /></Svg>
  ),
  moon: (p: Props) => (
    <Svg {...p}><path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5" /></Svg>
  ),
  monitor: (p: Props) => (
    <Svg {...p}><rect x="2.5" y="4" width="19" height="12.5" rx="1.5" /><path d="M8.5 20.5h7M12 16.5v4" /></Svg>
  ),
  close: (p: Props) => (
    <Svg {...p}><path d="M18 6 6 18M6 6l12 12" /></Svg>
  ),
  menu: (p: Props) => (
    <Svg {...p}><path d="M3 6h18M3 12h18M3 18h18" /></Svg>
  ),
  plus: (p: Props) => (
    <Svg {...p}><path d="M12 5v14M5 12h14" /></Svg>
  ),
  download: (p: Props) => (
    <Svg {...p}><path d="M12 3v12m0 0 4.5-4.5M12 15l-4.5-4.5M4 19h16" /></Svg>
  ),
  signOut: (p: Props) => (
    <Svg {...p}><path d="M9 21H5.5A1.5 1.5 0 0 1 4 19.5v-15A1.5 1.5 0 0 1 5.5 3H9M16 16l5-4-5-4M21 12H9" /></Svg>
  ),
  alert: (p: Props) => (
    <Svg {...p}><circle cx="12" cy="12" r="9" /><path d="M12 7.5v5.5M12 16.2v.3" /></Svg>
  ),
  check: (p: Props) => (
    <Svg {...p}><path d="M20 6 9.5 17 4 11.5" /></Svg>
  ),
  inbox: (p: Props) => (
    <Svg {...p}><path d="M3 13h5l1.5 3h5l1.5-3h5" /><path d="M5.5 4h13l2.5 9v6a1.5 1.5 0 0 1-1.5 1.5H4.5A1.5 1.5 0 0 1 3 19v-6z" /></Svg>
  ),
  shield: (p: Props) => (
    <Svg {...p}><path d="M12 3 4.5 6v6c0 4.4 3.1 7.9 7.5 9 4.4-1.1 7.5-4.6 7.5-9V6z" /><path d="m9 12 2 2 4-4" /></Svg>
  ),
  copy: (p: Props) => (
    <Svg {...p}><rect x="9" y="9" width="12" height="12" rx="2" /><path d="M5 15H4.5A1.5 1.5 0 0 1 3 13.5v-9A1.5 1.5 0 0 1 4.5 3h9A1.5 1.5 0 0 1 15 4.5V5" /></Svg>
  ),
  bolt: (p: Props) => (
    <Svg {...p}><path d="M13 2 4 14h7l-1 8 9-12h-7z" /></Svg>
  ),
}
