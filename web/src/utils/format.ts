// 千/百万/十亿/万亿 数字格式化。<1000 原样，>1000 自动加 K/M/B/T 后缀。
export function formatNumber(n: number): string {
  if (!isFinite(n)) return '0'
  const abs = Math.abs(n)
  if (abs < 1000) return n.toString()
  if (abs < 1_000_000) return trimZeros(n / 1000) + 'K'
  if (abs < 1_000_000_000) return trimZeros(n / 1_000_000) + 'M'
  if (abs < 1_000_000_000_000) return trimZeros(n / 1_000_000_000) + 'B'
  return trimZeros(n / 1_000_000_000_000) + 'T'
}

function trimZeros(v: number): string {
  const s = v.toFixed(2)
  return s.replace(/\.?0+$/, '')
}

// 费用：带币种符号；金额更细腻，<1 显示 4 位小数，否则 K/M/B/T
export function formatCost(v: number, currency: string = 'USD'): string {
  const sym = currency === 'CNY' ? '¥' : '$'
  if (!isFinite(v) || v === 0) return `${sym}0`
  if (v < 1) return sym + v.toFixed(4).replace(/0+$/, '').replace(/\.$/, '')
  return sym + formatNumber(v)
}
