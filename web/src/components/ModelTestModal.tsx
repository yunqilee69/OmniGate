import { useEffect, useRef, useState } from 'react'
import { Alert, Button, Empty, Modal, Space, Table, Tabs, Tooltip, message } from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import { api } from '../api'
import StatusTag from './StatusTag'

export interface KeyProbeResult {
  key_id: number
  key_name: string
  key_masked: string
  key_status: string
  ok: boolean
  http_status: number
  latency_ms: number
  error_code: string
  message: string
  prompt_tokens: number
  completion_tokens: number
}

export interface ModelTestResult {
  model_id: number
  model: string
  provider: string
  protocol: string
  keys: KeyProbeResult[]
  error?: string
}

export interface TestTarget { id: number; name: string }

function keyStatusTag(status: string) {
  if (status === 'active') return <StatusTag tone="ok">可用</StatusTag>
  if (status === 'cooldown') return <StatusTag tone="warn">冷却</StatusTag>
  return <StatusTag tone="error">已禁用</StatusTag>
}

function KeyResultTable({ result, onSetKeyStatus }: {
  result: ModelTestResult
  onSetKeyStatus: (keyId: number, status: 'active' | 'disabled') => void
}) {
  if (result.error) {
    return <Alert type="error" showIcon message={`调用失败：${result.error}`} />
  }
  if (!result.keys.length) {
    return <Empty description="该模型未绑定密钥" image={Empty.PRESENTED_IMAGE_SIMPLE} />
  }
  return (
    <Table<KeyProbeResult> rowKey="key_id" dataSource={result.keys} size="small" pagination={false} tableLayout="fixed">
      <Table.Column
        title="密钥"
        width={220}
        render={(_, k: KeyProbeResult) => (
          <Space size={4}>
            <span>{k.key_name || `key#${k.key_id}`}</span>
            <code style={{ fontSize: 12, color: '#8f8f8f' }}>{k.key_masked}</code>
          </Space>
        )}
      />
      <Table.Column title="当前状态" width={100} render={(_, k: KeyProbeResult) => keyStatusTag(k.key_status)} />
      <Table.Column
        title="测试结果"
        width={100}
        render={(_, k: KeyProbeResult) => (k.ok
          ? <StatusTag tone="ok">成功</StatusTag>
          : (
            <Tooltip title={[k.error_code, k.message].filter(Boolean).join(' — ')}>
              <StatusTag tone="error">失败</StatusTag>
            </Tooltip>
          ))}
      />
      <Table.Column title="HTTP" dataIndex="http_status" width={70} render={(v) => v || '-'} />
      <Table.Column title="耗时" dataIndex="latency_ms" width={90} render={(v) => `${v}ms`} />
      <Table.Column
        title="Tokens(入/出)"
        width={110}
        render={(_, k: KeyProbeResult) => ((k.prompt_tokens || k.completion_tokens) ? `${k.prompt_tokens}/${k.completion_tokens}` : '-')}
      />
      <Table.Column
        title="错误原因"
        ellipsis
        render={(_, k: KeyProbeResult) => (k.ok
          ? <span style={{ color: '#8f8f8f' }}>-</span>
          : (
            <Tooltip title={[k.error_code, k.message].filter(Boolean).join(' — ')}>
              <span style={{ color: '#cf1322' }}>{[k.error_code, k.message].filter(Boolean).join(' — ') || '未知错误'}</span>
            </Tooltip>
          ))}
      />
      <Table.Column
        title="操作"
        width={90}
        render={(_, k: KeyProbeResult) => (k.key_status === 'disabled'
          ? <Button size="small" onClick={() => onSetKeyStatus(k.key_id, 'active')}>启用</Button>
          : <Button size="small" danger ghost onClick={() => onSetKeyStatus(k.key_id, 'disabled')}>禁用</Button>)}
      />
    </Table>
  )
}

// 模型测试弹窗：逐密钥并发探测。单个模型直接出表格；多个模型时 Tabs 切换。
export default function ModelTestModal({ open, targets, onClose, onKeysChanged }: {
  open: boolean
  targets: TestTarget[]
  onClose: () => void
  onKeysChanged?: () => void
}) {
  const [results, setResults] = useState<Record<number, ModelTestResult>>({})
  const [pending, setPending] = useState<Set<number>>(new Set())
  const runIdRef = useRef(0)

  const run = async () => {
    if (!targets.length) return
    const runId = ++runIdRef.current
    setResults({})
    setPending(new Set(targets.map((t) => t.id)))
    await Promise.all(targets.map(async (t) => {
      try {
        const res = await api<ModelTestResult>('POST', `/api/models/${t.id}/test-keys`)
        if (runId !== runIdRef.current) return
        setResults((prev) => ({ ...prev, [t.id]: res }))
      } catch (e: any) {
        if (runId !== runIdRef.current) return
        setResults((prev) => ({
          ...prev,
          [t.id]: { model_id: t.id, model: t.name, provider: '', protocol: '', keys: [], error: e.message },
        }))
      } finally {
        if (runId === runIdRef.current) {
          setPending((prev) => {
            const next = new Set(prev)
            next.delete(t.id)
            return next
          })
        }
      }
    }))
  }

  useEffect(() => {
    if (open) {
      void run()
    } else {
      runIdRef.current++
      setResults({})
      setPending(new Set())
    }
    // targets 由调用方在打开前设置，这里仅响应开合
  }, [open])

  const setKeyStatus = async (modelId: number, keyId: number, status: 'active' | 'disabled') => {
    try {
      await api('PUT', `/api/keys/${keyId}`, { status })
      setResults((prev) => {
        const m = prev[modelId]
        if (!m) return prev
        return {
          ...prev,
          [modelId]: { ...m, keys: m.keys.map((k) => (k.key_id === keyId ? { ...k, key_status: status } : k)) },
        }
      })
      onKeysChanged?.()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  const batch = targets.length > 1

  return (
    <Modal
      title={batch ? `测试全部模型（${targets.length} 个，逐密钥并发）` : `模型测试 — ${targets[0]?.name ?? ''}`}
      open={open}
      onCancel={onClose}
      width={batch ? 920 : 800}
      footer={[
        <Button key="rerun" icon={<ReloadOutlined />} loading={pending.size > 0} onClick={() => void run()}>重新测试</Button>,
        <Button key="close" type="primary" onClick={onClose}>关闭</Button>,
      ]}
    >
      {batch ? (
        <Tabs
          items={targets.map((t) => {
            const res = results[t.id]
            const label = !res
              ? (pending.has(t.id) ? `${t.name}（测试中…）` : t.name)
              : res.error ? `${t.name}（调用失败）` : `${t.name}（${res.keys.filter((k) => k.ok).length}/${res.keys.length}）`
            return {
              key: String(t.id),
              label,
              children: res
                ? <KeyResultTable result={res} onSetKeyStatus={(kid, st) => void setKeyStatus(t.id, kid, st)} />
                : <Empty description="等待测试…" image={Empty.PRESENTED_IMAGE_SIMPLE} />,
            }
          })}
        />
      ) : (
        targets[0] && (results[targets[0].id]
          ? <KeyResultTable result={results[targets[0].id]} onSetKeyStatus={(kid, st) => void setKeyStatus(targets[0].id, kid, st)} />
          : <Empty description="测试中…" image={Empty.PRESENTED_IMAGE_SIMPLE} />)
      )}
    </Modal>
  )
}
