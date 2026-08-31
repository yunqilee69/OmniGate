import dayjs from 'dayjs'

export type StatsDimension = 'route' | 'model' | 'provider' | 'key' | 'status' | 'error_code'
export type BucketSize = '1h' | '1d' | '1w'

export interface TimeseriesPoint {
  bucket: number
  total: number
  success: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  cost: number
  total_tokens: number
  fallback_count: number
  avg_ttft_ms: number
  avg_total_ms: number
}

export interface TimeseriesResponse {
  bucket_s: number
  points: TimeseriesPoint[]
}

export interface Overview {
  total: number
  success: number
  errors: number
  success_rate: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
  p95_ttft_ms: number
  p95_total_ms: number
  fallback_count: number
  fallback_rate: number
}

export interface Item {
  dim: string
  key_masked?: string
  key_name?: string
  total: number
  success: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  cost: number
  avg_ttft_ms: number
  avg_total_ms: number
  avg_retries: number
}

export interface FixtureRequest {
  from: number
  to: number
  bucket: BucketSize
  currency: 'USD' | 'CNY'
  dim: StatsDimension
  route?: string
  model?: string
  provider?: string
}

export interface FixturePayload {
  overview: Overview
  breakdown: Item[]
  timeseries: TimeseriesResponse
}

const dimensionRows: Record<StatsDimension, readonly string[]> = {
  route: ['chat-default', 'coding-fast', 'vision-review', 'embedding-search'],
  model: ['gpt-4o-mini', 'claude-3-7-sonnet', 'deepseek-v3', 'qwen-max'],
  provider: ['OpenAI', 'Anthropic', 'DeepSeek', '通义千问'],
  key: ['key-01', 'key-02', 'key-03', 'key-04'],
  status: ['success', 'error', 'client_error', 'cooldown'],
  error_code: ['429', '500', '502', 'timeout', 'context_length'],
}

const filterFactor = (request: FixtureRequest): number => {
  const activeFilters = [request.route, request.model, request.provider].filter(Boolean).length
  return Math.pow(0.68, activeFilters)
}

const pointFor = (bucket: number, index: number, request: FixtureRequest): TimeseriesPoint => {
  const scale = filterFactor(request)
  const hourly = request.bucket === '1h'
  const base = hourly ? 8 : 82
  const wave = (index * 17) % (hourly ? 11 : 47)
  const total = Math.max(1, Math.round((base + wave + (index % 5) * 3) * scale))
  const errors = Math.max(1, Math.round(total * (0.055 + (index % 4) * 0.012)))
  const success = total - errors
  const fallbackCount = Math.max(0, Math.round(total * (index % 5 === 0 ? 0.12 : 0.025)))
  const promptTokens = Math.round(total * (hourly ? 1120 : 9800) + index * 137)
  const completionTokens = Math.round(total * (hourly ? 540 : 4200) + index * 83)

  return {
    bucket,
    total,
    success,
    errors,
    prompt_tokens: promptTokens,
    completion_tokens: completionTokens,
    total_tokens: promptTokens + completionTokens,
    cost: (promptTokens * 0.0000018 + completionTokens * 0.0000062) * (request.currency === 'CNY' ? 7.2 : 1),
    fallback_count: fallbackCount,
    avg_ttft_ms: 290 + (index % 7) * 18 + (hourly ? 12 : 0),
    avg_total_ms: 1320 + (index % 6) * 105 + (hourly ? 60 : 0),
  }
}

const buildPoints = (request: FixtureRequest): TimeseriesPoint[] => {
  const start = dayjs().subtract(6, 'day').startOf('day')
  if (request.bucket === '1w') {
    return [pointFor(start.unix(), 0, request)]
  }
  const count = request.bucket === '1h' ? 168 : 7
  const step = request.bucket === '1h' ? 3600 : 86400
  return Array.from({ length: count }, (_, index) => pointFor(start.unix() + index * step, index, request))
}

const buildBreakdown = (request: FixtureRequest, points: TimeseriesPoint[]): Item[] => {
  const total = points.reduce((sum, point) => sum + point.total, 0)
  const cost = points.reduce((sum, point) => sum + point.cost, 0)
  return dimensionRows[request.dim].map((dim, index) => {
    const share = [0.38, 0.27, 0.2, 0.15][index] ?? 0.1
    const rowTotal = Math.max(1, Math.round(total * share))
    const errors = request.dim === 'status'
      ? dim === 'success' ? 0 : rowTotal
      : Math.max(1, Math.round(rowTotal * (0.04 + index * 0.02)))
    const success = request.dim === 'status' ? (dim === 'success' ? rowTotal : 0) : rowTotal - errors
    const promptTokens = Math.round(rowTotal * (90 + index * 14))
    const completionTokens = Math.round(rowTotal * (38 + index * 9))
    return {
      dim,
      key_masked: request.dim === 'key' ? `sk-live-••••${String(index + 17).padStart(2, '0')}` : undefined,
      key_name: request.dim === 'key' ? `Production key ${index + 1}` : undefined,
      total: rowTotal,
      success,
      errors,
      prompt_tokens: promptTokens,
      completion_tokens: completionTokens,
      cost: cost * share,
      avg_ttft_ms: 260 + index * 31,
      avg_total_ms: 1180 + index * 145,
      avg_retries: 0.06 + index * 0.08,
    }
  })
}

export function makeStatsFixture(request: FixtureRequest): FixturePayload {
  const points = buildPoints(request)
  const breakdown = buildBreakdown(request, points)
  const total = points.reduce((sum, point) => sum + point.total, 0)
  const success = points.reduce((sum, point) => sum + point.success, 0)
  const errors = points.reduce((sum, point) => sum + point.errors, 0)
  const promptTokens = points.reduce((sum, point) => sum + point.prompt_tokens, 0)
  const completionTokens = points.reduce((sum, point) => sum + point.completion_tokens, 0)
  const cost = points.reduce((sum, point) => sum + point.cost, 0)
  const fallbackCount = points.reduce((sum, point) => sum + point.fallback_count, 0)

  return {
    overview: {
      total,
      success,
      errors,
      success_rate: total ? success / total : 0,
      prompt_tokens: promptTokens,
      completion_tokens: completionTokens,
      total_tokens: promptTokens + completionTokens,
      cost,
      p95_ttft_ms: 510,
      p95_total_ms: 2140,
      fallback_count: fallbackCount,
      fallback_rate: total ? fallbackCount / total : 0,
    },
    breakdown,
    timeseries: { bucket_s: request.bucket === '1h' ? 3600 : request.bucket === '1d' ? 86400 : 604800, points },
  }
}
