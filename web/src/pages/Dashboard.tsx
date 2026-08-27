import { useEffect, useState } from 'react'
import { Card, Col, Empty, Row, Statistic, Table, Tooltip, Button, message } from 'antd'
import dayjs from 'dayjs'
import { api } from '../api'
import Chart from '../components/Chart'
import StatusTag from '../components/StatusTag'
import { formatNumber, formatCost } from '../utils/format'

interface Overview {
  total: number
  success: number
  errors: number
  success_rate: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
}

interface HealthModel {
  id: number
  name: string
  status: string
  fail_count: number
  cooldown_until: number
  disable_reason: string
  last_error: string
}

interface TopModelRow {
  dim: string
  total: number
  success: number
  prompt_tokens: number
  completion_tokens: number
  cost: number
}

const modelStatusTag = (m: HealthModel) => {
  if (m.status === 'active') return <StatusTag tone="ok">正常</StatusTag>
  if (m.status === 'cooldown') {
    const remain = m.cooldown_until - Math.floor(Date.now() / 1000)
    return <StatusTag tone="warn">限流冷却{remain > 0 ? ` ${remain}s` : ''}</StatusTag>
  }
  return <Tooltip title={m.disable_reason}><StatusTag tone="error">不可用</StatusTag></Tooltip>
}

// 水平柱图：按总 Tokens 倒序自上而下，每条标注调用次数与总 Tokens
function topChartOption(rows: TopModelRow[]) {
  const reversed = [...rows].reverse()
  const yData = reversed.map((r) => r.dim)
  const sData = reversed.map((r) => ({
    value: r.prompt_tokens + r.completion_tokens,
    modelName: r.dim,
    count: r.total,
    tokens: r.prompt_tokens + r.completion_tokens,
  }))
  return {
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'shadow' },
      formatter: (params: any) => {
        const p = params[0]
        return `${p.data.modelName}<br/>调用次数：${p.data.count}<br/>总 Tokens：${p.data.tokens}`
      },
    },
    grid: { left: 120, right: 100, top: 16, bottom: 20 },
    xAxis: { type: 'value', name: '总 Tokens' },
    yAxis: { type: 'category', data: yData, axisTick: { show: false } },
    series: [{
      type: 'bar',
      data: sData,
      itemStyle: { color: '#171717' },
      label: {
        show: true,
        position: 'right',
        formatter: (p: any) => `${p.data.count}次 / ${p.data.tokens}tok`,
        color: '#4d4d4d',
      },
    }],
  }
}

type Granularity = '1m' | '15m' | '1h'
const GRANULARITIES: { key: Granularity; label: string; points: number }[] = [
  { key: '1m', label: '1 分钟', points: 24 * 60 },
  { key: '15m', label: '15 分钟', points: 24 * 4 },
  { key: '1h', label: '1 小时', points: 24 },
]

export default function Dashboard() {
  const [ov, setOv] = useState<Overview | null>(null)
  const [series, setSeries] = useState<any[]>([])
  const [models, setModels] = useState<HealthModel[]>([])
  const [topModels, setTopModels] = useState<TopModelRow[]>([])
  const [now, setNow] = useState(Math.floor(Date.now() / 1000))
  const [granularity, setGranularity] = useState<Granularity>('1m')

  const load = async () => {
    const startOfDay = dayjs().startOf('day').unix()
    const endOfDay = dayjs().endOf('day').unix()
    try {
      const [o, ts, h, top] = await Promise.all([
        api('GET', `/api/stats/overview?from=${startOfDay}&to=${endOfDay}`),
        api('GET', `/api/stats/timeseries?from=${startOfDay}&to=${endOfDay}&bucket=${granularity}`),
        api('GET', '/api/health'),
        api('GET', `/api/stats/breakdown?dim=model&from=${startOfDay}&to=${endOfDay}`),
      ])
      setOv(o)
      setSeries(ts.points ?? [])
      setModels(h.models ?? [])
      setNow(h.now ?? Math.floor(Date.now() / 1000))
      const sorted = [...(top ?? [])].sort(
        (a, b) => (b.prompt_tokens + b.completion_tokens) - (a.prompt_tokens + a.completion_tokens),
      ).slice(0, 10)
      setTopModels(sorted)
    } catch (e: any) {
      message.error(e.message)
    }
  }
  useEffect(() => { load() }, [granularity])
  useEffect(() => {
    const t = setInterval(load, 10000)
    return () => clearInterval(t)
  }, [granularity])

  const chartOption = {
    tooltip: { trigger: 'axis' },
    legend: { data: ['请求数', '平均首字响应(ms)'] },
    grid: { left: 50, right: 50, bottom: 30 },
    xAxis: { type: 'category', data: series.map((p) => dayjs(p.bucket * 1000).format('HH:mm')), boundaryGap: false },
    yAxis: [
      { type: 'value', name: '请求数' },
      { type: 'value', name: '首字响应(ms)' },
    ],
    series: [
      { name: '请求数', type: 'bar', data: series.map((p) => p.total) },
      { name: '平均首字响应(ms)', type: 'line', yAxisIndex: 1, smooth: true, data: series.map((p) => Math.round(p.avg_ttft_ms)) },
    ],
  }

  return (
    <div>
      <Row gutter={[16, 16]}>
        <Col flex="1 1 0">
          <div style={{ background: '#eaf4ff', padding: 20, borderRadius: 12, minHeight: 96 }}>
            <Statistic
              title="总调用次数"
              value={ov?.total ?? 0}
              formatter={(v) => (
                <span style={{ display: 'inline-flex', alignItems: 'baseline', gap: 12 }}>
                  <span>{formatNumber(+v)}</span>
                  <span style={{ fontSize: 12, color: '#4d4d4d', fontWeight: 400 }}>
                    成功: {formatNumber(ov?.success ?? 0)} 次
                  </span>
                </span>
              )}
              valueStyle={{ color: '#171717' }}
            />
          </div>
        </Col>
        <Col flex="1 1 0">
          <div style={{ background: '#f0fbe8', padding: 20, borderRadius: 12, minHeight: 96 }}>
            <Statistic
              title="费用"
              value={ov?.cost ?? 0}
              formatter={(v) => formatCost(+v)}
              valueStyle={{ color: '#171717' }}
            />
          </div>
        </Col>
        <Col flex="1 1 0">
          <div style={{ background: '#f4f0ff', padding: 20, borderRadius: 12, minHeight: 96 }}>
            <Statistic
              title="总 Tokens"
              value={ov?.total_tokens ?? 0}
              formatter={(v) => formatNumber(+v)}
              valueStyle={{ color: '#171717' }}
            />
          </div>
        </Col>
        <Col flex="1 1 0">
          <div style={{ background: '#e8fbf4', padding: 20, borderRadius: 12, minHeight: 96 }}>
            <Statistic
              title="输入 Tokens"
              value={ov?.prompt_tokens ?? 0}
              formatter={(v) => formatNumber(+v)}
              valueStyle={{ color: '#171717' }}
            />
          </div>
        </Col>
        <Col flex="1 1 0">
          <div style={{ background: '#fff4e6', padding: 20, borderRadius: 12, minHeight: 96 }}>
            <Statistic
              title="输出 Tokens"
              value={ov?.completion_tokens ?? 0}
              formatter={(v) => formatNumber(+v)}
              valueStyle={{ color: '#171717' }}
            />
          </div>
        </Col>
      </Row>
      <Card
        title="流量趋势（00:00 - 23:59）"
        extra={
          <div onClick={(e) => e.stopPropagation()}>
            {GRANULARITIES.map((g) => (
              <Button
                key={g.key}
                size="small"
                type={granularity === g.key ? 'primary' : 'default'}
                onClick={() => setGranularity(g.key)}
                style={{ marginLeft: 8 }}
              >
                {g.label}
              </Button>
            ))}
          </div>
        }
        style={{ marginTop: 16 }}
      >
        {series.length === 0 ? (
          <Empty description="所选时间粒度下今日暂无调用" image={Empty.PRESENTED_IMAGE_SIMPLE} />
        ) : (
          <Chart option={chartOption} height={280} />
        )}
      </Card>
      <Card title="模型调用详情" style={{ marginTop: 16 }}>
        {topModels.length === 0 ? (
          <Empty description="今日暂无调用" image={Empty.PRESENTED_IMAGE_SIMPLE} />
        ) : (
          <Table<TopModelRow>
            rowKey="dim"
            dataSource={topModels}
            size="small"
            pagination={false}
            tableLayout="fixed"
            rowClassName={() => 'log-row-clean'}
          >
            <Table.Column title="模型" dataIndex="dim" width={220} ellipsis />
            <Table.Column title="请求数" dataIndex="total" width={90} sorter={(a, b) => a.total - b.total} render={(v) => formatNumber(+v)} />
            <Table.Column title="平均 Tokens" width={120} sorter={(a, b) => ((b.prompt_tokens + b.completion_tokens) / Math.max(b.total, 1)) - ((a.prompt_tokens + a.completion_tokens) / Math.max(a.total, 1))} render={(_, r) => formatNumber(Math.round((r.prompt_tokens + r.completion_tokens) / Math.max(r.total, 1)))} />
            <Table.Column title="总 Tokens" width={120} sorter={(a, b) => (b.prompt_tokens + b.completion_tokens) - (a.prompt_tokens + a.completion_tokens)} render={(_, r) => formatNumber(r.prompt_tokens + r.completion_tokens)} />
            <Table.Column title="输入 Tokens" dataIndex="prompt_tokens" width={110} sorter={(a, b) => a.prompt_tokens - b.prompt_tokens} render={(v) => formatNumber(+v)} />
            <Table.Column title="输出 Tokens" dataIndex="completion_tokens" width={110} sorter={(a, b) => a.completion_tokens - b.completion_tokens} render={(v) => formatNumber(+v)} />
            <Table.Column title="费用" dataIndex="cost" width={100} sorter={(a, b) => a.cost - b.cost} render={(v) => formatCost(+v)} />
          </Table>
        )}
      </Card>
      <Card title="模型健康（按真实可达性：所有 key 限流时不算正常）" style={{ marginTop: 16 }}>
        {models.length === 0 ? (
          <Empty description="暂无模型" image={Empty.PRESENTED_IMAGE_SIMPLE} />
        ) : (
          <Table<HealthModel> rowKey="id" dataSource={models} pagination={false} size="small" tableLayout="fixed">
            <Table.Column title="ID" dataIndex="id" width={60} />
            <Table.Column title="模型" dataIndex="name" width={160} />
            <Table.Column title="状态" width={100} render={(_, m) => modelStatusTag(m)} />
            <Table.Column title="连续失败" dataIndex="fail_count" width={90} />
            <Table.Column
              title="冷却/禁用详情"
              render={(_, m: HealthModel) => {
                if (m.status === 'cooldown') {
                  const remain = m.cooldown_until - now
                  return remain > 0 ? <span>{remain}s 后可探测</span> : <span>冷却已到期（待请求触发）</span>
                }
                if (m.status === 'no_key' || m.status === 'error') {
                  return <Tooltip title={m.last_error}><span style={{ color: '#cf1322' }}>{m.disable_reason || m.last_error || '-'}</span></Tooltip>
                }
                if (m.status === 'disabled') {
                  return <span style={{ color: '#cf1322' }}>{m.disable_reason || '已禁用'}</span>
                }
                return <span style={{ color: '#8f8f8f' }}>-</span>
              }}
            />
            <Table.Column
              title="操作"
              width={120}
              render={(_, m) => m.status === 'disabled' ? (
                <Button size="small" type="primary" danger ghost onClick={() => enable(m.id)}>手动解禁</Button>
              ) : <span style={{ color: '#8f8f8f' }}>-</span>}
            />
          </Table>
        )}
      </Card>
    </div>
  )
}

function enable(id: number) {
  api('POST', `/api/models/${id}/enable`).then(() => message.success('已重新启用')).catch((e) => message.error(e.message))
}
