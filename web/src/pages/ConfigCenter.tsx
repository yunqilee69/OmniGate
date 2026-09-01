import { useEffect, useRef, useState } from 'react'
import {
  Button, Card, Checkbox, Dropdown, Empty, Form, Input, InputNumber, Modal, Popconfirm, Select,
  Space, Spin, Table, Tabs, Tag, Tooltip, message,
} from 'antd'
import { 
  PlusOutlined, CloudServerOutlined, EyeOutlined, EyeInvisibleOutlined, 
  InfoCircleOutlined, ApiOutlined, DownloadOutlined, UploadOutlined, 
  DeleteOutlined, MoreOutlined 
} from '@ant-design/icons'
import dayjs from 'dayjs'
import { api } from '../api'
import StatusTag from '../components/StatusTag'
import ModelTestModal, { type TestTarget } from '../components/ModelTestModal'

interface Provider {
  id: number
  name: string
  base_url: string
  protocol: string
  proxy_url: string
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
  type: string
  protocol: string
  input_price: number
  output_price: number
  price_currency: string
  status: string
  fail_count: number
  cooldown_until: number
  disable_reason: string
  last_error: string
  key_ids: number[]
}

const protocolOptions = [
  { value: 'openai', label: 'OpenAI（/chat/completions）' },
  { value: 'responses', label: 'OpenAI Responses（/responses）' },
  { value: 'anthropic', label: 'Anthropic（/messages）' },
]

const modelTypeOptions = [
  { value: 'chat', label: '对话（/v1/chat/completions）' },
  { value: 'embedding', label: '向量（/v1/embeddings）' },
  { value: 'rerank', label: '重排（/v1/rerank）' },
]

const proxyURLLabel = (
  <Space size={4}>
    代理 URL
    <Tooltip
      title={(
        <div style={{ maxWidth: 320 }}>
          <div>需要认证时，格式为：</div>
          <code>http://用户名:密码@代理地址:端口</code>
          <div style={{ marginTop: 6 }}>用户名和密码中的特殊字符请分别进行 URL 编码，例如：</div>
          <code>@ → %40　: → %3A　/ → %2F　# → %23</code>
          <div style={{ marginTop: 6 }}>例如密码为 p@ss:word，应填写：</div>
          <code>http://user:p%40ss%3Aword@127.0.0.1:7890</code>
        </div>
      )}
    >
      <InfoCircleOutlined tabIndex={0} aria-label="代理 URL 配置说明" />
    </Tooltip>
  </Space>
)

const modelTypeTag = (t: string) => {
  if (t === 'embedding') return <Tag color="geekblue">embedding</Tag>
  if (t === 'rerank') return <Tag color="purple">rerank</Tag>
  return <Tag>chat</Tag>
}

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
  const [selectedProviders, setSelectedProviders] = useState<number[]>([])
  const [importing, setImporting] = useState(false)

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
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => { load() }, [])

  const handleExport = async () => {
    try {
      const config = await api('GET', '/api/providers/export')
      const blob = new Blob([JSON.stringify(config, null, 2)], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `omnigate-config-${new Date().toISOString().split('T')[0]}.json`
      a.click()
      URL.revokeObjectURL(url)
      message.success('配置已导出')
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : String(e))
    }
  }

  const handleImport = () => {
    const input = document.createElement('input')
    input.type = 'file'
    input.accept = '.json'
    input.onchange = async (e) => {
      const file = (e.target as HTMLInputElement).files?.[0]
      if (!file) return
      setImporting(true)
      try {
        const text = await file.text()
        const config = JSON.parse(text)
        const result = await api<{ imported: number; skipped: number; errors: string[] }>('POST', '/api/providers/import', config)
        if (result.errors.length > 0) {
          message.warning(`导入完成：成功 ${result.imported} 个，跳过 ${result.skipped} 个，失败 ${result.errors.length} 个`)
        } else {
          message.success(`导入成功：${result.imported} 个提供商，跳过 ${result.skipped} 个已存在`)
        }
        load()
      } catch (e: unknown) {
        message.error('导入失败：' + (e instanceof Error ? e.message : String(e)))
      } finally {
        setImporting(false)
      }
    }
    input.click()
  }

  const handleBatchDelete = async () => {
    if (selectedProviders.length === 0) {
      message.warning('请先选择要删除的提供商')
      return
    }
    try {
      await Promise.all(selectedProviders.map(id => api('DELETE', `/api/providers/${id}`)))
      message.success(`已删除 ${selectedProviders.length} 个提供商`)
      setSelectedProviders([])
      load()
    } catch (e: unknown) {
      message.error('批量删除失败：' + (e instanceof Error ? e.message : String(e)))
    }
  }

  const provider = providers.find((p) => p.id === selected) ?? null
  const providerKeys = keys.filter((k) => k.provider_id === selected)
  const providerModels = models.filter((m) => m.provider_id === selected)

  return (
    <div style={{ display: 'flex', gap: 16, alignItems: 'flex-start' }}>
      <div style={{ width: 280, flexShrink: 0 }}>
        <Card
          title="提供商"
          size="small"
          extra={
            <Space size={4}>
              <Dropdown
                menu={{
                  items: [
                    { key: 'export', label: '导出配置', icon: <DownloadOutlined /> },
                    { key: 'import', label: '导入配置', icon: <UploadOutlined />, disabled: importing },
                    { key: 'divider', type: 'divider' },
                    { key: 'batch-delete', label: '批量删除', icon: <DeleteOutlined />, danger: true, disabled: selectedProviders.length === 0 },
                  ],
                  onClick: ({ key }) => {
                    if (key === 'export') handleExport()
                    else if (key === 'import') handleImport()
                    else if (key === 'batch-delete') {
                      Modal.confirm({
                        title: '确认批量删除',
                        content: `将删除 ${selectedProviders.length} 个提供商及其关联的密钥和模型，此操作不可恢复。`,
                        okText: '确认删除',
                        okType: 'danger',
                        onOk: handleBatchDelete,
                      })
                    }
                  }
                }}
              >
                <Button size="small" icon={<MoreOutlined />} />
              </Dropdown>
              <ProviderFormButton providers={providers} onSaved={() => load()} />
            </Space>
          }
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
                  selected={selectedProviders.includes(p.id)}
                  onSelect={(checked) => {
                    if (checked) {
                      setSelectedProviders([...selectedProviders, p.id])
                    } else {
                      setSelectedProviders(selectedProviders.filter(id => id !== p.id))
                    }
                  }}
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

function ProviderCardItem({ p, active, modelCount, keyCount, onClick, onSaved, onDeleted, selected, onSelect }: {
  p: Provider
  active: boolean
  modelCount: number
  keyCount: number
  onClick: () => void
  onSaved: () => void
  onDeleted: () => void
  selected: boolean
  onSelect: (checked: boolean) => void
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
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : String(e))
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
          <Checkbox 
            checked={selected} 
            onClick={(e) => e.stopPropagation()} 
            onChange={(e) => onSelect(e.target.checked)}
          />
          <CloudServerOutlined />
          <b>{p.name}</b>
        </Space>
        <Space onClick={(e) => e.stopPropagation()}>
          <a onClick={() => { form.setFieldsValue(p); setOpen(true) }}>编辑</a>
          <Popconfirm
            title="删除将级联清理密钥/模型，确认？"
            onConfirm={async () => {
              try { 
                await api('DELETE', `/api/providers/${p.id}`)
                message.success('已删除')
                onDeleted() 
              } catch (e: unknown) { 
                message.error(e instanceof Error ? e.message : String(e))
              }
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
          <Form.Item
            name="base_url"
            label="Base URL"
            rules={[
              { required: true, message: '请输入 Base URL' },
              { pattern: /^https?:\/\/.+/, message: '必须以 http:// 或 https:// 开头' },
            ]}
          >
            <Input />
          </Form.Item>
          <Form.Item
            name="proxy_url"
            label={proxyURLLabel}
            extra="可选，支持 http://、https:// 或 socks5://，账号密码直接写在 URL 中"
            rules={[{ pattern: /^(https?|socks5):\/\/\S+$/, message: '请输入有效的代理 URL' }]}
          >
            <Input placeholder="http://user:pass@127.0.0.1:7890" />
          </Form.Item>
          <Form.Item name="timeout_ms" label="首字响应超时(ms)"><InputNumber min={1000} style={{ width: '100%' }} /></Form.Item>
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
          <Form.Item
            name="base_url"
            label="Base URL"
            rules={[
              { required: true, message: '请输入 Base URL' },
              { pattern: /^https?:\/\/.+/, message: '必须以 http:// 或 https:// 开头' },
            ]}
          >
            <Input placeholder="https://open.bigmodel.cn/api/paas/v4" />
          </Form.Item>
          <Form.Item
            name="proxy_url"
            label={proxyURLLabel}
            extra="可选，支持 http://、https:// 或 socks5://，账号密码直接写在 URL 中"
            rules={[{ pattern: /^(https?|socks5):\/\/\S+$/, message: '请输入有效的代理 URL' }]}
          >
            <Input placeholder="http://user:pass@127.0.0.1:7890" />
          </Form.Item>
          <Form.Item name="timeout_ms" label="首字响应超时(ms)" initialValue={120000}>
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
  const [testTargets, setTestTargets] = useState<TestTarget[] | null>(null)
  const [fetchingModels, setFetchingModels] = useState(false)
  const [availableModels, setAvailableModels] = useState<string[]>([])
  const [form] = Form.useForm()

  const handleFetchModels = async () => {
    if (keys.length === 0) {
      message.warning('该提供商暂无密钥，至少需要一个密钥才能获取模型列表')
      return
    }
    setFetchingModels(true)
    setAvailableModels([])
    try {
      const res = await api<{ models: { id: string }[] }>('POST', '/api/providers/fetch-models', {
        base_url: provider.base_url,
        api_key: keys[0].key_value,
        proxy_url: provider.proxy_url,
      })
      setAvailableModels(res.models.map((m) => m.id))
      if (res.models.length === 0) message.info('提供商返回了空模型列表')
    } catch (e: any) {
      message.error(e.message || '获取模型列表失败')
    } finally {
      setFetchingModels(false)
    }
  }

  const testOne = (m: Model) => setTestTargets([{ id: m.id, name: m.name }])

  const runBatchTest = () => setTestTargets(models.map((m) => ({ id: m.id, name: m.name })))

  const openForm = (m?: Model) => {
    setEditing(m ?? null)
    setAvailableModels([]) // 清空可用模型列表，避免干扰
    if (m) {
      form.setFieldsValue({
        name: m.name, type: m.type || 'chat', protocol: m.protocol,
        input_price: m.input_price, output_price: m.output_price,
        price_currency: m.price_currency || 'USD', key_ids: m.key_ids,
      })
    } else {
      form.resetFields()
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
        <Button onClick={runBatchTest} disabled={models.length === 0}>测试全部模型</Button>
      </Space>
      <Table<Model> rowKey="id" dataSource={models} tableLayout="fixed" size="small" scroll={{ x: true }}>
        <Table.Column title="模型名" dataIndex="name" width={160} />
        <Table.Column title="类型" dataIndex="type" width={110} render={(v) => modelTypeTag(v || 'chat')} />
        <Table.Column title="协议" dataIndex="protocol" width={200} render={(v) => (
          <span className="mono" style={{ fontSize: 12, color: '#4d4d4d' }}>{v}</span>
        )} />
        <Table.Column title="绑定密钥" dataIndex="key_ids" render={(ids: number[]) =>
          (ids ?? []).map((id) => {
    const k = keys.find((x) => x.id === id)
    return <Tag key={id}>{k ? (k.name || k.key_value) : `#${id}`}</Tag>
  })
        } />
        <Table.Column title="输入/输出价(1M)" width={160} render={(_, m: Model) => {
          const sym = m.price_currency === 'CNY' ? '¥' : '$'
          return `${sym}${m.input_price} / ${sym}${m.output_price}`
        }} />
        <Table.Column title="状态" width={110} render={(_, m: Model) => modelStatusTag(m)} />
        <Table.Column title="连续失败" dataIndex="fail_count" width={80} />
        <Table.Column title="操作" width={240} render={(_, m: Model) => (
          <Space>
            <Button size="small" onClick={() => testOne(m)}>测试</Button>
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
            <div style={{ display: 'flex', gap: 8, alignItems: 'flex-start', flexWrap: 'wrap' }}>
              <Input placeholder="如 glm-4.6 / claude-sonnet-4" style={{ flex: 1, minWidth: 200 }} />
              <Tooltip title={keys.length === 0 ? '需要至少一个密钥' : `从 ${provider.base_url}/v1/models 获取可用模型列表`}>
                <Button
                  icon={fetchingModels ? undefined : <ApiOutlined />}
                  loading={fetchingModels}
                  onClick={handleFetchModels}
                  disabled={keys.length === 0}
                >
                  {fetchingModels ? '获取中…' : '获取模型'}
                </Button>
              </Tooltip>
            </div>
          </Form.Item>
          {availableModels.length > 0 && (
            <Form.Item label="可用模型">
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
                {availableModels.map((m) => (
                  <Button
                    key={m}
                    size="small"
                    type="dashed"
                    onClick={() => form.setFieldValue('name', m)}
                    style={{ fontFamily: 'monospace', fontSize: 12 }}
                  >
                    {m}
                  </Button>
                ))}
              </div>
            </Form.Item>
          )}
          <Form.Item name="type" label="端点类型" initialValue="chat" rules={[{ required: true }]}
            extra="决定该模型挂在哪个代理端点下；路由会按类型过滤后端">
            <Select
              options={modelTypeOptions}
              onChange={(v) => {
                if (v !== 'chat') form.setFieldValue('protocol', 'openai')
              }}
            />
          </Form.Item>
          <Form.Item noStyle shouldUpdate={(prev, cur) => prev.type !== cur.type}>
            {({ getFieldValue }) => {
              const type = getFieldValue('type') as string | undefined
              if (type && type !== 'chat') {
                const isRerank = type === 'rerank'
                return (
                  <Form.Item name="protocol" label="出站格式（由端点类型决定）"
                    extra={isRerank
                      ? '重排无官方标准，直通 Cohere /v1/rerank 骨架，不做跨厂商改写'
                      : '向量走 OpenAI /v1/embeddings 标准格式直通'}>
                    <Select disabled options={[{
                      value: 'openai',
                      label: isRerank ? 'Cohere rerank（/v1/rerank）' : 'OpenAI embeddings（/v1/embeddings）',
                    }]} />
                  </Form.Item>
                )
              }
              return (
                <Form.Item name="protocol" label="上游协议" initialValue="openai" rules={[{ required: true }]}
                  extra="决定出站请求格式与响应转换方式">
                  <Select options={protocolOptions} />
                </Form.Item>
              )
            }}
          </Form.Item>
          <Form.Item label="价格（每 1M token）">
            <Space>
              <Form.Item name="input_price" initialValue={0} noStyle>
                <InputNumber min={0} placeholder="输入价" style={{ width: 160 }} />
              </Form.Item>
              <Form.Item name="output_price" initialValue={0} noStyle>
                <InputNumber min={0} placeholder="输出价" style={{ width: 160 }} />
              </Form.Item>
              <Form.Item name="price_currency" initialValue="USD" noStyle>
                <Select
                  style={{ width: 120 }}
                  options={[
                    { value: 'USD', label: '$ 美元' },
                    { value: 'CNY', label: '¥ 人民币' },
                  ]}
                />
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

      <ModelTestModal
        open={!!testTargets}
        targets={testTargets ?? []}
        onClose={() => setTestTargets(null)}
        onKeysChanged={() => onSaved()}
      />
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
