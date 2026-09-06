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
