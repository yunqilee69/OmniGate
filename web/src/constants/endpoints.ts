export const ENDPOINT_META: Record<string, { path: string; hint?: string }> = {
  completions: { path: '/v1/chat/completions', hint: 'OpenAI' },
  messages: { path: '/v1/messages', hint: 'Anthropic' },
  responses: { path: '/v1/responses', hint: 'OpenAI' },
  embedding: { path: '/v1/embeddings', hint: '向量化' },
  rerank: { path: '/v1/rerank', hint: '重排' },
  image: { path: '/v1/images/generations', hint: '生图' },
  mcp: { path: '/v1/mcp/{route_name}', hint: 'MCP 工具聚合' },
}

export const ENDPOINT_ORDER = Object.keys(ENDPOINT_META)

export const FAMILY_TABS: { key: string; label: string; endpoints: string[] }[] = [
  { key: 'chat', label: '对话', endpoints: ['completions', 'messages', 'responses'] },
  { key: 'embedding', label: '向量', endpoints: ['embedding'] },
  { key: 'rerank', label: '重排', endpoints: ['rerank'] },
  { key: 'image', label: '生图', endpoints: ['image'] },
  { key: 'mcp', label: 'MCP', endpoints: ['mcp'] },
]

export const FAMILY_KEYS = FAMILY_TABS.map((f) => f.key)

export const endpointFamily = (ep: string) =>
  FAMILY_TABS.find((f) => f.endpoints.includes(ep))?.key ?? ep

export const familyLabel = (key: string) =>
  FAMILY_TABS.find((f) => f.key === key)?.label ?? key

export const familyDefaultEndpoint = (key: string) =>
  FAMILY_TABS.find((f) => f.key === key)?.endpoints[0] ?? 'completions'

export const endpointLabel = (v?: string) => {
  if (!v) return v
  const family = FAMILY_TABS.find((f) => f.key === v)
  if (family) return family.label
  return familyLabel(endpointFamily(v))
}
