import { useEffect, useMemo, useRef, useState } from 'react'
import type { CSSProperties, ReactNode } from 'react'
import {
  AutoComplete, Button, Card, Collapse, Image, Input, InputNumber, Select, Space, Tabs, Tag, Tooltip, Typography, Upload, message,
} from 'antd'
import { ClearOutlined, DownloadOutlined, SendOutlined, StopOutlined, ToolOutlined } from '@ant-design/icons'
import { Link } from 'react-router-dom'
import { Bubble } from '@ant-design/x'
import XMarkdown from '@ant-design/x-markdown'
import { api } from '../api'
import { isRecord } from '../utils/guards'
interface Route {
  id: number
  name: string
  endpoint: string
  targets?: { model_id: number }[]
}

interface VirtualKey {
  id: number
  key_value: string
  name: string
  status: string
}

interface ModelInfo {
  id: number
  type: string
}

interface EmbeddingResp {
  data?: { index: number; embedding: number[] }[]
  usage?: { prompt_tokens?: number; total_tokens?: number }
}

interface RerankResp {
  results?: { index: number; relevance_score: number }[]
}

interface ImageResp {
  data?: { url?: string; b64_json?: string }[]
  usage?: { input_tokens?: number; output_tokens?: number; total_tokens?: number }
}

interface ToolCall {
  id: string
  type: 'function'
  function: { name: string; arguments: string }
}

interface Usage {
  prompt_tokens?: number
  completion_tokens?: number
  total_tokens?: number
}

// 流式累加器：content 逐字追加；tool_calls 按 delta.index 分片拼装
interface StreamAcc {
  reasoning: string
  content: string
  calls: Map<number, { id?: string; name?: string; args: string }>
  finish: string
  usage?: Usage
}

// 对话消息：assistant 消息可携带 reasoning（思考过程）、tool_calls 与统计信息；tool 消息为 MCP 执行结果。
type Msg =
  | { role: 'user'; content: string }
  | {
      role: 'assistant'
      content: string
      reasoning?: string
      tool_calls?: ToolCall[]
      usage?: Usage
      latency_ms?: number
      aborted?: boolean
    }
  | { role: 'tool'; tool_call_id: string; name: string; content: string; is_error?: boolean; latency_ms?: number }

type OpenAIMessage =
  | { role: 'user' | 'system'; content: string }
  | { role: 'assistant'; content: string; tool_calls?: ToolCall[] }
  | { role: 'tool'; tool_call_id: string; content: string }

interface OpenAITool {
  type: 'function'
  function: { name: string; description: string; parameters: Record<string, unknown> }
}

// 已发现的 MCP 工具。key 为暴露给模型的唯一名；wireName 为发给 MCP 端点的原始名。
interface McpToolInfo {
  key: string
  route: string
  wireName: string
  description: string
  schema: Record<string, unknown>
}

interface McpToolWire {
  name: string
  description?: unknown
  inputSchema?: unknown
}

const MCP_PROTOCOL_VERSION = '2024-11-05'

// ---------- 本地草稿 ----------
// 测试配置与结果临时存于 localStorage：切走菜单或刷新后回来，仍可继续使用与查看。
// 虚拟密钥明文（vkKey）与图片 base64（参考图、生图结果）体积大或敏感，不落盘。
const DRAFT_KEY = 'omnigate.playground.draft.v1'
const DRAFT_TABS = ['chat', 'embedding', 'rerank', 'image']

interface Draft {
  tab: string
  chatRoute?: string
  vkId?: number
  mcpRoutes: string[]
  systemPrompt: string
  temperature: number | null
  maxRounds: number
  msgs: Msg[]
  input: string
  embRoute?: string
  embText: string
  embMs: number | null
  embResult: EmbeddingResp | null
  rrkRoute?: string
  rrkQuery: string
  rrkDocs: string
  rrkTopN: number | null
  rrkMs: number | null
  rrkDocArr: string[]
  rrkResult: RerankResp | null
  imgRoute?: string
  imgPrompt: string
  imgSize: string
  imgRatio: string
  imgN: number
  imgMs: number | null
}

const draftStr = (v: unknown, fallback = ''): string => (typeof v === 'string' ? v : fallback)
const draftOptStr = (v: unknown): string | undefined => (typeof v === 'string' && v ? v : undefined)
const draftNum = (v: unknown, fallback: number): number =>
  typeof v === 'number' && Number.isFinite(v) ? v : fallback
const draftOptNum = (v: unknown): number | null =>
  typeof v === 'number' && Number.isFinite(v) ? v : null
const draftList = (v: unknown): unknown[] => (Array.isArray(v) ? v : [])
const draftStrList = (v: unknown): string[] =>
  draftList(v).filter((s): s is string => typeof s === 'string')

function draftUsage(v: unknown): Usage | undefined {
  if (!isRecord(v)) return undefined
  const usage: Usage = {}
  const prompt = draftOptNum(v.prompt_tokens)
  const completion = draftOptNum(v.completion_tokens)
  const total = draftOptNum(v.total_tokens)
  if (prompt !== null) usage.prompt_tokens = prompt
  if (completion !== null) usage.completion_tokens = completion
  if (total !== null) usage.total_tokens = total
  return usage
}

// 草稿逐字段校验：草稿可能来自旧版本或被手工改动，任何损坏都退化为可渲染的最小值，
// 避免脏数据在渲染期抛错把整个页面打崩。
function draftToolCalls(v: unknown): ToolCall[] | undefined {
  const calls: ToolCall[] = []
  for (const c of draftList(v)) {
    if (!isRecord(c) || typeof c.id !== 'string' || !isRecord(c.function)) continue
    const name = c.function.name
    const args = c.function.arguments
    if (typeof name !== 'string' || typeof args !== 'string') continue
    calls.push({ id: c.id, type: 'function', function: { name, arguments: args } })
  }
  return calls.length ? calls : undefined
}

function draftMsgs(v: unknown): Msg[] {
  const msgs: Msg[] = []
  for (const m of draftList(v)) {
    if (!isRecord(m) || typeof m.content !== 'string') continue
    if (m.role === 'user') {
      msgs.push({ role: 'user', content: m.content })
    } else if (m.role === 'assistant') {
      const msg: Extract<Msg, { role: 'assistant' }> = { role: 'assistant', content: m.content }
      if (typeof m.reasoning === 'string') msg.reasoning = m.reasoning
      const calls = draftToolCalls(m.tool_calls)
      if (calls) msg.tool_calls = calls
      const usage = draftUsage(m.usage)
      if (usage) msg.usage = usage
      if (typeof m.latency_ms === 'number') msg.latency_ms = m.latency_ms
      if (m.aborted === true) msg.aborted = true
      msgs.push(msg)
    } else if (m.role === 'tool' && typeof m.tool_call_id === 'string') {
      const msg: Extract<Msg, { role: 'tool' }> = {
        role: 'tool',
        tool_call_id: m.tool_call_id,
        name: draftStr(m.name),
        content: m.content,
      }
      if (m.is_error === true) msg.is_error = true
      if (typeof m.latency_ms === 'number') msg.latency_ms = m.latency_ms
      msgs.push(msg)
    }
  }
  return msgs
}

function draftEmbeddingResult(v: unknown): EmbeddingResp | null {
  if (!isRecord(v)) return null
  const data = draftList(v.data).flatMap((item) => {
    if (!isRecord(item) || typeof item.index !== 'number') return []
    const embedding = draftList(item.embedding).filter((n): n is number => typeof n === 'number')
    return [{ index: item.index, embedding }]
  })
  const usage = draftUsage(v.usage)
  return {
    data,
    usage: usage ? { prompt_tokens: usage.prompt_tokens, total_tokens: usage.total_tokens } : undefined,
  }
}

function draftRerankResult(v: unknown): RerankResp | null {
  if (!isRecord(v)) return null
  const results = draftList(v.results).flatMap((r) =>
    isRecord(r) && typeof r.index === 'number'
      ? [{ index: r.index, relevance_score: draftNum(r.relevance_score, NaN) }]
      : [],
  )
  return { results }
}

function loadDraft(): Draft {
  let raw: Record<string, unknown> = {}
  try {
    const stored = localStorage.getItem(DRAFT_KEY)
    const parsed: unknown = stored === null ? null : JSON.parse(stored)
    if (isRecord(parsed)) raw = parsed
  } catch {
    /* 草稿缺失或损坏：按空草稿处理 */
  }
  const tab = draftStr(raw.tab, 'chat')
  return {
    tab: DRAFT_TABS.includes(tab) ? tab : 'chat',
    chatRoute: draftOptStr(raw.chatRoute),
    vkId: draftOptNum(raw.vkId) ?? undefined,
    mcpRoutes: draftStrList(raw.mcpRoutes),
    systemPrompt: draftStr(raw.systemPrompt),
    temperature: draftOptNum(raw.temperature),
    maxRounds: draftNum(raw.maxRounds, 5),
    msgs: draftMsgs(raw.msgs),
    input: draftStr(raw.input),
    embRoute: draftOptStr(raw.embRoute),
    embText: draftStr(raw.embText),
    embMs: draftOptNum(raw.embMs),
    embResult: draftEmbeddingResult(raw.embResult),
    rrkRoute: draftOptStr(raw.rrkRoute),
    rrkQuery: draftStr(raw.rrkQuery),
    rrkDocs: draftStr(raw.rrkDocs),
    rrkTopN: draftOptNum(raw.rrkTopN),
    rrkMs: draftOptNum(raw.rrkMs),
    rrkDocArr: draftStrList(raw.rrkDocArr),
    rrkResult: draftRerankResult(raw.rrkResult),
    imgRoute: draftOptStr(raw.imgRoute),
    imgPrompt: draftStr(raw.imgPrompt),
    imgSize: draftStr(raw.imgSize, '1K'),
    imgRatio: draftStr(raw.imgRatio, '1:1'),
    imgN: draftNum(raw.imgN, 1),
    imgMs: draftOptNum(raw.imgMs),
  }
}

// 写入节流：流式输出期间 msgs 每 50ms 变一次，避免每个 chunk 都序列化整份草稿。
let draftTimer: number | null = null
let draftPending: Draft | null = null
let draftSession = 0
let draftWarned = false

// 页面挂载时领取会话号；重新挂载后换号，让仍在后台跑的流停止写草稿，避免覆盖新数据。

function queueDraftSave(draft: Draft, session: number): void {
  if (session !== draftSession) return
  draftPending = draft
  if (draftTimer !== null) return
  draftTimer = window.setTimeout(() => {
    draftTimer = null
    flushDraftSave()
  }, 400)
}

// 立即落盘（组件卸载等关键点调用，避免节流窗口内的改动丢失）。
function flushDraftSave(): void {
  if (draftTimer !== null) {
    window.clearTimeout(draftTimer)
    draftTimer = null
  }
  const draft = draftPending
  draftPending = null
  if (!draft) return
  try {
    localStorage.setItem(DRAFT_KEY, JSON.stringify(draft))
    return
  } catch {
    /* 配额超限或存储不可用：下面退化为精简草稿再试 */
  }
  try {
    localStorage.setItem(
      DRAFT_KEY,
      JSON.stringify({
        ...draft,
        embResult: null,
        embMs: null,
        rrkResult: null,
        rrkMs: null,
        rrkDocArr: [],
        imgMs: null,
      }),
    )
    if (!draftWarned) {
      draftWarned = true
      message.warning('本地草稿超出浏览器存储配额，已省略测试结果')
    }
  } catch {
    if (!draftWarned) {
      draftWarned = true
      message.warning('本地草稿过大或浏览器存储不可用，本次未能保存')
    }
  }
}


function errText(e: unknown): string {
  if (e instanceof Error && e.message) return e.message
  return String(e)
}

function isAbortError(e: unknown): boolean {
  return e instanceof DOMException && e.name === 'AbortError'
}

function asMcpTools(v: unknown): McpToolWire[] {
  if (!Array.isArray(v)) return []
  return v.filter(
    (t): t is McpToolWire =>
      isRecord(t) && 'name' in t && typeof t.name === 'string',
  )
}

function newStreamAcc(): StreamAcc {
  return { reasoning: '', content: '', calls: new Map(), finish: '' }
}

// 应用一个 SSE 事件（OpenAI chunk 格式）：delta.reasoning_content / delta.content 追加、delta.tool_calls 按 index 拼装。
// reasoning_content 为 GLM/DeepSeek/Claude 等推理模型的非官方扩展字段，思考过程与正文分开渲染。
function applyStreamEvent(evt: unknown, acc: StreamAcc): void {
  if (!isRecord(evt) || !Array.isArray(evt.choices)) return
  const choice = evt.choices[0]
  if (!isRecord(choice)) return
  const delta = isRecord(choice.delta) ? choice.delta : {}
  if (typeof delta.reasoning_content === 'string') acc.reasoning += delta.reasoning_content
  if (typeof delta.content === 'string') acc.content += delta.content
  if (Array.isArray(delta.tool_calls)) {
    for (const tc of delta.tool_calls) {
      if (!isRecord(tc) || typeof tc.index !== 'number') continue
      const cur = acc.calls.get(tc.index) ?? { args: '' }
      if (typeof tc.id === 'string') cur.id = tc.id
      if (isRecord(tc.function)) {
        if (typeof tc.function.name === 'string') cur.name = (cur.name ?? '') + tc.function.name
        if (typeof tc.function.arguments === 'string') cur.args += tc.function.arguments
      }
      acc.calls.set(tc.index, cur)
    }
  }
  if (typeof choice.finish_reason === 'string' && choice.finish_reason) acc.finish = choice.finish_reason
  if (isRecord(evt.usage)) {
    acc.usage = {
      prompt_tokens: typeof evt.usage.prompt_tokens === 'number' ? evt.usage.prompt_tokens : undefined,
      completion_tokens: typeof evt.usage.completion_tokens === 'number' ? evt.usage.completion_tokens : undefined,
      total_tokens: typeof evt.usage.total_tokens === 'number' ? evt.usage.total_tokens : undefined,
    }
  }
}

// 拼装完成的工具调用；缺少 id/name 的分片（上游异常）丢弃
function assembleToolCalls(acc: StreamAcc): ToolCall[] {
  return [...acc.calls.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([, c]) =>
      c.id && c.name
        ? { id: c.id, type: 'function' as const, function: { name: c.name, arguments: c.args } }
        : null,
    )
    .filter((t): t is ToolCall => t !== null)
}

// 读取 SSE 流，逐事件回调；data: [DONE] 或流结束返回。\n 与 \r\n 换行均兼容。
async function readSSE(body: ReadableStream<Uint8Array>, onEvent: (evt: unknown) => void): Promise<void> {
  const reader = body.getReader()
  const dec = new TextDecoder()
  let buf = ''
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buf += dec.decode(value, { stream: true })
      for (;;) {
        const m = /\r?\n\r?\n/.exec(buf)
        if (!m) break
        const block = buf.slice(0, m.index)
        buf = buf.slice(m.index + m[0].length)
        const data = block
          .split('\n')
          .filter((l) => l.startsWith('data:'))
          .map((l) => l.slice(5).trimStart())
          .join('\n')
        if (!data) continue // 注释/心跳行
        if (data === '[DONE]') return
        try {
          onEvent(JSON.parse(data))
        } catch {
          // 非 JSON 事件跳过
        }
      }
    }
  } finally {
    reader.releaseLock()
  }
}

function contentToText(content: unknown): string {
  if (Array.isArray(content)) {
    const parts = content
      .map((p) => (isRecord(p) && p.type === 'text' && typeof p.text === 'string' ? p.text : null))
      .filter((t): t is string => t !== null)
    if (parts.length > 0) return parts.join('\n')
  }
  if (content === undefined || content === null) return ''
  try {
    return JSON.stringify(content, null, 2)
  } catch {
    return String(content)
  }
}

function prettyJson(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

function toApiMessages(msgs: Msg[]): OpenAIMessage[] {
  return msgs.map((m) => {
    if (m.role === 'user') return { role: 'user' as const, content: m.content }
    if (m.role === 'tool') return { role: 'tool' as const, tool_call_id: m.tool_call_id, content: m.content }
    return m.tool_calls?.length
      ? { role: 'assistant' as const, content: m.content || '', tool_calls: m.tool_calls }
      : { role: 'assistant' as const, content: m.content || '' }
  })
}

// 提取上游 JSON-RPC/OpenAI 错误消息；无结构化消息时回退到状态码。
async function throwHttpError(res: Response): Promise<never> {
  const body: unknown = await res.json().catch(() => null)
  const msg =
    isRecord(body) && isRecord(body.error) && typeof body.error.message === 'string'
      ? body.error.message
      : `${res.status} ${res.statusText}`
  throw new Error(msg)
}

// 路由选择器：按家族过滤后的路由列表；familyLabel 用于占位与空态文案。
function RouteSelect({ routes, value, onChange, familyLabel }: {
  routes: Route[]
  value?: string
  onChange: (v?: string) => void
  familyLabel: string
}) {
  return (
    <Select
      showSearch
      style={{ width: '100%', marginTop: 4 }}
      placeholder={`选择${familyLabel}路由`}
      value={value}
      onChange={onChange}
      options={routes.map((r) => ({ value: r.name, label: r.name }))}
      notFoundContent={
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          暂无{familyLabel}路由，<Link to="/routes">去创建</Link>
        </Typography.Text>
      }
    />
  )
}

// 虚拟密钥选择器：四个测试 Tab 共用同一选中值。
function VkSelect({ vks, value, onChange }: {
  vks: VirtualKey[]
  value?: number
  onChange: (v?: number) => void
}) {
  return (
    <Select
      showSearch
      style={{ width: '100%', marginTop: 4 }}
      placeholder="选择虚拟密钥"
      value={value}
      onChange={onChange}
      options={vks.map((k) => ({ value: k.id, label: `${k.name}（${k.key_value}）` }))}
      notFoundContent={
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          暂无可用虚拟密钥，<Link to="/keys">去创建</Link>
        </Typography.Text>
      }
    />
  )
}

export default function PlaygroundPage() {
  // 首屏水合本地草稿；之后以各 state 为准，草稿在每次渲染后节流回写
  const [saved] = useState(loadDraft)
  const [baseLoaded, setBaseLoaded] = useState(false)
  const [routes, setRoutes] = useState<Route[]>([])
  const [vks, setVks] = useState<VirtualKey[]>([])
  const [models, setModels] = useState<ModelInfo[]>([])
  const [chatRoute, setChatRoute] = useState<string | undefined>(saved.chatRoute)
  const [vkId, setVkId] = useState<number | undefined>(saved.vkId)
  const [vkKey, setVkKey] = useState('')
  const [mcpRoutes, setMcpRoutes] = useState<string[]>(saved.mcpRoutes)
  const [tools, setTools] = useState<McpToolInfo[]>([])
  const [toolsLoading, setToolsLoading] = useState(false)

  const [tab, setTab] = useState(saved.tab)

  // Embedding 测试
  const [embRoute, setEmbRoute] = useState<string | undefined>(saved.embRoute)
  const [embText, setEmbText] = useState(saved.embText)
  const [embBusy, setEmbBusy] = useState(false)
  const [embMs, setEmbMs] = useState<number | null>(saved.embMs)
  const [embResult, setEmbResult] = useState<EmbeddingResp | null>(saved.embResult)

  // Rerank 测试
  const [rrkRoute, setRrkRoute] = useState<string | undefined>(saved.rrkRoute)
  const [rrkQuery, setRrkQuery] = useState(saved.rrkQuery)
  const [rrkDocs, setRrkDocs] = useState(saved.rrkDocs)
  const [rrkTopN, setRrkTopN] = useState<number | null>(saved.rrkTopN)
  const [rrkBusy, setRrkBusy] = useState(false)
  const [rrkMs, setRrkMs] = useState<number | null>(saved.rrkMs)
  const [rrkDocArr, setRrkDocArr] = useState<string[]>(saved.rrkDocArr)
  const [rrkResult, setRrkResult] = useState<RerankResp | null>(saved.rrkResult)

  // 生图测试
  const [imgRoute, setImgRoute] = useState<string | undefined>(saved.imgRoute)
  const [imgPrompt, setImgPrompt] = useState(saved.imgPrompt)
  const [imgSize, setImgSize] = useState(saved.imgSize)
  const [imgRatio, setImgRatio] = useState(saved.imgRatio)
  const [imgImages, setImgImages] = useState<string[]>([])
  const [imgN, setImgN] = useState(saved.imgN)
  const [imgBusy, setImgBusy] = useState(false)
  const [imgMs, setImgMs] = useState<number | null>(saved.imgMs)
  const [imgResult, setImgResult] = useState<ImageResp | null>(null)

  const [systemPrompt, setSystemPrompt] = useState(saved.systemPrompt)
  const [temperature, setTemperature] = useState<number | null>(saved.temperature)
  const [maxRounds, setMaxRounds] = useState(saved.maxRounds)

  const [msgs, setMsgs] = useState<Msg[]>(saved.msgs)
  // 每条 assistant 消息的思考面板展开状态；流式中的最后一条强制展开，完成后回落到用户选择
  const [reasoningKeys, setReasoningKeys] = useState<Record<number, string[]>>({})
  const [input, setInput] = useState(saved.input)
  const [sending, setSending] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const listRef = useRef<HTMLDivElement>(null)

  // ---------- 本地草稿回写 ----------
  const draft: Draft = {
    tab,
    chatRoute,
    vkId,
    mcpRoutes,
    systemPrompt,
    temperature,
    maxRounds,
    msgs,
    input,
    embRoute,
    embText,
    embMs,
    embResult,
    rrkRoute,
    rrkQuery,
    rrkDocs,
    rrkTopN,
    rrkMs,
    rrkDocArr,
    rrkResult,
    imgRoute,
    imgPrompt,
    imgSize,
    imgRatio,
    imgN,
    imgMs,
  }
  const draftRef = useRef(draft)
  const sessionRef = useRef(0)
  // 认领会话号，并在卸载（切菜单/关页）时立即落盘
  useEffect(() => {
    sessionRef.current = ++draftSession
    return () => flushDraftSave()
  }, [])
  // 每次渲染后排队回写；节流合并，流式期间的高频 setMsgs 不会频繁序列化
  useEffect(() => {
    draftRef.current = draft
    queueDraftSave(draft, sessionRef.current)
  })

  // MCP 路由 → 会话 ID（initialize 响应头 MCP-Session-Id）；动态增删故用 Map
  const mcpSessions = useRef<Map<string, string>>(new Map())

  const mcpRouteOptions = useMemo(() => routes.filter((r) => r.endpoint === 'mcp'), [routes])
  const activeVks = useMemo(() => vks.filter((k) => k.status === 'active'), [vks])

  // 路由 → 模型类型集合：按家族过滤路由（targets × models 联查；无 type 视为 chat）
  const modelsById = useMemo(() => new Map(models.map((m) => [m.id, m])), [models])
  const routeFamilies = useMemo(() => {
    const map: Record<string, Set<string>> = {}
    for (const r of routes) {
      const set = new Set<string>()
      for (const t of r.targets ?? []) {
        set.add(modelsById.get(t.model_id)?.type || 'chat')
      }
      map[r.name] = set
    }
    return map
  }, [routes, modelsById])
  const routesFor = (family: string) =>
    routes.filter((r) => r.endpoint !== 'mcp' && routeFamilies[r.name]?.has(family))
  const chatRoutes = useMemo(() => routesFor('chat'), [routesFor])

  const toolMap = useMemo(() => {
    const m: Record<string, McpToolInfo> = {}
    for (const t of tools) m[t.key] = t
    return m
  }, [tools])

  const openAITools = useMemo<OpenAITool[]>(
    () =>
      tools.map((t) => ({
        type: 'function',
        function: { name: t.key, description: t.description, parameters: t.schema },
      })),
    [tools],
  )

  useEffect(() => {
    Promise.all([
      api<Route[]>('GET', '/api/routes'),
      api<VirtualKey[]>('GET', '/api/virtual-keys'),
      api<ModelInfo[]>('GET', '/api/models'),
    ])
      .then(([rs, ks, ms]) => {
        setRoutes(rs)
        setVks(ks)
        setModels(ms)
        // 草稿里的选择可能已失效（路由/密钥被删）：清掉，避免 reveal 报错与选择器空显
        const routeNames = new Set(rs.map((r) => r.name))
        const validRoute = (cur?: string) => (cur && routeNames.has(cur) ? cur : undefined)
        setChatRoute(validRoute)
        setEmbRoute(validRoute)
        setRrkRoute(validRoute)
        setImgRoute(validRoute)
        setMcpRoutes((cur) => cur.filter((name) => routeNames.has(name)))
        setVkId((cur) => (cur !== undefined && ks.some((k) => k.id === cur) ? cur : undefined))
        setBaseLoaded(true)
      })
      .catch((e: unknown) => message.error(errText(e)))
  }, [])

  // 切换虚拟 key 时按需 reveal 明文（仅驻留内存，不落 localStorage）
  // 首屏是草稿水合出的 key：等基础数据到位并校验通过后才发请求
  useEffect(() => {
    setVkKey('')
    if (!baseLoaded || !vkId) return
    api<{ key_value: string }>('GET', `/api/virtual-keys/${vkId}/reveal-key`)
      .then((d) => setVkKey(d.key_value))
      .catch((e: unknown) => message.error(errText(e)))
  }, [baseLoaded, vkId])

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight })
  }, [msgs])

  // MCP JSON-RPC 调用。无会话时由调用方先 initialize（响应头带回 MCP-Session-Id）。
  const mcpRpc = async (
    route: string,
    method: string,
    params: unknown,
    signal?: AbortSignal,
  ): Promise<unknown> => {
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${vkKey}`,
    }
    const sid = mcpSessions.current.get(route)
    if (sid) headers['MCP-Session-Id'] = sid

    const res = await fetch(`/v1/mcp/${encodeURIComponent(route)}`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ jsonrpc: '2.0', id: Date.now() % 1e9, method, params }),
      signal,
    })
    const newSid = res.headers.get('MCP-Session-Id')
    if (newSid) mcpSessions.current.set(route, newSid)
    if (!res.ok) await throwHttpError(res)
    const body: unknown = await res.json()
    if (isRecord(body) && body.error != null) {
      throw new Error(
        isRecord(body.error) && typeof body.error.message === 'string'
          ? body.error.message
          : JSON.stringify(body.error),
      )
    }
    return isRecord(body) ? body.result : undefined
  }

  const ensureMcpSession = async (route: string, signal?: AbortSignal) => {
    if (mcpSessions.current.has(route)) return
    await mcpRpc(
      route,
      'initialize',
      {
        protocolVersion: MCP_PROTOCOL_VERSION,
        capabilities: {},
        clientInfo: { name: 'omnigate-playground', version: '1.0.0' },
      },
      signal,
    )
  }

  const loadTools = async () => {
    if (!vkKey) {
      message.warning('请先选择虚拟密钥')
      return
    }
    if (!mcpRoutes.length) return
    setToolsLoading(true)
    try {
      const found: McpToolInfo[] = []
      for (const route of mcpRoutes) {
        mcpSessions.current.delete(route)
        await ensureMcpSession(route)
        const result = await mcpRpc(route, 'tools/list', {})
        const list = asMcpTools(isRecord(result) ? result.tools : undefined)
        for (const t of list) {
          // 多路由时加路由前缀避免跨路由同名冲突；调用时经 toolMap 映射回原始名
          found.push({
            key: mcpRoutes.length > 1 ? `${route}__${t.name}` : t.name,
            route,
            wireName: t.name,
            description: typeof t.description === 'string' ? t.description : '',
            schema: isRecord(t.inputSchema) ? t.inputSchema : { type: 'object' },
          })
        }
      }
      setTools(found)
      message.success(`已加载 ${found.length} 个工具`)
    } catch (e: unknown) {
      message.error(`加载 MCP 工具失败：${errText(e)}`)
    } finally {
      setToolsLoading(false)
    }
  }

  const execToolCall = async (
    tc: ToolCall,
    signal: AbortSignal,
  ): Promise<{ content: string; is_error: boolean; latency_ms: number }> => {
    const t0 = performance.now()
    const info = toolMap[tc.function.name]
    if (!info) {
      return { content: `工具 ${tc.function.name} 未注册，请重新加载 MCP 工具`, is_error: true, latency_ms: 0 }
    }
    let args: unknown = {}
    if (tc.function.arguments) {
      try {
        args = JSON.parse(tc.function.arguments)
      } catch {
        return { content: `工具参数不是合法 JSON：${tc.function.arguments}`, is_error: true, latency_ms: 0 }
      }
    }
    try {
      await ensureMcpSession(info.route, signal)
      const result = await mcpRpc(info.route, 'tools/call', { name: info.wireName, arguments: args }, signal)
      return {
        content: contentToText(isRecord(result) ? result.content : undefined),
        is_error: isRecord(result) && result.isError === true,
        latency_ms: Math.round(performance.now() - t0),
      }
    } catch (e: unknown) {
      if (isAbortError(e)) throw e
      return { content: errText(e), is_error: true, latency_ms: Math.round(performance.now() - t0) }
    }
  }

  const send = async () => {
    const text = input.trim()
    if (!text || sending) return
    if (!chatRoute) {
      message.warning('请先选择对话路由')
      return
    }
    if (!vkKey) {
      message.warning('请先选择虚拟密钥')
      return
    }

    const ctrl = new AbortController()
    abortRef.current = ctrl
    setSending(true)
    setInput('')

    const convo: Msg[] = [...msgs, { role: 'user', content: text }]
    setMsgs(convo)

    try {
      for (let round = 0; round < Math.max(1, maxRounds); round++) {
        const apiMsgs = toApiMessages(convo)
        if (systemPrompt.trim()) apiMsgs.unshift({ role: 'system', content: systemPrompt.trim() })
        const body: Record<string, unknown> = { model: chatRoute, stream: true, messages: apiMsgs }
        if (temperature !== null && temperature !== undefined) body.temperature = temperature
        if (openAITools.length) {
          body.tools = openAITools
          body.tool_choice = 'auto'
        }

        const t0 = performance.now()
        const res = await fetch('/v1/chat/completions', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${vkKey}` },
          body: JSON.stringify(body),
          signal: ctrl.signal,
        })
        if (!res.ok) await throwHttpError(res)
        if (!res.body) throw new Error('上游未返回响应流')

        // 占位 assistant 消息：随 chunk 到达实时更新（50ms 节流渲染）
        const acc = newStreamAcc()
        const assistant: Extract<Msg, { role: 'assistant' }> = { role: 'assistant', content: '' }
        convo.push(assistant)
        let lastFlush = 0
        const flush = (force = false) => {
          const now = performance.now()
          if (!force && now - lastFlush < 50) return
          assistant.content = acc.content
          if (acc.reasoning) assistant.reasoning = acc.reasoning
          setMsgs([...convo])
          // 组件已卸载（切走菜单）时 setMsgs 不再生效，这里直接把结果写进草稿，后台完成的回答不丢
          queueDraftSave({ ...draftRef.current, msgs: convo }, sessionRef.current)
          lastFlush = now
        }
        await readSSE(res.body, (evt) => {
          applyStreamEvent(evt, acc)
          flush()
        })
        flush(true)
        assistant.usage = acc.usage

        const calls = assembleToolCalls(acc)
        if (!calls.length || acc.finish !== 'tool_calls') {
          setMsgs([...convo])
          break
        }

        // 模型请求调用工具：执行 MCP tools/call 并回填结果继续下一轮
        assistant.tool_calls = calls
        setMsgs([...convo])
        for (const tc of calls) {
          const r = await execToolCall(tc, ctrl.signal)
          convo.push({
            role: 'tool',
            tool_call_id: tc.id,
            name: tc.function.name,
            content: r.content,
            is_error: r.is_error,
            latency_ms: r.latency_ms,
          })
        }
        setMsgs([...convo])
      }
    } catch (e: unknown) {
      if (isAbortError(e)) {
        // 中止：保留已流出的部分内容
        const last = convo[convo.length - 1]
        if (last?.role === 'assistant') last.aborted = true
        setMsgs([...convo])
      } else {
        message.error(errText(e))
      }
    } finally {
      setSending(false)
      abortRef.current = null
    }
  }

  const clearChat = () => {
    abortRef.current?.abort()
    setMsgs([])
    setReasoningKeys({})
    setInput('')
    for (const [route, sid] of mcpSessions.current) {
      fetch(`/v1/mcp/${encodeURIComponent(route)}`, {
        method: 'DELETE',
        headers: { 'MCP-Session-Id': sid, Authorization: `Bearer ${vkKey}` },
      }).catch(() => {})
    }
    mcpSessions.current.clear()
  }

  const toolResultOf = (id: string): Extract<Msg, { role: 'tool' }> | undefined => {
    for (let i = msgs.length - 1; i >= 0; i--) {
      const m = msgs[i]
      if (m.role === 'tool' && m.tool_call_id === id) return m
    }
    return undefined
  }

  // ---------- Embedding ----------
  const runEmbedding = async () => {
    const inputs = embText.split('\n').map((s) => s.trim()).filter(Boolean)
    if (!embRoute) { message.warning('请先选择向量路由'); return }
    if (!vkKey) { message.warning('请先选择虚拟密钥'); return }
    if (!inputs.length) { message.warning('请输入至少一条文本'); return }
    setEmbBusy(true)
    try {
      const t0 = performance.now()
      const res = await fetch('/v1/embeddings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${vkKey}` },
        body: JSON.stringify({ model: embRoute, input: inputs }),
      })
      if (!res.ok) await throwHttpError(res)
      const body = (await res.json()) as EmbeddingResp
      setEmbMs(Math.round(performance.now() - t0))
      setEmbResult({ data: body.data ?? [], usage: body.usage })
    } catch (e: unknown) {
      message.error(errText(e))
    } finally {
      setEmbBusy(false)
    }
  }

  // ---------- Rerank ----------
  const runRerank = async () => {
    const docs = rrkDocs.split('\n').map((s) => s.trim()).filter(Boolean)
    if (!rrkRoute) { message.warning('请先选择重排路由'); return }
    if (!vkKey) { message.warning('请先选择虚拟密钥'); return }
    if (!rrkQuery.trim() || !docs.length) { message.warning('请输入 query 与候选文档'); return }
    setRrkBusy(true)
    try {
      const t0 = performance.now()
      const body: Record<string, unknown> = { model: rrkRoute, query: rrkQuery.trim(), documents: docs }
      if (rrkTopN) body.top_n = rrkTopN
      const res = await fetch('/v1/rerank', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${vkKey}` },
        body: JSON.stringify(body),
      })
      if (!res.ok) await throwHttpError(res)
      const parsed = (await res.json()) as RerankResp
      setRrkMs(Math.round(performance.now() - t0))
      setRrkDocArr(docs)
      setRrkResult({ results: parsed.results ?? [] })
    } catch (e: unknown) {
      message.error(errText(e))
    } finally {
      setRrkBusy(false)
    }
  }

  // ---------- 生图 ----------
  const runImage = async () => {
    const prompt = imgPrompt.trim()
    if (!imgRoute) { message.warning('请先选择生图路由'); return }
    if (!vkKey) { message.warning('请先选择虚拟密钥'); return }
    if (!prompt) { message.warning('请输入生图提示词'); return }
    setImgBusy(true)
    try {
      const t0 = performance.now()
      const body: Record<string, unknown> = { model: imgRoute, prompt, n: imgN }
      if (imgSize) body.size = imgSize
      if (imgRatio) body.ratio = imgRatio
      if (imgImages.length > 0) body.image = imgImages
      const res = await fetch('/v1/images/generations', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${vkKey}` },
        body: JSON.stringify(body),
      })
      if (!res.ok) await throwHttpError(res)
      const parsed = (await res.json()) as ImageResp
      setImgMs(Math.round(performance.now() - t0))
      setImgResult({ data: parsed.data ?? [], usage: parsed.usage })
    } catch (e: unknown) {
      message.error(errText(e))
    } finally {
      setImgBusy(false)
    }
  }

  // 非 chat 家族的配置侧栏：家族路由（按 targets 类型过滤）+ 共享虚拟密钥 + 各自参数
  const configCard = (
    familyLabel: string,
    familyKey: 'embedding' | 'rerank' | 'image',
    routeValue: string | undefined,
    onRoute: (v?: string) => void,
    extra: ReactNode,
  ) => (
    <Card
      title="测试配置"
      size="small"
      style={{ width: 320, flexShrink: 0, overflowY: 'auto' }}
      styles={{ body: { display: 'flex', flexDirection: 'column', gap: 12 } }}
    >
      <div>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>{familyLabel}路由</Typography.Text>
        <RouteSelect routes={routesFor(familyKey)} value={routeValue} onChange={onRoute} familyLabel={familyLabel} />
      </div>
      <div>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>虚拟密钥</Typography.Text>
        <VkSelect vks={activeVks} value={vkId} onChange={setVkId} />
      </div>
      {extra}
    </Card>
  )

  const chatPane = (
    <div style={{ display: 'flex', gap: 16, alignItems: 'stretch', height: 'calc(100vh - 190px)' }}>
      {/* 配置栏 */}
      <Card
        title="测试配置"
        size="small"
        style={{ width: 320, flexShrink: 0, overflowY: 'auto' }}
        styles={{ body: { display: 'flex', flexDirection: 'column', gap: 12 } }}
      >
        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>对话路由</Typography.Text>
          <RouteSelect routes={chatRoutes} value={chatRoute} onChange={setChatRoute} familyLabel="对话" />
        </div>
        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>虚拟密钥</Typography.Text>
          <VkSelect vks={activeVks} value={vkId} onChange={setVkId} />
        </div>
        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>MCP 路由（可选，多选）</Typography.Text>
          <Select
            mode="multiple"
            style={{ width: '100%', marginTop: 4 }}
            placeholder="选择要挂载的 MCP 路由"
            value={mcpRoutes}
            onChange={(v: string[]) => {
              setMcpRoutes(v)
              setTools([])
            }}
            options={mcpRouteOptions.map((r) => ({ value: r.name, label: r.name }))}
            notFoundContent={
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                暂无 mcp 路由，<Link to="/routes">去创建</Link>
              </Typography.Text>
            }
          />
        </div>
        <Button
          icon={<ToolOutlined />}
          onClick={loadTools}
          loading={toolsLoading}
          disabled={!mcpRoutes.length}
        >
          加载 MCP 工具{tools.length ? `（${tools.length}）` : ''}
        </Button>
        {tools.length > 0 && (
          <div>
            {tools.map((t) => (
              <Tooltip key={t.key} title={t.description || t.key}>
                <Tag style={{ marginBottom: 4 }}>{t.key}</Tag>
              </Tooltip>
            ))}
          </div>
        )}
        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>System Prompt</Typography.Text>
          <Input.TextArea
            rows={3}
            style={{ marginTop: 4 }}
            placeholder="可选"
            value={systemPrompt}
            onChange={(e) => setSystemPrompt(e.target.value)}
          />
        </div>
        <Space size="middle">
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>Temperature</Typography.Text>
            <InputNumber
              min={0}
              max={2}
              step={0.1}
              style={{ width: 100, marginTop: 4, display: 'block' }}
              placeholder="默认"
              value={temperature}
              onChange={(v) => setTemperature(v)}
            />
          </div>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>工具轮数上限</Typography.Text>
            <InputNumber
              min={1}
              max={10}
              style={{ width: 100, marginTop: 4, display: 'block' }}
              value={maxRounds}
              onChange={(v) => setMaxRounds(v ?? 5)}
            />
          </div>
        </Space>
      </Card>

      {/* 对话区 */}
      <Card
        title={`对话${chatRoute ? ` · ${chatRoute}` : ''}`}
        size="small"
        style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}
        styles={{ body: { flex: 1, display: 'flex', flexDirection: 'column', minHeight: 0 } }}
        extra={
          <Button size="small" icon={<ClearOutlined />} onClick={clearChat} disabled={!msgs.length}>
            清空
          </Button>
        }
      >
        <div
          ref={listRef}
          style={{ flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: 12, paddingRight: 4 }}
        >
          {msgs.length === 0 && (
            <div style={{ margin: 'auto', textAlign: 'center' }}>
              <Typography.Text type="secondary">
                选择路由与密钥后开始对话；挂载 MCP 路由并加载工具后，模型可自动调用工具。
              </Typography.Text>
            </div>
          )}
          {msgs.map((m, i) => {
            if (m.role === 'user') {
              return (
                <div key={i} style={{ display: 'flex', justifyContent: 'flex-end' }}>
                  <div
                    style={{
                      maxWidth: '75%',
                      background: '#171717',
                      color: '#fff',
                      borderRadius: 8,
                      padding: '8px 12px',
                      whiteSpace: 'pre-wrap',
                      wordBreak: 'break-word',
                    }}
                  >
                    {m.content}
                  </div>
                </div>
              )
            }
            if (m.role === 'tool') return null // 工具结果并入对应 tool_calls 折叠面板展示
            const toolPanels = (m.tool_calls ?? []).map((tc) => {
              const tr = toolResultOf(tc.id)
              return {
                key: tc.id,
                label: (
                  <Space size={8}>
                    <ToolOutlined />
                    <Typography.Text code style={{ fontSize: 12 }}>{tc.function.name}</Typography.Text>
                    {tr && (
                      <Tag color={tr.is_error ? 'red' : 'green'} style={{ fontSize: 11 }}>
                        {tr.is_error ? '失败' : '成功'}
                        {tr.latency_ms ? ` ${tr.latency_ms}ms` : ''}
                      </Tag>
                    )}
                  </Space>
                ),
                children: (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                    <div>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>参数</Typography.Text>
                      <pre style={preStyle}>{prettyJson(tc.function.arguments || '{}')}</pre>
                    </div>
                    {tr && (
                      <div>
                        <Typography.Text type="secondary" style={{ fontSize: 12 }}>结果</Typography.Text>
                        <pre style={{ ...preStyle, color: tr.is_error ? '#cf1322' : undefined }}>{tr.content || '(空)'}</pre>
                      </div>
                    )}
                  </div>
                ),
              }
            })
            const showContent = m.content || m.aborted
            const streaming = sending && i === msgs.length - 1
            return (
              <div key={i} style={{ display: 'flex', justifyContent: 'flex-start' }}>
                <div style={{ maxWidth: '85%', minWidth: 0 }}>
                  {m.reasoning && (
                    <Collapse
                      size="small"
                      style={{ marginBottom: 8, background: '#fff' }}
                      activeKey={sending && i === msgs.length - 1 ? ['r'] : (reasoningKeys[i] ?? [])}
                      onChange={(k) => setReasoningKeys((prev) => ({ ...prev, [i]: k as string[] }))}
                      items={[{
                        key: 'r',
                        label: <Typography.Text type="secondary" style={{ fontSize: 12 }}>思考过程</Typography.Text>,
                        children: (
                          <div style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word', color: '#8f8f8f', fontSize: 12 }}>
                            {m.reasoning}
                          </div>
                        ),
                      }]}
                    />
                  )}
                  {showContent !== undefined && showContent !== '' && (
                    <Bubble
                      variant="outlined"
                      loading={streaming && !m.content && !m.reasoning}
                      content={m.content + (m.aborted ? '\n\n*（已中止）*' : '')}
                      messageRender={(c) => (
                        <Typography>
                          <XMarkdown
                            content={String(c)}
                            streaming={streaming ? { hasNextChunk: true, tail: true, enableAnimation: true } : undefined}
                          />
                        </Typography>
                      )}
                      style={{ opacity: m.aborted ? 0.55 : undefined }}
                    />
                  )}
                  {toolPanels.length > 0 && (
                    <Collapse
                      size="small"
                      style={{ marginTop: showContent ? 8 : 0, background: '#fff' }}
                      items={toolPanels}
                    />
                  )}
                  {(m.usage || m.latency_ms !== undefined) && (
                    <Space size={4} style={{ marginTop: 4 }}>
                      {m.latency_ms !== undefined && <Tag style={{ fontSize: 11 }}>{(m.latency_ms / 1000).toFixed(2)}s</Tag>}
                      {m.usage?.total_tokens !== undefined && (
                        <Tag style={{ fontSize: 11 }}>
                          {m.usage.prompt_tokens ?? '?'} + {m.usage.completion_tokens ?? '?'} = {m.usage.total_tokens} tok
                        </Tag>
                      )}
                    </Space>
                  )}
                </div>
              </div>
            )
          })}
        </div>
        <div style={{ marginTop: 12, display: 'flex', gap: 8, alignItems: 'flex-end' }}>
          <Input.TextArea
            placeholder="输入消息，Enter 发送，Shift+Enter 换行"
            autoSize={{ minRows: 1, maxRows: 6 }}
            value={input}
            disabled={sending}
            onChange={(e) => setInput(e.target.value)}
            onPressEnter={(e) => {
              if (!e.shiftKey) {
                e.preventDefault()
                send()
              }
            }}
            style={{ flex: 1 }}
          />
          {sending ? (
            <Button danger icon={<StopOutlined />} onClick={() => abortRef.current?.abort()}>
              停止
            </Button>
          ) : (
            <Button type="primary" icon={<SendOutlined />} onClick={send} disabled={!input.trim()}>
              发送
            </Button>
          )}
        </div>
      </Card>
    </div>
  )

  const embeddingPane = (
    <div style={{ display: 'flex', gap: 16, alignItems: 'stretch', height: 'calc(100vh - 190px)' }}>
      {configCard('向量', 'embedding', embRoute, setEmbRoute, (
        <>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>输入文本（每行一条）</Typography.Text>
            <Input.TextArea
              rows={6}
              style={{ marginTop: 4 }}
              placeholder={'第一行文本\n第二行文本'}
              value={embText}
              onChange={(e) => setEmbText(e.target.value)}
            />
          </div>
          <Button type="primary" icon={<SendOutlined />} loading={embBusy} disabled={!embText.trim()} onClick={runEmbedding}>
            计算向量
          </Button>
        </>
      ))}
      <Card
        title={`向量结果${embRoute ? ` · ${embRoute}` : ''}`}
        size="small"
        style={{ flex: 1, minWidth: 0, overflowY: 'auto' }}
        styles={{ body: { display: 'flex', flexDirection: 'column', gap: 12 } }}
      >
        {!embResult && (
          <div style={{ margin: 'auto', textAlign: 'center' }}>
            <Typography.Text type="secondary">选择路由与密钥后输入文本；每行一条将分别返回向量。</Typography.Text>
          </div>
        )}
        {embResult && (
          <>
            <Space size={4} wrap>
              {embMs !== null && <Tag style={{ fontSize: 11 }}>{(embMs / 1000).toFixed(2)}s</Tag>}
              {embResult.usage?.total_tokens !== undefined && (
                <Tag style={{ fontSize: 11 }}>{embResult.usage.prompt_tokens ?? '?'} tok</Tag>
              )}
            </Space>
            {(embResult.data ?? []).map((item) => {
              const vec = item.embedding ?? []
              return (
                <div key={item.index} style={{ border: '1px solid #ebebeb', borderRadius: 8, padding: '8px 12px' }}>
                  <Space size={8}>
                    <Tag style={{ fontSize: 11 }}>#{item.index}</Tag>
                    <Tag style={{ fontSize: 11 }}>{vec.length} 维</Tag>
                  </Space>
                  <pre style={preStyle}>{vec.slice(0, 8).map((v) => v.toFixed(6)).join(', ')}{vec.length > 8 ? ', …' : ''}</pre>
                </div>
              )
            })}
          </>
        )}
      </Card>
    </div>
  )

  const rerankPane = (
    <div style={{ display: 'flex', gap: 16, alignItems: 'stretch', height: 'calc(100vh - 190px)' }}>
      {configCard('重排', 'rerank', rrkRoute, setRrkRoute, (
        <>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>Query</Typography.Text>
            <Input
              style={{ marginTop: 4 }}
              placeholder="检索问题"
              value={rrkQuery}
              onChange={(e) => setRrkQuery(e.target.value)}
            />
          </div>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>候选文档（每行一条）</Typography.Text>
            <Input.TextArea
              rows={6}
              style={{ marginTop: 4 }}
              value={rrkDocs}
              onChange={(e) => setRrkDocs(e.target.value)}
            />
          </div>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>Top N（可选）</Typography.Text>
            <InputNumber
              min={1}
              max={100}
              style={{ width: '100%', marginTop: 4 }}
              placeholder="默认全部"
              value={rrkTopN}
              onChange={(v) => setRrkTopN(v)}
            />
          </div>
          <Button
            type="primary"
            icon={<SendOutlined />}
            loading={rrkBusy}
            disabled={!rrkQuery.trim() || !rrkDocs.trim()}
            onClick={runRerank}
          >
            重排
          </Button>
        </>
      ))}
      <Card
        title={`重排结果${rrkRoute ? ` · ${rrkRoute}` : ''}`}
        size="small"
        style={{ flex: 1, minWidth: 0, overflowY: 'auto' }}
        styles={{ body: { display: 'flex', flexDirection: 'column', gap: 12 } }}
      >
        {!rrkResult && (
          <div style={{ margin: 'auto', textAlign: 'center' }}>
            <Typography.Text type="secondary">输入 query 与候选文档，按相关度重排展示。</Typography.Text>
          </div>
        )}
        {rrkResult && (
          <>
            <Space size={4} wrap>
              {rrkMs !== null && <Tag style={{ fontSize: 11 }}>{(rrkMs / 1000).toFixed(2)}s</Tag>}
              <Tag style={{ fontSize: 11 }}>{rrkResult.results?.length ?? 0} 条</Tag>
            </Space>
            {(rrkResult.results ?? []).map((r, i) => (
              <div key={`${r.index}-${i}`} style={{ border: '1px solid #ebebeb', borderRadius: 8, padding: '8px 12px' }}>
                <Space size={8}>
                  <Tag style={{ fontSize: 11 }}>#{r.index}</Tag>
                  <Tag color="blue" style={{ fontSize: 11 }}>score {Number(r.relevance_score).toFixed(4)}</Tag>
                </Space>
                <div style={{ marginTop: 6, fontSize: 13, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                  {rrkDocArr[r.index] ?? ''}
                </div>
              </div>
            ))}
          </>
        )}
      </Card>
    </div>
  )


  // 生成图片独立组件（支持悬浮下载 + 预览放大）
  function GeneratedImage({ src, index }: { src: string; index: number }) {
    const [isHovered, setIsHovered] = useState(false)
    return (
      <div
        style={{ position: 'relative' }}
        onMouseEnter={() => setIsHovered(true)}
        onMouseLeave={() => setIsHovered(false)}
      >
        <Image
          src={src}
          alt={`generated-${index}`}
          style={{ width: '100%', borderRadius: 8, border: '1px solid #ebebeb' }}
        />
        {isHovered && (
          <Button
            size="small"
            icon={<DownloadOutlined />}
            style={{
              position: 'absolute',
              top: 8,
              right: 8,
              opacity: 0.9,
            }}
            onClick={() => {
              const a = document.createElement('a')
              a.href = src
              a.download = `generated-${Date.now()}-${index}.png`
              a.click()
            }}
          >
            下载
          </Button>
        )}
      </div>
    )
  }

  const imagePane = (
    <div style={{ display: 'flex', gap: 16, alignItems: 'stretch', height: 'calc(100vh - 190px)' }}>
      {configCard('生图', 'image', imgRoute, setImgRoute, (
        <>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>提示词</Typography.Text>
            <Input.TextArea
              rows={4}
              style={{ marginTop: 4 }}
              placeholder="描述想生成的图像"
              value={imgPrompt}
              onChange={(e) => setImgPrompt(e.target.value)}
            />
          </div>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>图片分辨率</Typography.Text>
            <div style={{ marginTop: 4 }}>
              <Button.Group style={{ width: '100%' }}>
                {['1K', '2K', '3K', '4K'].map((size) => (
                  <Button
                    key={size}
                    type={imgSize === size ? 'primary' : 'default'}
                    style={{ flex: 1 }}
                    onClick={() => setImgSize(size)}
                  >
                    {size}
                  </Button>
                ))}
              </Button.Group>
            </div>
          </div>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>宽高比</Typography.Text>
            <div style={{ marginTop: 4, display: 'flex', flexDirection: 'column', gap: 4 }}>
              <Button.Group style={{ width: '100%' }}>
                {['1:1', '3:4', '4:3', '16:9'].map((ratio) => (
                  <Button
                    key={ratio}
                    type={imgRatio === ratio ? 'primary' : 'default'}
                    style={{ flex: 1 }}
                    onClick={() => setImgRatio(ratio)}
                  >
                    {ratio}
                  </Button>
                ))}
              </Button.Group>
              <Button.Group style={{ width: '100%' }}>
                {['9:16', '2:3', '3:2', '21:9'].map((ratio) => (
                  <Button
                    key={ratio}
                    type={imgRatio === ratio ? 'primary' : 'default'}
                    style={{ flex: 1 }}
                    onClick={() => setImgRatio(ratio)}
                  >
                    {ratio}
                  </Button>
                ))}
              </Button.Group>
            </div>
          </div>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>参考图(图生图/多图合成)</Typography.Text>
            <Upload
              accept="image/*"
              multiple
              fileList={imgImages.map((url, i) => ({ uid: String(i), name: `image-${i}`, status: 'done' as const, url }))}
              beforeUpload={(file) => {
                const reader = new FileReader()
                reader.onload = (e) => {
                  const dataUrl = e.target?.result as string
                  setImgImages((prev) => [...prev, dataUrl])
                }
                reader.readAsDataURL(file)
                return false
              }}
              onRemove={(file) => {
                const idx = imgImages.findIndex((_, i) => String(i) === file.uid)
                if (idx >= 0) setImgImages((prev) => prev.filter((_, i) => i !== idx))
              }}
              listType="picture-card"
              style={{ marginTop: 4 }}
            >
              {imgImages.length < 5 && <div><Typography.Text type="secondary">+上传</Typography.Text></div>}
            </Upload>
          </div>
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>张数</Typography.Text>
            <InputNumber
              min={1}
              max={4}
              style={{ width: '100%', marginTop: 4 }}
              value={imgN}
              onChange={(v) => setImgN(v ?? 1)}
            />
          </div>
          <Button type="primary" icon={<SendOutlined />} loading={imgBusy} disabled={!imgPrompt.trim()} onClick={runImage}>
            生成图像
          </Button>
        </>
      ))}
      <Card
        title={`生图结果${imgRoute ? ` · ${imgRoute}` : ''}`}
        size="small"
        style={{ flex: 1, minWidth: 0, overflowY: 'auto' }}
        styles={{ body: { display: 'flex', flexDirection: 'column', gap: 12 } }}
      >
        {!imgResult && (
          <div style={{ margin: 'auto', textAlign: 'center' }}>
            <Typography.Text type="secondary">输入提示词生成图像；url 与 b64_json 两种返回均可预览。</Typography.Text>
          </div>
        )}
        {imgResult && (
          <>
            <Space size={4} wrap>
              {imgMs !== null && <Tag style={{ fontSize: 11 }}>{(imgMs / 1000).toFixed(2)}s</Tag>}
              {imgResult.usage?.total_tokens !== undefined && (
                <Tag style={{ fontSize: 11 }}>
                  {imgResult.usage.input_tokens ?? '?'} + {imgResult.usage.output_tokens ?? '?'} = {imgResult.usage.total_tokens} tok
                </Tag>
              )}
            </Space>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))', gap: 12 }}>
              {(imgResult.data ?? []).map((d, i) => {
                const src = d.url ?? (d.b64_json ? `data:image/png;base64,${d.b64_json}` : '')
                return src ? <GeneratedImage key={i} src={src} index={i} /> : null
              })}
            </div>
          </>
        )}
      </Card>
    </div>
  )

  return (
    <Tabs
      activeKey={tab}
      onChange={setTab}
      items={[
        { key: 'chat', label: '对话', children: chatPane },
        { key: 'embedding', label: 'Embedding', children: embeddingPane },
        { key: 'rerank', label: 'Rerank', children: rerankPane },
        { key: 'image', label: '生图', children: imagePane },
      ]}
    />
  )
}

const preStyle: CSSProperties = {
  margin: '4px 0 0',
  padding: 8,
  background: '#f5f5f5',
  borderRadius: 6,
  fontSize: 12,
  maxHeight: 240,
  overflow: 'auto',
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-word',
}
