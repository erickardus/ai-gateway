// The gateway's own API, mounted alongside the app it serves.
const BASE = '/ui/api'

// CSRF_HEADER mirrors ui.CSRFHeader in Go. A request that changes state must
// carry it, which a cross-site form post or image tag cannot do.
const CSRF_HEADER = 'x-gateway-ui'

export class ApiError extends Error {
  readonly status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

// The gateway answers failures in the Anthropic error envelope, which is what
// its inference endpoints already use. Reading it here rather than inventing a
// second error shape means the UI reports exactly what curl would.
type ErrorEnvelope = { error?: { message?: string } }

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(BASE + path, {
    ...init,
    headers: {
      ...(init?.body ? { 'content-type': 'application/json' } : {}),
      ...(init?.method && init.method !== 'GET' ? { [CSRF_HEADER]: '1' } : {}),
      ...init?.headers,
    },
  })
  if (!response.ok) {
    let message = response.statusText
    try {
      const body = (await response.json()) as ErrorEnvelope
      if (body.error?.message) message = body.error.message
    } catch {
      // A response with no JSON body is a proxy or a network problem, and its
      // status line is the most useful thing left to report.
    }
    throw new ApiError(response.status, message)
  }
  if (response.status === 204) return undefined as T
  return (await response.json()) as T
}

export const api = {
  get: <T,>(path: string) => request<T>(path),
  post: <T,>(path: string, body?: unknown) =>
    request<T>(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body) }),
  // download saves a response to a file rather than parsing it.
  //
  // It goes through fetch instead of pointing a link at the URL so that a
  // failure arrives as the gateway's own error message. A bare <a download>
  // would hand the operator a file containing the JSON error envelope and call
  // it a spend report.
  download: async (path: string, filename: string) => {
    const response = await fetch(BASE + path)
    if (!response.ok) {
      let message = response.statusText
      try {
        const body = (await response.json()) as ErrorEnvelope
        if (body.error?.message) message = body.error.message
      } catch {
        // As in request: no JSON body means the status line is all there is.
      }
      throw new ApiError(response.status, message)
    }
    const url = URL.createObjectURL(await response.blob())
    const link = document.createElement('a')
    link.href = url
    link.download = filename
    link.click()
    URL.revokeObjectURL(url)
  },
}

export type Session = { subject: string }

export type DeploymentHealth = {
  deployment: string
  model_name: string
  format: string
  auth_mode: string
  api_base: string
  cooling_down: boolean
  in_flight: number
}

export type Pricing = {
  input_per_1m: number
  output_per_1m: number
  cache_read_per_1m: number
  cache_write_per_1m: number
}

export type Totals = {
  requests: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost: number
  billable_requests: number
  cache_savings: number
  window_start: string
}

export type PromptCacheStatus = {
  affinity: boolean
  affinity_ttl: string
  affinity_max_in_flight_lead: number
  inject: boolean
  affinity_note?: string
}

export type Overview = {
  status: string
  strategy: string
  healthy_deployments: number
  total_deployments: number
  deployments: DeploymentHealth[]
  prompt_cache: PromptCacheStatus
  features: {
    metrics: boolean
    response_cache: boolean
    spend: boolean
    sso: boolean
    rbac: boolean
    shared_state: boolean
  }
  traffic: { held: number; capacity: number; note: string }
  shared_state?: { degradations: number; reachable: boolean }
  keys?: Record<string, number>
  spend?: {
    requests: number
    billable_requests: number
    input_tokens: number
    output_tokens: number
    cache_read_tokens: number
    cache_write_tokens: number
    cost: number
    cache_savings: number
  }
}

export type Deployment = DeploymentHealth & {
  weight: number
  rpm?: number
  tpm?: number
  upstream_model?: string
  billable: boolean
  priced: boolean
  cost: Pricing
  spend: Totals
}

export type DeploymentsResponse = {
  deployments: Deployment[]
  healthy_deployments: number
  total_deployments: number
  strategy: string
  prompt_cache: PromptCacheStatus
}

export type Key = {
  hash: string
  alias?: string
  models?: string[]
  rpm_limit?: number
  tpm_limit?: number
  allow_passthrough?: boolean
  blocked?: boolean
  created_at: string
  expires_at?: string
  max_budget?: number
  budget_duration?: number
  subject?: string
  device?: string
  scope?: string
  spend_subject: string
  spend: number
}

export type KeysResponse = {
  keys: Key[]
  count: number
  scopes: string[] | null
  groups: string[]
}

export type SpendRow = { subject: string; alias?: string } & Totals

export type SpendResponse = {
  entries: SpendRow[] | null
  count: number
  total_cost: number
  total_cache_savings: number
  note: string
  cache_savings_note: string
}

// A bucket of consumption over one interval, from the durable spend history.
//
// It is a different endpoint from SpendResponse above, and deliberately: that
// one reads the ledger that enforces budgets and reports the current window,
// this one reads the history and reports what was spent whether or not the
// window it fell in has since rolled over.
export type SpendBucket = {
  start: string
  subject: string
  alias?: string
  requests: number
  billable_requests: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost: number
  cache_savings: number
}

export type SpendHistoryResponse = {
  kind: string
  subject: string
  interval: string
  from: string
  to: string
  buckets: SpendBucket[] | null
  count: number
  total_cost: number
  total_cache_savings: number
  // True when the buckets cover the same money at more than one level — a
  // scope report over every subject returns an organisation and its teams —
  // so total_cost is a sum of what was returned rather than what was spent.
  overlapping: boolean
  note: string
}

export type Usage = {
  input_tokens?: number
  output_tokens?: number
  cache_read_tokens?: number
  cache_write_tokens?: number
}

export type TrafficRecord = {
  id: string
  at: string
  key_alias?: string
  spend_subject?: string
  scope?: string
  model_group?: string
  deployment?: string
  format?: string
  streaming: boolean
  outcome: string
  reject_reason?: string
  status_class?: string
  retries: number
  fallbacks: number
  prompt_affinity?: string
  usage: Usage
  cost: number
  billable: boolean
  cache_savings: number
  latency_ms: number
  ttft_ms?: number
  stream_completed?: boolean
  throughput_tps?: number
  request_bytes?: number
  response_bytes?: number
}

export type TrafficResponse = {
  records: TrafficRecord[]
  held: number
  capacity: number
  note: string
}

export type Scope = {
  id: string
  kind: string
  alias?: string
  parent?: string
  models?: string[]
  rpm_limit?: number
  tpm_limit?: number
  max_budget?: number
  budget_duration?: number
  blocked?: boolean
  spend: number
  keys: number
}

export type ScopesResponse = { scopes: Scope[]; enabled: boolean; note: string }

// Analytics is the traffic ring aggregated server-side.
//
// It reads the same buffer the Traffic page lists row by row, which is why it
// carries the same caveat: bounded, per process, and therefore a sample of
// recent traffic rather than a history. The gateway does the bucketing because
// summing two thousand records in the browser on every poll is work the process
// that already holds them can do once.
export type AnalyticsBucket = {
  start: string
  requests: number
  errors: number
  rejected: number
  cache_hits: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost: number
  cache_savings: number
  latency_p50_ms: number
  latency_p95_ms: number
}

export type AnalyticsTotals = {
  requests: number
  errors: number
  rejected: number
  cache_hits: number
  streamed: number
  retried: number
  fell_back: number
  billable_requests: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost: number
  cache_savings: number
}

export type AnalyticsLatency = {
  p50_ms: number
  p95_ms: number
  p99_ms: number
  max_ms: number
  ttft_p50_ms: number
  ttft_p95_ms: number
  throughput_p50_tps: number
}

export type Breakdown = {
  name: string
  requests: number
  errors: number
  cost: number
  input_tokens: number
  output_tokens: number
  latency_p50_ms: number
  latency_p95_ms: number
}

export type AnalyticsResponse = {
  window: string
  from: string
  to: string
  bucket_seconds: number
  series: AnalyticsBucket[] | null
  totals: AnalyticsTotals
  latency: AnalyticsLatency
  outcomes: { outcome: string; count: number }[] | null
  reject_reasons: { reason: string; count: number }[] | null
  groups: Breakdown[] | null
  deployments: Breakdown[] | null
  keys: Breakdown[] | null
  note: string
}

export type AuditRecord = {
  seq: number
  at: string
  prev?: string
  action: string
  actor?: { kind: string; id?: string; subject?: string; remote?: string }
  target_kind?: string
  target?: string
  outcome: string
  detail?: Record<string, string>
  hash: string
  error?: string
}

export type AuditResponse = {
  readable: boolean
  sink: string
  records: AuditRecord[] | null
  count: number
  sealed: boolean
  seal_reason?: string
  verified: boolean
  verified_through?: number
  verify_error?: string
  note: string
}

export type GroupConfig = {
  name: string
  format?: string
  deployments: string[] | null
  fallbacks?: string[] | null
  context_window_fallbacks?: string[] | null
  content_policy_fallbacks?: string[] | null
}

export type ConfigResponse = {
  strategy: string
  groups: GroupConfig[] | null
  router: Record<string, number>
  translation: { enabled: boolean; default_max_tokens?: number; note?: string }
  response_cache: {
    enabled: boolean
    ttl_seconds?: number
    scope?: string
    max_entries?: number
    shared?: boolean
    // Present only where the cache implementation can report its own size.
    entries?: number
  }
  prompt_cache: PromptCacheStatus
  // history_target and the other *_target fields are redacted host/database
  // descriptions, never a DSN: the console must never be a place a connection
  // string with a password in it can be read off the screen.
  spend: { ledger: boolean; history: boolean; store_path?: string; history_target?: string }
  audit: { enabled: boolean; sink: string; readable: boolean; path?: string; target?: string }
  limits: Record<string, number>
  keys: { store_kind: string; master_key_set: boolean; header_names: string[] | null; store_target?: string }
  sso: { enabled: boolean; issuer?: string }
  rbac: { enabled: boolean; organizations?: number; teams?: number; projects?: number }
  note?: string
}
