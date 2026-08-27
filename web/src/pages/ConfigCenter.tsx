import { useEffect, useRef, useState } from 'react'
import {
  Button, Card, Empty, Form, Input, InputNumber, Modal, Popconfirm, Select,
  Space, Spin, Table, Tabs, Tag, Tooltip, message,
} from 'antd'
import { PlusOutlined, CloudServerOutlined, EyeOutlined, EyeInvisibleOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import { api } from '../api'
import StatusTag from '../components/StatusTag'

interface Provider {
  id: number
  name: string
  base_url: string
  protocol: string
  timeout_ms: number
  remark: string
}

interface Key {
  id: number
  provider_id: number
  key_value: string
  key_value_plain?: string
  name: string
  status: 'active' | 'cooldown' | 'disabled'
  cooldown_until: number
  rate_limited_count: number
  disable_reason: string
  last_used_at: number
  last_error: string
}

interface Model {
  id: number
  provider_id: number
  name: string
  protocol: string
  input_price: number
  output_price: number
  status: string
  fail_count: number
  cooldown_until: number
  disable_reason: string
  last_error: string
  key_ids: number[]
}

const protocolOptions = [
  { value: 'openai', label: 'OpenAI（chat/completions）' },
  { value: 'responses', label: 'OpenAI Responses（/responses）' },
  { value: 'anthropic', label: 'Anthropic（/v1/messages）' },
]

const nowSec = () => Math.floor(Date.now() / 1000)

const modelStatusTag = (m: Model) => {
  if (m.status === 'active') return <StatusTag tone="ok">正常</StatusTag>
  if (m.status === 'cooldown') {
    const remain = m.cooldown_until - nowSec()
    return <StatusTag tone="warn">冷却{remain > 0 ? ` ${remain}s` : '（半开）'}</StatusTag>
  }
  return <Tooltip title={m.disable_reason}><StatusTag tone="error">已禁用</StatusTag></Tooltip>
}

const keyStatusTag = (k: Key) => {
  if (k.status === 'active') return <StatusTag tone="ok">可用</StatusTag>
  if (k.status === 'cooldown') {
    const remain = k.cooldown_until - nowSec()
    return <StatusTag tone="warn">限流冷却{remain > 0 ? ` ${remain}s` : ''}</StatusTag>
  }
  return <Tooltip title={k.disable_reason}><StatusTag tone="error">已禁用</StatusTag></Tooltip>
}

export default function ConfigCenter() {
  const [providers, setProviders] = useState<Provider[]>([])
  const [keys, setKeys] = useState<Key[]>([])
  const [models, setModels] = useState<Model[]>([])
  const [selected, setSelected] = useState<number | null>(null)
  const [loading, setLoading] = useState(false)

  const load = async (prefer?: number) => {
    setLoading(true)
    try {
      const [ps, ks, ms] = await Promise.all([
        api<Provider[]>('GET', '/api/providers'), api<Key[]>('GET', '/api/keys'), api<Model[]>('GET', '/api/models'),
      ])
      setProviders(ps)
      setKeys(ks)
      setModels(ms)
      const cur = prefer ?? selected
      setSelected(cur && ps.some((p) => p.id === cur) ? cur : (ps[0]?.id ?? null))
    } catch (e: any) {
      message.error(e.message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => { load() }, [])

  const provider = providers.find((p) => p.id === selected) ?? null
  const providerKeys = keys.filter((k) => k.provider_id === selected)
  const providerModels = models.filter((m) => m.provider_id === selected)

  return (
    <div style={{ display: 'flex', gap: 16, alignItems: 'flex-start' }}>
      <div style={{ width: 280, flexShrink: 0 }}>
        <Card
          title="提供商"
          size="small"
          extra={<ProviderFormButton providers={providers} onSaved={() => load()} />}
        >
          {loading && providers.length === 0 ? <Spin /> : providers.length === 0 ? (
            <Empty description="暂无提供商" image={Empty.PRESENTED_IMAGE_SIMPLE} />
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              {providers.map((p) => (
                <ProviderCardItem
                  key={p.id}
                  p={p}
                  active={p.id === selected}
                  modelCount={models.filter((m) => m.provider_id === p.id).length}
                  keyCount={keys.filter((k) => k.provider_id === p.id).length}
                  onClick={() => setSelected(p.id)}
                  onSaved={() => load(p.id)}
                  onDeleted={() => load(providers.find((x) => x.id !== p.id)?.id)}
                />
              ))}
            </div>
          )}
        </Card>
      </div>
      <div style={{ flex: 1, minWidth: 0 }}>
        {provider ? (
          <Tabs
            defaultActiveKey="models"
            items={[
              {
                key: 'models',
                label: `模型（${providerModels.length}）`,
                children: (
                  <ModelsTab
                    provider={provider}
                    keys={providerKeys}
                    models={providerModels}
                    onSaved={() => load(provider.id)}
                  />
                ),
              },
              {
                key: 'keys',
                label: `密钥（${providerKeys.length}）`,
                children: <KeysTab provider={provider} keys={providerKeys} onSaved={() => load(provider.id)} />,
              },
            ]}
          />
        ) : (
          <Card><Empty description="左侧选择或新增一个提供商" /></Card>
        )}
      </div>
    </div>
  )
}

function ProviderCardItem({ p, active, modelCount, keyCount, onClick, onSaved, onDeleted }: {
  p: Provider
  active: boolean
  modelCount: number
  keyCount: number
  onClick: () => void
  onSaved: () => void
  onDeleted: () => void
}) {
  const [open, setOpen] = useState(false)
  const [form] = Form.useForm()

  const submit = async () => {
    const values = await form.validateFields()
    try {
      await api('PUT', `/api/providers/${p.id}`, values)
      message.success('已保存')
      setOpen(false)
      onSaved()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  return (
    <Card
      size="small"
      hoverable
      onClick={onClick}
      style={active ? { borderColor: '#171717', background: '#fafafa' } : undefined}
    >
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <Space>
          <CloudServerOutlined />
          <b>{p.name}</b>
        </Space>
        <Space onClick={(e) => e.stopPropagation()}>
          <a onClick={() => { form.setFieldsValue(p); setOpen(true) }}>编辑</a>
          <Popconfirm
            title="删除将级联清理密钥/模型，确认？"
            onConfirm={async () => {
              try { await api('DELETE', `/api/providers/${p.id}`); message.success('已删除'); onDeleted() } catch (e: any) { message.error(e.message) }
            }}
          >
            <a style={{ color: '#ee0000' }}>删除</a>
          </Popconfirm>
        </Space>
      </div>
      <div style={{ color: '#8f8f8f', fontSize: 12, marginTop: 4 }}>
        <div style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.base_url}</div>
        <div style={{ marginTop: 2 }}>{modelCount} 模型 · {keyCount} 密钥</div>
      </div>

      <Modal title={`编辑提供商 ${p.name}`} open={open} onOk={submit} onCancel={() => setOpen(false)} destroyOnClose>
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="名称" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="base_url" label="Base URL" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="timeout_ms" label="首字节超时(ms)"><InputNumber min={1000} style={{ width: '100%' }} /></Form.Item>
          <Form.Item name="remark" label="备注"><Input /></Form.Item>
        </Form>
      </Modal>
    </Card>
  )
}

function ProviderFormButton({ providers, onSaved }: { providers: Provider[]; onSaved: () => void }) {
  const [open, setOpen] = useState(false)
  const [form] = Form.useForm()

  const submit = async () => {
    const values = await form.validateFields()
    try {
      const created = await api<Provider>('POST', '/api/providers', values)
      message.success('已创建')
      setOpen(false)
      form.resetFields()
      onSaved()
      void created
    } catch (e: any) {
      message.error(e.message)
    }
  }
  void providers

  return (
    <>
      <Button size="small" type="primary" icon={<PlusOutlined />} onClick={() => setOpen(true)}>新增</Button>
      <Modal title="新增提供商" open={open} onOk={submit} onCancel={() => setOpen(false)} destroyOnClose>
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="名称" rules={[{ required: true }]}><Input placeholder="如 zhipu / openrouter" /></Form.Item>
          <Form.Item name="base_url" label="Base URL" rules={[{ required: true }]}>
            <Input placeholder="https://open.bigmodel.cn/api/paas/v4" />
          </Form.Item>
          <Form.Item name="timeout_ms" label="首字节超时(ms)" initialValue={120000}>
            <InputNumber min={1000} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="remark" label="备注"><Input /></Form.Item>
        </Form>
      </Modal>
    </>
  )
}

function ModelsTab({ provider, keys, models, onSaved }: {
  provider: Provider
  keys: Key[]
  models: Model[]
  onSaved: () => void
}) {
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Model | null>(null)
  const [testing, setTesting] = useState<Set<number>>(new Set())
  const [batchTesting, setBatchTesting] = useState(false)
  const [batchResults, setBatchResults] = useState<any[] | null>(null)
  const [form] = Form.useForm()

  const testOne = async (m: Model) => {
    setTesting((s) => new Set(s).add(m.id))
    try {
      const res = await api('POST', `/api/models/${m.id}/test`)
      if (res.ok) {
        message.success(`${m.name} 可用（${res.latency_ms}ms）`)
      } else {
        Modal.error({
          title: `${m.name} 不可用`,
          content: `错误：${res.error_code || '-'}${res.message ? ` — ${res.message}` : ''}`,
        })
      }
    } catch (e: any) {
      message.error(e.message)
    } finally {
      setTesting((s) => {
        const next = new Set(s)
        next.delete(m.id)
        return next
      })
    }
  }

  const runBatchTest = async () => {
    setBatchTesting(true)
    try {
      const results = await api('POST', `/api/providers/${provider.id}/test`)
      setBatchResults(results)
    } catch (e: any) {
      message.error(e.message)
    } finally {
      setBatchTesting(false)
    }
  }

  const openForm = (m?: Model) => {
    setEditing(m ?? null)
    form.resetFields()
    if (m) {
      form.setFieldsValue({
        name: m.name, protocol: m.protocol,
        input_price: m.input_price, output_price: m.output_price, key_ids: m.key_ids,
      })
    }
    setOpen(true)
  }

  const submit = async () => {
    const values = await form.validateFields()
    try {
      if (editing) {
        await api('PUT', `/api/models/${editing.id}`, values)
      } else {
        await api('POST', '/api/models', { ...values, provider_id: provider.id })
      }
      message.success('已保存（即时生效）')
      setOpen(false)
      onSaved()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  const toggle = async (m: Model) => {
    try {
      await api('POST', `/api/models/${m.id}/${m.status === 'disabled' ? 'enable' : 'disable'}`)
      onSaved()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" onClick={() => openForm()}>新增模型</Button>
        <Button loading={batchTesting} onClick={runBatchTest}>测试全部模型</Button>
      </Space>
      <Table<Model> rowKey="id" dataSource={models} tableLayout="fixed" size="small" scroll={{ x: true }}>
        <Table.Column title="模型名" dataIndex="name" width={160} />
        <Table.Column title="协议" dataIndex="protocol" width={200} render={(v) => (
          <span className="mono" style={{ fontSize: 12, color: '#4d4d4d' }}>{v}</span>
        )} />
        <Table.Column title="绑定密钥" dataIndex="key_ids" render={(ids: number[]) =>
          (ids ?? []).map((id) => {
    const k = keys.find((x) => x.id === id)
    return <Tag key={id}>{k ? (k.name || k.key_value) : `#${id}`}</Tag>
  })
        } />
        <Table.Column title="输入/输出价(1M)" width={140} render={(_, m: Model) => `${m.input_price} / ${m.output_price}`} />
        <Table.Column title="状态" width={110} render={(_, m: Model) => modelStatusTag(m)} />
        <Table.Column title="连续失败" dataIndex="fail_count" width={80} />
        <Table.Column title="操作" width={240} render={(_, m: Model) => (
          <Space>
            <Button size="small" loading={testing.has(m.id)} onClick={() => testOne(m)}>测试</Button>
            <Button size="small" onClick={() => openForm(m)}>编辑</Button>
            <Button size="small" onClick={() => toggle(m)}>{m.status === 'disabled' ? '解禁' : '禁用'}</Button>
            <Popconfirm title="删除模型将清理路由目标与密钥绑定，确认？" onConfirm={async () => {
              try { await api('DELETE', `/api/models/${m.id}`); onSaved() } catch (e: any) { message.error(e.message) }
            }}>
              <Button size="small" danger>删除</Button>
            </Popconfirm>
          </Space>
        )} />
      </Table>

      <Modal title={editing ? '编辑模型' : `新增模型（提供商：${provider.name}）`} open={open} onOk={submit} onCancel={() => setOpen(false)} destroyOnClose width={560}>
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="真实模型名" rules={[{ required: true }]}>
            <Input placeholder="如 glm-4.6 / claude-sonnet-4" />
          </Form.Item>
          <Form.Item name="protocol" label="上游协议" initialValue="openai" rules={[{ required: true }]} extra="决定出站请求格式与响应转换方式">
            <Select options={protocolOptions} />
          </Form.Item>
          <Form.Item label="价格（每 1M token）">
            <Space>
              <Form.Item name="input_price" initialValue={0} noStyle>
                <InputNumber min={0} placeholder="输入价" style={{ width: 200 }} />
              </Form.Item>
              <Form.Item name="output_price" initialValue={0} noStyle>
                <InputNumber min={0} placeholder="输出价" style={{ width: 200 }} />
              </Form.Item>
            </Space>
          </Form.Item>
          <Form.Item name="key_ids" label="绑定密钥（多选）" extra="仅显示当前提供商下的密钥"
            rules={[{ required: true, message: '至少绑定一个密钥' }]}>
            <Select
              mode="multiple"
              placeholder="选择可访问该模型的密钥"
              options={keys.map((k) => ({
                value: k.id,
                label: k.name || k.key_value,
              }))}
            />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={`批量测试 — ${provider.name}`}
        open={!!batchResults}
        onCancel={() => setBatchResults(null)}
        footer={null}
        width={640}
      >
        <Table dataSource={batchResults ?? []} rowKey="model_id" size="small" pagination={false}>
          <Table.Column title="模型" dataIndex="model" width={180} />
          <Table.Column title="协议" dataIndex="protocol" width={110} render={(v) => (
            <span className="mono" style={{ fontSize: 12 }}>{v}</span>
          )} />
          <Table.Column title="结果" width={90} render={(_, r: any) => r.ok
            ? <StatusTag tone="ok">可用</StatusTag>
            : <Tooltip title={`${r.error_code ?? ''} ${r.message ?? ''}`}><StatusTag tone="error">失败</StatusTag></Tooltip>} />
          <Table.Column title="耗时" dataIndex="latency_ms" width={90} render={(v) => `${v}ms`} />
          <Table.Column title="错误" render={(_, r: any) => r.ok ? '-' : `${r.error_code ?? ''} ${r.message ?? ''}`} ellipsis />
        </Table>
      </Modal>
    </div>
  )
}

function KeysTab({ provider, keys, onSaved }: { provider: Provider; keys: Key[]; onSaved: () => void }) {
  const [open, setOpen] = useState(false)
  const [plainValues, setPlainValues] = useState<Map<number, string> | null>(null)
  const [revealedIds, setRevealedIds] = useState<Set<number>>(new Set())
  const [revealLoadingId, setRevealLoadingId] = useState<number | null>(null)
  const revealAttempted = useRef(false)
  const [form] = Form.useForm()
  const [editingKey, setEditingKey] = useState<Key | null>(null)
  const [editForm] = Form.useForm()

  const submitKeys = async () => {
    const values = await form.validateFields()
    try {
      await api('POST', '/api/keys', {
        provider_id: provider.id,
        key_value: values.key_value.trim(),
        name: values.name.trim(),
      })
      message.success('已新增')
      setOpen(false)
      form.resetFields()
      onSaved()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  const setKeyStatus = async (key: Key, status: 'active' | 'disabled') => {
    try {
      await api('PUT', `/api/keys/${key.id}`, { status })
      onSaved()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  const saveKeyEdit = async () => {
    const values = await editForm.validateFields()
    try {
      await api('PUT', `/api/keys/${editingKey!.id}`, { name: values.name.trim(), key_value: values.key_value.trim() })
      message.success('已保存')
      setEditingKey(null)
      onSaved()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  const toggleReveal = async (key: Key) => {
    if (revealedIds.has(key.id)) {
      setRevealedIds((current) => {
        const next = new Set(current)
        next.delete(key.id)
        return next
      })
      return
    }

    let values = plainValues
    if (!values) {
      if (revealAttempted.current) return
      revealAttempted.current = true
      setRevealLoadingId(key.id)
      try {
        const revealedKeys = await api<Key[]>('GET', '/api/keys?reveal=1')
        values = new Map(revealedKeys.map((item) => [item.id, item.key_value_plain ?? item.key_value]))
        setPlainValues(values)
      } catch (e: any) {
        message.error(e.message)
        return
      } finally {
        setRevealLoadingId(null)
      }
    }
    setRevealedIds((current) => new Set(current).add(key.id))
  }

  const availableCount = keys.filter((key) => key.status === 'active').length

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" onClick={() => { form.resetFields(); setOpen(true) }}>新增密钥</Button>
        <span style={{ color: '#8f8f8f', fontSize: 12 }}>共 {keys.length} 个，可用 {availableCount} 个</span>
      </Space>
      <Table<Key> rowKey="id" dataSource={keys} tableLayout="fixed" size="small" scroll={{ x: true }}>
        <Table.Column title="名称" dataIndex="name" width={140} render={(value) => value || '-'} />
        <Table.Column title="密钥" dataIndex="key_value" width={260} render={(_, key: Key) => {
          const revealed = revealedIds.has(key.id)
          return (
            <Space size={4}>
              <code>{revealed ? (plainValues?.get(key.id) ?? key.key_value) : key.key_value}</code>
              <Button
                size="small"
                type="text"
                loading={revealLoadingId === key.id}
                icon={revealed ? <EyeInvisibleOutlined /> : <EyeOutlined />}
                onClick={() => toggleReveal(key)}
                aria-label={revealed ? '隐藏密钥' : '显示密钥'}
              />
            </Space>
          )
        }} />
        <Table.Column title="状态" render={(_, key: Key) => keyStatusTag(key)} width={130} />
        <Table.Column title="429次数" dataIndex="rate_limited_count" width={80} />
        <Table.Column title="最近使用" dataIndex="last_used_at" width={150}
          render={(value) => (value ? dayjs(value * 1000).format('MM-DD HH:mm:ss') : '-')} />
        <Table.Column title="最近错误" dataIndex="last_error" ellipsis render={(value) => value || '-'} />
        <Table.Column title="操作" width={240} render={(_, key: Key) => (
          <Space>
            <Button size="small" onClick={() => { editForm.setFieldsValue({ name: key.name, key_value: key.key_value }); setEditingKey(key) }}>编辑</Button>
            {key.status !== 'active' && <Button size="small" onClick={() => setKeyStatus(key, 'active')}>启用</Button>}
            {key.status !== 'disabled' && <Button size="small" danger ghost onClick={() => setKeyStatus(key, 'disabled')}>禁用</Button>}
            <Popconfirm title="确认删除该密钥？" onConfirm={async () => {
              try { await api('DELETE', `/api/keys/${key.id}`); onSaved() } catch (e: any) { message.error(e.message) }
            }}>
              <Button size="small" danger>删除</Button>
            </Popconfirm>
          </Space>
        )} />
      </Table>

      <Modal title="编辑密钥" open={!!editingKey} onOk={saveKeyEdit} onCancel={() => setEditingKey(null)} destroyOnClose>
        <Form form={editForm} layout="vertical">
          <Form.Item name="name" label="名称（日志、统计、模型绑定处显示）" rules={[{ required: true, message: '请输入名称' }]}>
            <Input placeholder="如：主力账号" maxLength={100} />
          </Form.Item>
          <Form.Item
            name="key_value"
            label="密钥值"
            extra="保持脱敏值（含 ****）不变则不修改原密钥；输入新值则替换"
            rules={[{ required: true, message: '请输入密钥值' }]}
          >
            <Input placeholder="sk-..." />
          </Form.Item>
        </Form>
      </Modal>

      <Modal title={`新增密钥（提供商：${provider.name}）`} open={open} onOk={submitKeys} onCancel={() => setOpen(false)} destroyOnClose>
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="名称（日志、统计、模型绑定处显示）" rules={[{ required: true, message: '请输入名称' }]}>
            <Input placeholder="如：主力账号" maxLength={100} />
          </Form.Item>
          <Form.Item name="key_value" label="密钥" rules={[{ required: true, message: '请输入密钥' }]}>
            <Input placeholder="sk-..." />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
