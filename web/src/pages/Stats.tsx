import { useEffect, useMemo, useState } from 'react'
import { Card, Col, DatePicker, Empty, Radio, Row, Select, Space, Statistic, Table, message } from 'antd'
import dayjs, { Dayjs } from 'dayjs'
import { api } from '../api'
import Chart from '../components/Chart'
import { CurrencyToggle, useCurrency } from '../components/CurrencyToggle'
import { formatNumber, formatCost } from '../utils/format'
import {
  type BucketSize, type FixtureRequest, type Item, type Overview, makeStatsFixture,
  type StatsDimension, type TimeseriesPoint, type TimeseriesResponse,
} from '../fixtures/statsFixtures'
import {
  costTokenOption, distributionOption, fallbackOption, latencyOption, rankingOption,
  requestTrendOption,
} from '../helpers/statsChartOptions'

const dimensions: { value: StatsDimension; label: string }[] = [
  { value: 'route', label: '路由' },
  { value: 'model', label: '模型' },
  { value: 'provider', label: '提供商' },
  { value: 'key', label: '密钥' },
  { value: 'status', label: '状态' },
  { value: 'error_code', label: '错误码' },
]

const bucketOptions: { value: BucketSize; label: string }[] = [
  { value: '1h', label: '1 小时' },
  { value: '1d', label: '1 天' },
  { value: '1w', label: '1 周' },
]

const emptyTimeseries: TimeseriesResponse = { bucket_s: 86400, points: [] }

function queryBucket(bucket: BucketSize): string {
  return bucket === '1d' ? '24h' : bucket === '1w' ? '168h' : '1h'
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : '统计数据加载失败'
}

function StatTile({ title, value, tint, suffix }: { title: string; value: string; tint: string; suffix?: string }) {
  return (
    <div style={{ background: tint, padding: 16, borderRadius: 12, minHeight: 82 }}>
      <Statistic title={title} value={value} suffix={suffix} valueStyle={{ color: '#171717', fontSize: 22 }} />
    </div>
  )
}

function ChartOrEmpty({ option, points, height = 300 }: { option: object; points: TimeseriesPoint[]; height?: number }) {
  return points.length ? <Chart option={option} height={height} /> : <Empty description="暂无时间序列数据" image={Empty.PRESENTED_IMAGE_SIMPLE} />
}

function chartData(points: TimeseriesPoint[], bucket: BucketSize): TimeseriesPoint[] {
  if (points.length < 2 || bucket === '1w') return points
  const step = bucket === '1h' ? 3600 : 86400
  const start = points[0]?.bucket ?? 0
  const end = points[points.length - 1]?.bucket ?? start
  const pointMap = new Map(points.map((point) => [point.bucket, point]))
  return Array.from({ length: Math.floor((end - start) / step) + 1 }, (_, index) => {
    return pointMap.get(start + index * step) ?? {
      bucket: start + index * step, total: 0, success: 0, errors: 0,
      prompt_tokens: 0, completion_tokens: 0, cost: 0, total_tokens: 0,
      fallback_count: 0, avg_ttft_ms: 0, avg_total_ms: 0,
    }
  })
}

export default function Stats() {
  const [dim, setDim] = useState<StatsDimension>('route')
  const [bucket, setBucket] = useState<BucketSize>('1d')
  const [range, setRange] = useState<[Dayjs, Dayjs]>([dayjs().subtract(6, 'day').startOf('day'), dayjs()])
  const [items, setItems] = useState<Item[]>([])
  const [statusItems, setStatusItems] = useState<Item[]>([])
  const [errorItems, setErrorItems] = useState<Item[]>([])
  const [filterItems, setFilterItems] = useState<Record<'route' | 'model' | 'provider', Item[]>>({ route: [], model: [], provider: [] })
  const [series, setSeries] = useState<TimeseriesResponse>(emptyTimeseries)
  const [ov, setOv] = useState<Overview | null>(null)
  const [currency, setCurrency] = useCurrency()
  const [route, setRoute] = useState<string>()
  const [model, setModel] = useState<string>()
  const [provider, setProvider] = useState<string>()
  const fixture = new URLSearchParams(window.location.search).get('fixture') === '1'

  useEffect(() => {
    const request: FixtureRequest = {
      from: range[0].unix(), to: range[1].unix(), bucket, currency, dim, route, model, provider,
    }
    if (fixture) {
      const fixturePayload = makeStatsFixture(request)
      setOv(fixturePayload.overview)
      setItems(fixturePayload.breakdown)
      setSeries(fixturePayload.timeseries)
      setStatusItems(makeStatsFixture({ ...request, dim: 'status' }).breakdown)
      setErrorItems(makeStatsFixture({ ...request, dim: 'error_code' }).breakdown)
      setFilterItems({
        route: makeStatsFixture({ ...request, dim: 'route' }).breakdown,
        model: makeStatsFixture({ ...request, dim: 'model' }).breakdown,
        provider: makeStatsFixture({ ...request, dim: 'provider' }).breakdown,
      })
      return
    }

    const from = range[0].unix()
    const to = range[1].unix()
    const base = `from=${from}&to=${to}&currency=${currency}`
    const timeseriesQuery = new URLSearchParams({ from: String(from), to: String(to), bucket: queryBucket(bucket), currency })
    if (route) timeseriesQuery.set('route', route)
    if (model) timeseriesQuery.set('model', model)
    if (provider) timeseriesQuery.set('provider', provider)
    Promise.all([
      api<Overview>('GET', `/api/stats/overview?${base}`),
      api<Item[]>('GET', `/api/stats/breakdown?dim=${dim}&${base}`),
      api<TimeseriesResponse>('GET', `/api/stats/timeseries?${timeseriesQuery.toString()}`),
      api<Item[]>('GET', `/api/stats/breakdown?dim=status&${base}`),
      api<Item[]>('GET', `/api/stats/breakdown?dim=error_code&${base}`),
      api<Item[]>('GET', `/api/stats/breakdown?dim=route&${base}`),
      api<Item[]>('GET', `/api/stats/breakdown?dim=model&${base}`),
      api<Item[]>('GET', `/api/stats/breakdown?dim=provider&${base}`),
    ]).then(([overview, breakdown, timeseries, statuses, errors, routes, models, providers]) => {
      setOv(overview)
      setItems(breakdown)
      setSeries(timeseries)
      setStatusItems(statuses)
      setErrorItems(errors)
      setFilterItems({ route: routes, model: models, provider: providers })
    }).catch((error: unknown) => message.error(errorMessage(error)))
  }, [bucket, currency, dim, fixture, model, provider, range, route])

  const points = useMemo(() => chartData(series.points, bucket), [bucket, series.points])
  const statusDistribution = statusItems
    .filter((item) => item.total > 0)
    .map((item) => ({ name: item.dim === 'success' ? '成功' : item.dim === 'error' ? '错误' : item.dim, value: item.total }))
  const errorDistribution = errorItems.filter((item) => item.total > 0).slice(0, 8).map((item) => ({ name: item.dim, value: item.total }))
  const filterOptions = (key: 'route' | 'model' | 'provider') => filterItems[key].map((item) => ({ value: item.dim, label: item.dim }))
  const rankingLabel = dimensions.find((entry) => entry.value === dim)?.label ?? '维度'

  return (
    <div style={{ minWidth: 0 }}>
      <Row gutter={[12, 12]} style={{ marginBottom: 16 }}>
        <Col xs={24} sm={12} md={8} lg={6}><StatTile title="总调用次数" value={formatNumber(ov?.total ?? 0)} tint="#eaf4ff" suffix={ov ? `成功率 ${(ov.success_rate * 100).toFixed(1)}%` : undefined} /></Col>
        <Col xs={24} sm={12} md={8} lg={6}><StatTile title="费用" value={formatCost(ov?.cost ?? 0, currency)} tint="#f0fbe8" /></Col>
        <Col xs={24} sm={12} md={8} lg={6}><StatTile title="总 Tokens" value={formatNumber(ov?.total_tokens ?? 0)} tint="#f4f0ff" /></Col>
        <Col xs={24} sm={12} md={8} lg={6}><StatTile title="输入 / 输出" value={`${formatNumber(ov?.prompt_tokens ?? 0)} / ${formatNumber(ov?.completion_tokens ?? 0)}`} tint="#e8fbf4" suffix="tok" /></Col>
        <Col xs={24} sm={12} md={8} lg={6}><StatTile title="P95 首字响应" value={formatNumber(Math.round(ov?.p95_ttft_ms ?? 0))} tint="#fff4e6" suffix="ms" /></Col>
        <Col xs={24} sm={12} md={8} lg={6}><StatTile title="P95 总耗时" value={formatNumber(Math.round(ov?.p95_total_ms ?? 0))} tint="#fff4e6" suffix="ms" /></Col>
        {(ov?.fallback_count ?? 0) > 0 && <Col xs={24} sm={12} md={8} lg={6}><StatTile title="兜底使用" value={formatNumber(ov?.fallback_count ?? 0)} tint="#fff0f0" suffix={`${((ov?.fallback_rate ?? 0) * 100).toFixed(1)}%`} /></Col>}
      </Row>

      <Card style={{ marginBottom: 16 }}>
        <Space wrap size={[12, 12]} style={{ width: '100%' }}>
          <Radio.Group options={dimensions} value={dim} onChange={(event) => setDim(event.target.value)} optionType="button" />
          <Select value={bucket} onChange={setBucket} options={bucketOptions} style={{ width: 112 }} aria-label="时间粒度" />
          <Select allowClear value={route} onChange={setRoute} options={filterOptions('route')} placeholder="路由" style={{ width: 156 }} />
          <Select allowClear value={model} onChange={setModel} options={filterOptions('model')} placeholder="模型" style={{ width: 176 }} showSearch />
          <Select allowClear value={provider} onChange={setProvider} options={filterOptions('provider')} placeholder="提供商" style={{ width: 150 }} />
          <DatePicker.RangePicker value={range} onChange={(value) => value?.[0] && value[1] && setRange([value[0], value[1]])} />
          <CurrencyToggle value={currency} onChange={setCurrency} />
        </Space>
      </Card>

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={24} lg={14}><Card title="请求量与状态趋势"><ChartOrEmpty option={requestTrendOption(points, bucket)} points={points} /></Card></Col>
        <Col xs={24} lg={10}><Card title="费用与 Tokens 趋势"><ChartOrEmpty option={costTokenOption(points, bucket)} points={points} /></Card></Col>
        <Col xs={24} lg={14}><Card title="延迟趋势"><ChartOrEmpty option={latencyOption(points, bucket)} points={points} /></Card></Col>
        <Col xs={24} lg={10}><Card title="成功 / 错误分布">{statusDistribution.length ? <Chart option={distributionOption(statusDistribution)} height={300} /> : <Empty description="暂无状态数据" image={Empty.PRESENTED_IMAGE_SIMPLE} />}</Card></Col>
      </Row>

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={24} lg={14}><Card title={`${rankingLabel} Top 8`}>{items.length ? <Chart option={rankingOption(items)} height={300} /> : <Empty description="暂无排行数据" image={Empty.PRESENTED_IMAGE_SIMPLE} />}</Card></Col>
        <Col xs={24} lg={10}><Card title="兜底使用趋势">{(ov?.fallback_count ?? 0) > 0 ? <ChartOrEmpty option={fallbackOption(points, bucket)} points={points} /> : <Empty description="当前时间范围未使用兜底" image={Empty.PRESENTED_IMAGE_SIMPLE} />}</Card></Col>
      </Row>

      {errorDistribution.length > 0 && <Card title="错误码分布" style={{ marginBottom: 16 }}><Chart option={distributionOption(errorDistribution, ['#c50000', '#ee0000', '#ab570a', '#7928ca'])} height={300} /></Card>}

      <Card title="维度明细">
        <Table<Item> rowKey="dim" dataSource={items} size="small" tableLayout="fixed" scroll={{ x: 1160 }} pagination={{ pageSize: 20 }}>
          <Table.Column title="维度值" dataIndex="dim" render={(_, item: Item) => dim === 'key' ? <code style={{ fontSize: 12 }}>{item.key_name || item.key_masked || `key#${item.dim}`}</code> : item.dim} />
          <Table.Column title="请求" dataIndex="total" sorter={(a: Item, b: Item) => a.total - b.total} render={(value) => formatNumber(Number(value))} />
          <Table.Column title="成功" dataIndex="success" render={(value) => formatNumber(Number(value))} />
          <Table.Column title="错误" dataIndex="errors" render={(value) => formatNumber(Number(value))} />
          <Table.Column title="错误率" render={(_, item: Item) => item.total ? `${((1 - item.success / item.total) * 100).toFixed(1)}%` : '-'} />
          <Table.Column title="Prompt" dataIndex="prompt_tokens" render={(value) => formatNumber(Number(value))} />
          <Table.Column title="Completion" dataIndex="completion_tokens" render={(value) => formatNumber(Number(value))} />
          <Table.Column title="费用" dataIndex="cost" render={(value) => formatCost(Number(value), currency)} />
          <Table.Column title="平均首字响应" dataIndex="avg_ttft_ms" render={(value) => `${Math.round(Number(value))}ms`} />
          <Table.Column title="平均耗时" dataIndex="avg_total_ms" render={(value) => `${Math.round(Number(value))}ms`} />
          <Table.Column title="平均重试" dataIndex="avg_retries" render={(value) => Number(value).toFixed(2)} />
        </Table>
      </Card>
    </div>
  )
}
