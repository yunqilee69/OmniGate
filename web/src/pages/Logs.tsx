import { useEffect, useState } from 'react'
import { Button, DatePicker, Modal, Select, Space, Table, message } from 'antd'
import dayjs, { Dayjs } from 'dayjs'
import { useNavigate } from 'react-router-dom'
import { api } from '../api'
import { formatCounts } from '../utils/format'
import StatusTag from '../components/StatusTag'

interface Log {
  id: number
  request_id: string
  route: string
  endpoint: string
  provider: string
  key_id: number
  key_value_masked?: string
  key_name?: string
  vk_id?: number
  vk_name?: string
  status: string
  error_code: string
  error_body?: string
  prompt_tokens: number
  completion_tokens: number
  cached_tokens: number
  tokens_estimated: boolean
  ttft_ms: number
  total_ms: number
  tps: number
  cost: number
  retries: number
  created_at: number
}
type Provider = { id: number; name: string }
type VirtualKeyOpt = { id: number; name: string }

const statusTag = (s: string) => {
  if (s === 'success') return <StatusTag tone="ok">成功</StatusTag>
  if (s === 'pending') return <StatusTag tone="processing">转发中</StatusTag>
  if (s === 'client_error') return <StatusTag tone="mute">客户端错误</StatusTag>
  if (s === 'cooldown') return <StatusTag tone="warn">冷却</StatusTag>
  return <StatusTag tone="error">错误</StatusTag>
}

const endpointLabel = (v?: string) => ({
  completions: '对话',
  messages: '对话 (Anthropic)',
  responses: 'Responses',
  embedding: '向量化',
  rerank: '重排',
  image: '生图',
  mcp: 'MCP',
}[v ?? ''] ?? v)

export default function Logs() {
  const nav = useNavigate()
  const [items, setItems] = useState<Log[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [routes, setRoutes] = useState<{ id: number; name: string; endpoint?: string }[]>([])
  const [providers, setProviders] = useState<string[]>([])
  const [filterRoute, setFilterRoute] = useState<string | undefined>()
  const [filterProvider, setFilterProvider] = useState<string | undefined>()
  const [filterStatus, setFilterStatus] = useState<string | undefined>()
  const [filterEndpoint, setFilterEndpoint] = useState<string | undefined>()
  const [filterVK, setFilterVK] = useState<number | undefined>()
  const [virtualKeys, setVirtualKeys] = useState<VirtualKeyOpt[]>([])
  // null = 未手动圈定范围，默认"最近 7 天（含今天）"；每次 load 动态计算，保证刷新能看到最新日志
  const [range, setRange] = useState<[Dayjs, Dayjs] | null>(null)

  const load = async (p = page) => {
    const [start, end] = range ?? [dayjs().subtract(6, 'day'), dayjs()]
    const q = new URLSearchParams({
      // 按天粒度过滤：起止分别对齐到当日 00:00:00 / 23:59:59
      from: String(start.startOf('day').unix()),
      to: String(end.endOf('day').unix()),
      page: String(p),
      size: '50',
    })
    if (filterRoute) q.set('route', filterRoute)
    if (filterProvider) q.set('provider', filterProvider)
    if (filterStatus) q.set('status', filterStatus)
    if (filterEndpoint) q.set('endpoint', filterEndpoint)
    if (filterVK) q.set('vk_id', String(filterVK))
    try {
      const res = await api<{ total: number; items: Log[] }>('GET', `/api/logs?${q.toString()}`)
      setItems(res.items)
      setTotal(res.total)
    } catch (e) {
      message.error(e instanceof Error ? e.message : String(e))
    }
  }
  useEffect(() => { load(1) }, [filterRoute, filterProvider, filterStatus, filterEndpoint, filterVK, range])
  useEffect(() => {
    api<{ id: number; name: string; endpoint?: string }[]>('GET', '/api/routes').then(setRoutes).catch(() => {})
    api<Provider[]>('GET', '/api/providers').then((ps) => {
      setProviders(ps.map((p) => p.name).sort())
    }).catch(() => {})
    api<VirtualKeyOpt[]>('GET', '/api/virtual-keys').then((ks) => {
      setVirtualKeys(ks.map((k) => ({ id: k.id, name: k.name })).sort((a, b) => a.name.localeCompare(b.name)))
    }).catch(() => {})
  }, [])

  const confirmClearLogs = () => {
    Modal.confirm({
      title: '确认清空全部请求日志与内容记录？',
      content: '将删除全部请求日志明细、尝试日志与内容捕获记录（请求/响应正文），每日统计数据保留。此操作不可恢复。',
      okText: '清空日志',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        try {
          const r = await api<{ cleared: Record<string, number> }>('POST', '/api/maintenance/clear-logs', { confirm: true })
          message.success(`已清空：${formatCounts(r.cleared)}`)
          setPage(1)
          await load(1)
        } catch (e) {
          message.error(e instanceof Error ? e.message : String(e))
        }
      },
    })
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Select
          allowClear placeholder="路由" style={{ width: 180 }}
          value={filterRoute} onChange={setFilterRoute}
          options={routes.map((r) => ({ value: r.name, label: r.name }))}
        />
        <Select
          allowClear placeholder="提供商" style={{ width: 140 }}
          value={filterProvider} onChange={setFilterProvider}
          options={providers.map((p) => ({ value: p, label: p }))}
        />
        <Select
          allowClear placeholder="路由类型" style={{ width: 140 }}
          value={filterEndpoint} onChange={setFilterEndpoint}
          options={[...new Set(routes.map((r) => r.endpoint).filter(Boolean))].map((v) => ({
            value: v, label: endpointLabel(v),
          }))}
        />
        <Select
          allowClear placeholder="虚拟密钥" style={{ width: 180 }}
          value={filterVK} onChange={setFilterVK}
          showSearch optionFilterProp="label"
          options={virtualKeys.map((k) => ({ value: k.id, label: k.name }))}
        />
        <Select
          allowClear placeholder="状态" style={{ width: 140 }}
          value={filterStatus} onChange={setFilterStatus}
          options={[
            { value: 'success', label: '成功' },
            { value: 'error', label: '错误' },
            { value: 'client_error', label: '客户端错误' },
            { value: 'pending', label: '转发中' },
          ]}
        />
        <DatePicker.RangePicker
          value={range ?? [dayjs().subtract(6, 'day'), dayjs()]}
          onChange={(v) => setRange(v?.[0] && v[1] ? [v[0], v[1]] : null)}
        />
        <Button onClick={() => load()}>刷新</Button>
        <Button danger onClick={confirmClearLogs}>清空日志</Button>
      </Space>
      <Table<Log>
        rowKey="id"
        dataSource={items}
        size="small"
        tableLayout="fixed"
        scroll={{ x: 1750 }}
        onRow={(log) => ({ onClick: () => nav(`/logs/${log.request_id}`), style: { cursor: 'pointer' } })}
        rowClassName={(l) => l.status === 'error' ? 'log-row-error' : ''}
        pagination={{
          current: page, total, pageSize: 50,
          onChange: (p) => { setPage(p); load(p) },
          showTotal: (t) => `共 ${t} 条`,
        }}
      >
        <Table.Column title="时间" dataIndex="created_at" width={150} fixed="left"
          render={(v) => dayjs(v * 1000).format('MM-DD HH:mm:ss')} />
        <Table.Column title="路由" dataIndex="route" width={110}
          render={(v) => <code>{v}</code>} />
        <Table.Column title="路由类型" dataIndex="endpoint" width={110}
          render={(v) => endpointLabel(v)} />
        <Table.Column title="虚拟密钥" width={140} ellipsis
          render={(_, l: Log) => l.vk_name || (l.vk_id ? `#${l.vk_id}` : '-')} />
        <Table.Column title="提供商/模型" dataIndex="model" width={200} ellipsis
          render={(_, l) => l.provider ? `${l.provider}/${l.model}` : l.model} />
        <Table.Column title="密钥" width={180} render={(_, l: Log) => (
          <code style={{ fontSize: 11, color: '#8f8f8f' }}>{l.key_name || l.key_value_masked || (l.key_id ? `#${l.key_id}` : '-')}</code>
        )} />
        <Table.Column title="状态" dataIndex="status" width={90} render={(_, l) => statusTag(l.status)} />
        <Table.Column title="错误码" dataIndex="error_code" width={90} />
        <Table.Column title="Tokens" width={88} render={(_, l: Log) => (
          <div style={{ lineHeight: '18px', color: '#666' }}>
            <div>↑ {l.prompt_tokens}{l.tokens_estimated ? '~' : ''}</div>
            <div>↓ {l.completion_tokens}{l.tokens_estimated ? '~' : ''}</div>
          </div>
        )} />
        <Table.Column title="缓存率" width={80} render={(_, l: Log) => {
          if (l.prompt_tokens === 0) return '-'
          const rate = (l.cached_tokens / l.prompt_tokens * 100).toFixed(1)
          return `${rate}%`
        }} />
        <Table.Column title="首字/总耗时" width={120} render={(_, l: Log) => (
          <div style={{ lineHeight: '18px', color: '#666' }}>
            <div>{l.ttft_ms}ms</div>
            <div style={{ color: '#8f8f8f' }}>{l.total_ms}ms</div>
          </div>
        )} />
        <Table.Column title="速度" width={86} render={(_, l: Log) => (
          l.tps > 0 ? <code>{l.tps.toFixed(1)} tok/s</code> : <span style={{ color: '#8f8f8f' }}>-</span>
        )} />
        <Table.Column title="费用" dataIndex="cost" width={90} render={(v) => v.toFixed(5)} />
        <Table.Column title="重试" dataIndex="retries" width={60} />
      </Table>
    </div>
  )
}
