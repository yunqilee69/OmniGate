import { useEffect, useState } from 'react'
import { Card, Col, DatePicker, Radio, Row, Statistic, Table, message } from 'antd'
import dayjs, { Dayjs } from 'dayjs'
import { api } from '../api'
import Chart from '../components/Chart'
import { CurrencyToggle, useCurrency } from '../components/CurrencyToggle'
import { formatNumber, formatCost } from '../utils/format'

interface Item {
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

interface Overview {
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
}

const dims = [
  { value: 'route', label: '路由' },
  { value: 'model', label: '模型' },
  { value: 'provider', label: '提供商' },
  { value: 'key', label: '密钥' },
  { value: 'status', label: '状态' },
  { value: 'error_code', label: '错误码' },
]

export default function Stats() {
  const [dim, setDim] = useState('route')
  const [range, setRange] = useState<[Dayjs, Dayjs]>([dayjs().subtract(6, 'day').startOf('day'), dayjs()])
  const [items, setItems] = useState<Item[]>([])
  const [series, setSeries] = useState<any[]>([])
  const [ov, setOv] = useState<Overview | null>(null)
  const [currency, setCurrency] = useCurrency()

  const load = async () => {
    const from = range[0].unix()
    const to = range[1].unix()
    const cur = `&currency=${currency}`
    try {
      const [o, bd, ts] = await Promise.all([
        api('GET', `/api/stats/overview?from=${from}&to=${to}${cur}`),
        api('GET', `/api/stats/breakdown?dim=${dim}&from=${from}&to=${to}${cur}`),
        api('GET', `/api/stats/timeseries?from=${from}&to=${to}&bucket=1d${cur}`),
      ])
      setOv(o)
      setItems(bd)
      setSeries(ts.points ?? [])
    } catch (e: any) {
      message.error(e.message)
    }
  }
  useEffect(() => { load() }, [dim, range, currency])

  const chartOption = {
    tooltip: { trigger: 'axis' },
    legend: { data: ['请求数', '成功数'] },
    grid: { left: 50, right: 50, bottom: 30 },
    xAxis: { type: 'category', data: series.map((p) => dayjs(p.bucket * 1000).format('MM-DD')) },
    yAxis: { type: 'value' },
    series: [
      { name: '请求数', type: 'bar', data: series.map((p) => p.total) },
      { name: '成功数', type: 'line', data: series.map((p) => p.success) },
    ],
  }

  return (
    <div>
      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
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
              formatter={(v) => formatCost(+v, currency)}
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
      <Card style={{ marginBottom: 16 }}>
        <Radio.Group options={dims} value={dim} onChange={(e) => setDim(e.target.value)} optionType="button" />
        <DatePicker.RangePicker
          style={{ marginLeft: 16 }}
          value={range}
          onChange={(v) => v?.[0] && v[1] && setRange([v[0], v[1]])}
        />
        <span style={{ float: 'right' }}>
          <CurrencyToggle value={currency} onChange={setCurrency} />
        </span>
      </Card>
      <Card title="每日请求趋势" style={{ marginBottom: 16 }}>
        <Chart option={chartOption} />
      </Card>
      <Card title="维度明细">
        <Table<Item> rowKey="dim" dataSource={items} size="small" pagination={{ pageSize: 20 }}>
            <Table.Column title="维度值" dataIndex="dim" render={(_, it: Item) => {
              if (dim !== 'key') return it.dim
              const label = it.key_name || it.key_masked
              return label ? <code style={{ fontSize: 12 }}>{label}</code> : `key#${it.dim}`
            }} />
          <Table.Column title="请求" dataIndex="total" sorter={(a: Item, b: Item) => a.total - b.total} />
          <Table.Column title="成功" dataIndex="success" />
          <Table.Column title="错误" dataIndex="errors" />
          <Table.Column title="错误率" render={(_, it: Item) =>
            it.total > 0 ? `${((1 - it.success / it.total) * 100).toFixed(1)}%` : '-'
          } />
          <Table.Column title="Prompt" dataIndex="prompt_tokens" />
          <Table.Column title="Completion" dataIndex="completion_tokens" />
          <Table.Column title="费用" dataIndex="cost" render={(v) => formatCost(+v, currency)} />
          <Table.Column title="平均首字响应(ms)" dataIndex="avg_ttft_ms" render={(v) => Math.round(v)} />
          <Table.Column title="平均耗时(ms)" dataIndex="avg_total_ms" render={(v) => Math.round(v)} />
          <Table.Column title="平均重试" dataIndex="avg_retries" render={(v) => v.toFixed(2)} />
        </Table>
      </Card>
    </div>
  )
}
