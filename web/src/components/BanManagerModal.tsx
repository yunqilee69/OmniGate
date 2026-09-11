import { useCallback, useEffect, useState } from 'react'
import { Button, Modal, Space, Table, Tooltip, message } from 'antd'
import { api } from '../api'
import StatusTag from './StatusTag'

interface ComboBanItem {
  key_id: number
  key_name: string
  key_masked: string
  status: 'active' | 'temp_banned' | 'perm_banned'
  banned_until: number
  fail_count: number
  ban_reason?: string
  last_error?: string
}

interface ModelRef {
  id: number
  name: string
}

function statusTag(status: string, item: ComboBanItem) {
  switch (status) {
    case 'perm_banned':
      return <Tooltip title={item.ban_reason || item.last_error || '已禁用'}><StatusTag tone="error">已禁用</StatusTag></Tooltip>
    case 'temp_banned':
      return <Tooltip title={item.ban_reason || '临时冷却'}><StatusTag tone="warn">临时冷却</StatusTag></Tooltip>
    default:
      return <StatusTag tone="ok">正常</StatusTag>
  }
}

// 禁用管理弹窗：查看模型下各密钥组合的禁用明细，并支持单个/批量解禁。
export default function BanManagerModal({ open, model, onClose, onChanged }: {
  open: boolean
  model: ModelRef | null
  onClose: () => void
  onChanged: () => void
}) {
  const [rows, setRows] = useState<ComboBanItem[]>([])
  const [selected, setSelected] = useState<number[]>([])
  const [loading, setLoading] = useState(false)
  const [submitting, setSubmitting] = useState(false)

  const load = useCallback(async () => {
    if (!model) return
    setLoading(true)
    try {
      setRows(await api<ComboBanItem[]>('GET', `/api/models/${model.id}/bans`))
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [model])

  useEffect(() => {
    if (open && model) {
      setSelected([])
      void load()
    }
  }, [open, model, load])

  const bannedIds = rows.filter((r) => r.status !== 'active').map((r) => r.key_id)
  const selectedBanned = selected.filter((id) => bannedIds.includes(id))

  const unban = async (keyId: number) => {
    if (!model) return
    setSubmitting(true)
    try {
      await api('DELETE', `/api/models/${model.id}/bans/${keyId}`)
      await load()
      onChanged()
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  const unbanSelected = async () => {
    if (!model || selectedBanned.length === 0) return
    setSubmitting(true)
    try {
      await Promise.all(selectedBanned.map((id) => api('DELETE', `/api/models/${model.id}/bans/${id}`)))
      message.success(`已解禁 ${selectedBanned.length} 个组合`)
      setSelected([])
      await load()
      onChanged()
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      title={model ? `禁用管理 — ${model.name}` : '禁用管理'}
      open={open}
      onCancel={onClose}
      footer={null}
      width={760}
    >
      <Space style={{ marginBottom: 12 }}>
        <Button danger disabled={selectedBanned.length === 0} loading={submitting} onClick={unbanSelected}>
          解禁选中（{selectedBanned.length}）
        </Button>
      </Space>
      <Table<ComboBanItem>
        rowKey="key_id"
        dataSource={rows}
        loading={loading}
        size="small"
        pagination={false}
        rowSelection={{
          selectedRowKeys: selected,
          onChange: (keys) => setSelected(keys as number[]),
          getCheckboxProps: (r) => ({ disabled: !bannedIds.includes((r as ComboBanItem).key_id) }),
        }}
      >
        <Table.Column title="密钥" width={200} render={(_, r: ComboBanItem) => (
          <Space size={4}>
            <span>{r.key_name || `key#${r.key_id}`}</span>
            <code style={{ fontSize: 12, color: '#8f8f8f' }}>{r.key_masked}</code>
          </Space>
        )} />
        <Table.Column title="状态" width={100} render={(_, r: ComboBanItem) => statusTag(r.status, r)} />
        <Table.Column title="原因 / 到期" ellipsis render={(_, r: ComboBanItem) => {
          if (r.status === 'active') return <span style={{ color: '#8f8f8f' }}>-</span>
          if (r.status === 'temp_banned') {
            const remain = r.banned_until - Math.floor(Date.now() / 1000)
            return <span>{r.ban_reason || '临时冷却'}{remain > 0 ? `（${remain}s 后到期）` : ''}</span>
          }
          return <span>{r.ban_reason || r.last_error || '-'}</span>
        }} />
        <Table.Column title="失败次数" dataIndex="fail_count" width={90} render={(v) => (v || 0)} />
        <Table.Column title="操作" width={90} render={(_, r: ComboBanItem) => (
          r.status === 'active'
            ? <span style={{ color: '#8f8f8f' }}>-</span>
            : <Button size="small" danger loading={submitting} onClick={() => unban(r.key_id)}>解禁</Button>
        )} />
      </Table>
    </Modal>
  )
}
