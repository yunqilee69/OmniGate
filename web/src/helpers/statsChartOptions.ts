import type { EChartsOption } from 'echarts'
import dayjs from 'dayjs'
import type { Item, TimeseriesPoint } from '../fixtures/statsFixtures'

const colors = {
  ink: '#171717',
  body: '#4d4d4d',
  mute: '#8f8f8f',
  hairline: '#ebebeb',
  link: '#0070f3',
  success: '#16805c',
  warning: '#ab570a',
  error: '#c50000',
  violet: '#7928ca',
}

const axisStyle = { axisLine: { lineStyle: { color: colors.hairline } }, axisLabel: { color: colors.mute } }

export function bucketLabel(timestamp: number, bucket: string): string {
  if (bucket === '1h') return dayjs(timestamp * 1000).format('MM-DD HH:mm')
  if (bucket === '1w') return dayjs(timestamp * 1000).format('MM-DD')
  return dayjs(timestamp * 1000).format('MM-DD')
}

const baseGrid = { left: 48, right: 24, top: 40, bottom: 32, containLabel: true }

export function requestTrendOption(points: TimeseriesPoint[], bucket: string): EChartsOption {
  const labels = points.map((point) => bucketLabel(point.bucket, bucket))
  return {
    color: [colors.ink, colors.success, colors.error],
    tooltip: { trigger: 'axis' },
    legend: { top: 0, data: ['请求数', '成功', '错误'], textStyle: { color: colors.body } },
    grid: baseGrid,
    xAxis: { type: 'category', data: labels, axisLabel: { ...axisStyle.axisLabel, interval: Math.max(0, Math.floor(labels.length / 8) - 1) }, axisLine: axisStyle.axisLine },
    yAxis: { type: 'value', minInterval: 1, axisLabel: axisStyle.axisLabel, splitLine: { lineStyle: { color: colors.hairline } } },
    series: [
      { name: '请求数', type: 'bar', data: points.map((point) => point.total), barMaxWidth: 18 },
      { name: '成功', type: 'line', smooth: true, showSymbol: false, data: points.map((point) => point.success) },
      { name: '错误', type: 'line', smooth: true, showSymbol: false, data: points.map((point) => point.errors) },
    ],
  }
}

export function costTokenOption(points: TimeseriesPoint[], bucket: string): EChartsOption {
  return {
    color: [colors.violet, colors.warning],
    tooltip: { trigger: 'axis' },
    legend: { top: 0, data: ['费用', '总 Tokens'], textStyle: { color: colors.body } },
    grid: baseGrid,
    xAxis: { type: 'category', data: points.map((point) => bucketLabel(point.bucket, bucket)), axisLabel: { ...axisStyle.axisLabel, interval: Math.max(0, Math.floor(points.length / 8) - 1) }, axisLine: axisStyle.axisLine },
    yAxis: [
      { type: 'value', name: '费用', axisLabel: axisStyle.axisLabel, splitLine: { lineStyle: { color: colors.hairline } } },
      { type: 'value', name: 'Tokens', axisLabel: axisStyle.axisLabel, splitLine: { show: false } },
    ],
    series: [
      { name: '费用', type: 'line', smooth: true, showSymbol: false, data: points.map((point) => point.cost) },
      { name: '总 Tokens', type: 'bar', yAxisIndex: 1, data: points.map((point) => point.total_tokens), barMaxWidth: 18 },
    ],
  }
}

export function latencyOption(points: TimeseriesPoint[], bucket: string): EChartsOption {
  return {
    color: [colors.link, colors.ink],
    tooltip: { trigger: 'axis', valueFormatter: (value) => `${Math.round(Number(value))} ms` },
    legend: { top: 0, data: ['首字响应', '总耗时'], textStyle: { color: colors.body } },
    grid: baseGrid,
    xAxis: { type: 'category', data: points.map((point) => bucketLabel(point.bucket, bucket)), axisLabel: { ...axisStyle.axisLabel, interval: Math.max(0, Math.floor(points.length / 8) - 1) }, axisLine: axisStyle.axisLine },
    yAxis: { type: 'value', name: '毫秒', axisLabel: axisStyle.axisLabel, splitLine: { lineStyle: { color: colors.hairline } } },
    series: [
      { name: '首字响应', type: 'line', smooth: true, showSymbol: false, data: points.map((point) => point.avg_ttft_ms) },
      { name: '总耗时', type: 'line', smooth: true, showSymbol: false, data: points.map((point) => point.avg_total_ms) },
    ],
  }
}

export function fallbackOption(points: TimeseriesPoint[], bucket: string): EChartsOption {
  return {
    color: [colors.warning],
    tooltip: { trigger: 'axis' },
    grid: baseGrid,
    xAxis: { type: 'category', data: points.map((point) => bucketLabel(point.bucket, bucket)), axisLabel: { ...axisStyle.axisLabel, interval: Math.max(0, Math.floor(points.length / 8) - 1) }, axisLine: axisStyle.axisLine },
    yAxis: { type: 'value', minInterval: 1, name: '次数', axisLabel: axisStyle.axisLabel, splitLine: { lineStyle: { color: colors.hairline } } },
    series: [{ name: '兜底次数', type: 'bar', data: points.map((point) => point.fallback_count), barMaxWidth: 18 }],
  }
}

export function distributionOption(data: readonly { name: string; value: number }[], palette: string[] = [colors.success, colors.error, colors.warning, colors.violet]): EChartsOption {
  return {
    color: palette,
    tooltip: { trigger: 'item' },
    legend: { bottom: 0, type: 'scroll', textStyle: { color: colors.body } },
    series: [{ type: 'pie', radius: ['48%', '72%'], center: ['50%', '46%'], avoidLabelOverlap: true, itemStyle: { borderColor: '#ffffff', borderWidth: 2 }, label: { formatter: '{b}\n{d}%', color: colors.body }, data: [...data] }],
  }
}

export function rankingOption(items: Item[]): EChartsOption {
  const rows = [...items].sort((a, b) => b.total - a.total).slice(0, 8).reverse()
  return {
    color: [colors.ink],
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    grid: { left: 112, right: 28, top: 16, bottom: 24, containLabel: true },
    xAxis: { type: 'value', minInterval: 1, axisLabel: axisStyle.axisLabel, splitLine: { lineStyle: { color: colors.hairline } } },
    yAxis: { type: 'category', data: rows.map((item) => item.dim), axisLabel: { color: colors.body }, axisTick: { show: false } },
    series: [{ type: 'bar', data: rows.map((item) => item.total), barMaxWidth: 18, label: { show: true, position: 'right', color: colors.body } }],
  }
}
