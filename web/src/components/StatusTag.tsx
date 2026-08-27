import { Tag } from 'antd'
import type { CSSProperties } from 'react'

// Geist 语义标签：白底 + 发丝线 + 语义文字色（DESIGN.md Semantic 节）
const tones: Record<string, CSSProperties> = {
  ok: { color: '#0070f3', borderColor: '#d3e5ff', background: '#ffffff' },
  warn: { color: '#ab570a', borderColor: '#ffefcf', background: '#ffffff' },
  error: { color: '#ee0000', borderColor: 'rgba(238, 0, 0, 0.25)', background: '#ffffff' },
  mute: { color: '#8f8f8f', borderColor: '#ebebeb', background: '#ffffff' },
}

export default function StatusTag({ tone, children }: { tone: keyof typeof tones; children: React.ReactNode }) {
  return <Tag style={{ ...tones[tone], marginInlineEnd: 0 }}>{children}</Tag>
}
