import { useEffect, useState } from 'react'
import { Button, DatePicker, Select, Space, Table, message } from 'antd'
import dayjs, { Dayjs } from 'dayjs'
import { useNavigate } from 'react-router-dom'
import { api } from '../api'
import StatusTag from '../components/StatusTag'

interface Log {
  id: number
  request_id: string
  route: string
  model: string
  provider: string
  key_id: number
  key_value_masked?: string
  key_name?: string
  status: string
  error_code: string
  error_body?: string
  prompt_tokens: number
  completion_tokens: number
  cached_tokens: number
  tokens_estimated: boolean
  ttft_ms: number
  total_ms: number
  cost: number
  retries: number
  created_at: number
}

const statusTag = (s: string) => {
  if (s === 'success') return <StatusTag tone="ok">成功</StatusTag>
  if (s === 'pending') return <StatusTag tone="processing">转发中</StatusTag>
  if (s === 'client_error') return <StatusTag tone="mute">客户端错误</StatusTag>
  if (s === 'cooldown') return <StatusTag tone="warn">冷却</StatusTag>
  return <StatusTag tone="error">错误</StatusTag>
}

export default function Logs() {
  const nav = useNavigate()
  const [items, setItems] = useState<Log[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [routes, setRoutes] = useState<{ id: number; name: string }[]>([])
  const [filterRoute, setFilterRoute] = useState<string | undefined>()
  const [filterStatus, setFilterStatus] = useState<string | undefined>()
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
    if (filterStatus) q.set('status', filterStatus)
    try {
      const res = await api<{ total: number; items: Log[] }>('GET', `/api/logs?${q.toString()}`)
      setItems(res.items)
      setTotal(res.total)
    } catch (e: any) {
      message.error(e.message)
    }
  }

  useEffect(() => { load(1) }, [filterRoute, filterStatus, range])
  useEffect(() => { api<{ id: number; name: string }[]>('GET', '/api/routes').then(setRoutes).catch(() => {}) }, [])

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Select
          allowClear placeholder="路由" style={{ width: 180 }}
          value={filterRoute} onChange={setFilterRoute}
          options={routes.map((r) => ({ value: r.name, label: r.name }))}
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
      </Space>
      <Table<Log>
        rowKey="id"
        dataSource={items}
        size="small"
        tableLayout="fixed"
        scroll={{ x: 1480 }}
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
        <Table.Column title="费用" dataIndex="cost" width={90} render={(v) => v.toFixed(5)} />
        <Table.Column title="重试" dataIndex="retries" width={60} />
      </Table>
    </div>
  )
}
