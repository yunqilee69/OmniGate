import { useEffect, useState } from 'react'
import {
  Button, Form, Input, InputNumber, Modal, Popconfirm, Progress, Select, Space, Table,
  Tabs, Tooltip, Typography, message,
} from 'antd'
import { PlusOutlined, DeleteOutlined, CodeOutlined } from '@ant-design/icons'
import { api } from '../api'

interface Target {
  id: number
  model_id: number
  model_name: string
  provider_name: string
  weight: number
}

interface McpTarget {
  id: number
  mcp_backend_id: number
  backend_name: string
  target_url: string
  status: string
}

interface Route {
  id: number
  name: string
  endpoint: string
  remark: string
  targets: Target[]
  mcp_targets: McpTarget[]
}

interface Model { id: number; name: string; provider_id: number; protocol: string; type: string }
interface Provider { id: number; name: string }
interface MCPBackend { id: number; name: string; target_url: string; status: string }

export default function RoutesPage() {
  const [rows, setRows] = useState<Route[]>([])
  const [models, setModels] = useState<Model[]>([])
  const [providers, setProviders] = useState<Provider[]>([])
  const [mcpBackends, setMcpBackends] = useState<MCPBackend[]>([])
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Route | null>(null)
  const [exampleRoute, setExampleRoute] = useState<Route | null>(null)
  const [form] = Form.useForm()

  const load = async () => {
    try {
      const [rs, ms, ps, mbs] = await Promise.all([
        api('GET', '/api/routes'),
        api('GET', '/api/models'),
        api('GET', '/api/providers'),
        api('GET', '/api/mcp-backends'),
      ])
      setRows(rs)
      setModels(ms)
      setProviders(ps)
      setMcpBackends(mbs)
    } catch (e: any) {
      message.error(e.message)
    }
  }
  useEffect(() => { load() }, [])

  const openForm = (r?: Route) => {
    setEditing(r ?? null)
    form.resetFields()
    if (r) {
      form.setFieldsValue({
        name: r.name,
        endpoint: r.endpoint,
        remark: r.remark,
        targets: r.targets?.map((t) => ({ model_id: t.model_id, weight: t.weight })) || [],
        mcp_targets: r.mcp_targets?.map((t) => ({ mcp_backend_id: t.mcp_backend_id })) || [],
      })
    }
    setOpen(true)
  }

  const submit = async () => {
    const values = await form.validateFields()
    const endpoint = values.endpoint || 'completions'
    const payload: any = {
      name: values.name,
      endpoint: values.endpoint,
      remark: values.remark,
    }
    if (endpoint === 'mcp') {
      payload.mcp_targets = (values.mcp_targets ?? []).map((t: any) => ({
        mcp_backend_id: Number(t.mcp_backend_id),
      }))
    } else {
      payload.targets = (values.targets ?? []).map((t: any) => ({
        model_id: Number(t.model_id),
        weight: Number(t.weight),
      }))
    }
    try {
      if (editing) {
        await api('PUT', `/api/routes/${editing.id}`, payload)
      } else {
        await api('POST', '/api/routes', payload)
      }
      message.success('已保存（即时生效）')
      setOpen(false)
      load()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  const modelName = (id: number) => {
    const m = models.find((x) => x.id === id)
    if (!m) return `#${id}`
    const p = providers.find((x) => x.id === m.provider_id)
    return `${p?.name ?? ''} / ${m.name}`
  }

  const targetPercent = (t: Target, all: Target[]) => {
    const sum = all.reduce((s, x) => s + x.weight, 0)
    return sum > 0 ? Math.round((t.weight / sum) * 1000) / 10 : 0
  }

  return (
    <div>
      <Button type="primary" onClick={() => openForm()} style={{ marginBottom: 16 }}>新增路由</Button>
      <Table<Route> rowKey="id" dataSource={rows} expandable={{
        expandedRowRender: (r) => {
          if (r.endpoint === 'mcp') {
            return (
              <Table<McpTarget> rowKey="id" dataSource={r.mcp_targets} pagination={false} size="small">
                <Table.Column title="MCP" dataIndex="backend_name" />
                <Table.Column title="目标 URL" dataIndex="target_url" ellipsis />
                <Table.Column title="状态" dataIndex="status" width={80} render={(v) => (
                  <span style={{ color: v === 'active' ? '#52c41a' : '#999' }}>{v === 'active' ? '启用' : '禁用'}</span>
                )} />
              </Table>
            )
          }
          return (
            <Table<Target> rowKey="id" dataSource={r.targets} pagination={false} size="small">
              <Table.Column title="目标模型" render={(_, t: Target) => `${t.provider_name} / ${t.model_name}`} />
              <Table.Column title="权重" dataIndex="weight" width={80} />
              <Table.Column title="流量占比" width={200} render={(_, t: Target) => (
                <Progress percent={targetPercent(t, r.targets)} size="small" />
              )} />
            </Table>
          )
        },
      }}>
        <Table.Column title="ID" dataIndex="id" width={60} />
        <Table.Column title="逻辑 modelId" dataIndex="name" render={(v) => <code>{v}</code>} />
        <Table.Column title="端点类型" dataIndex="endpoint" width={120} />
        <Table.Column title="目标数" render={(_, r: Route) => r.endpoint === 'mcp' ? r.mcp_targets?.length || 0 : r.targets?.length || 0} width={80} />
        <Table.Column title="备注" dataIndex="remark" ellipsis />
        <Table.Column title="操作" width={230} render={(_, r: Route) => (
          <Space>
            <Tooltip title="获取请求命令，改模型名即可直接调用">
              <Button size="small" icon={<CodeOutlined />} onClick={() => setExampleRoute(r)}>请求示例</Button>
            </Tooltip>
            <Button size="small" onClick={() => openForm(r)}>编辑</Button>
            <Popconfirm title="确认删除该路由？" onConfirm={async () => {
              try { await api('DELETE', `/api/routes/${r.id}`); load() } catch (e: any) { message.error(e.message) }
            }}>
              <Button size="small" danger>删除</Button>
            </Popconfirm>
          </Space>
        )} />
      </Table>

      <RequestExample route={exampleRoute} onClose={() => setExampleRoute(null)} />

      <Modal title={editing ? '编辑路由' : '新增路由'} open={open} onOk={submit} onCancel={() => setOpen(false)} destroyOnHidden width={640}>
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="逻辑 modelId（客户端请求时填写）" rules={[{ required: true }]}>
            <Input placeholder="如 glm" />
          </Form.Item>
          <Form.Item 
            name="endpoint" 
            label="端点类型" 
            initialValue="completions" 
            rules={[{ required: true }]}
            extra="决定代理路径与协议：completions/messages/responses用于LLM，embedding/rerank/image用于专用模型，mcp用于MCP工具聚合"
          >
            <Select
              options={[
                { value: 'completions', label: 'completions — /v1/chat/completions（OpenAI）' },
                { value: 'messages', label: 'messages — /v1/messages（Anthropic）' },
                { value: 'responses', label: 'responses — /v1/responses（OpenAI）' },
                { value: 'embedding', label: 'embedding — /v1/embeddings（向量化）' },
                { value: 'rerank', label: 'rerank — /v1/rerank（重排）' },
                { value: 'image', label: 'image — /v1/images/generations（生图）' },
                { value: 'mcp', label: 'mcp — /v1/mcp/{route_name}（MCP 工具聚合）' },
              ]}
            />
          </Form.Item>
          <Form.Item name="remark" label="备注"><Input /></Form.Item>
          <Form.Item noStyle shouldUpdate={(prev, cur) => prev.endpoint !== cur.endpoint}>
            {({ getFieldValue }) => {
              const endpoint = getFieldValue('endpoint') || 'completions'
              if (endpoint === 'mcp') {
                return (
                  <Form.Item label="MCP">
                    <Form.List name="mcp_targets">
                      {(fields, { add, remove }) => (
                        <>
                          {fields.map((field) => (
                            <Space key={field.key} align="baseline" style={{ display: 'flex', marginBottom: 8 }}>
                              <Form.Item name={[field.name, 'mcp_backend_id']} rules={[{ required: true, message: '选择后端' }]} noStyle>
                                <Select
                                  placeholder="选择 MCP"
                                  style={{ width: 450 }}
                                  options={mcpBackends.filter((b) => b.status === 'active').map((b) => ({ 
                                    value: b.id, 
                                    label: `${b.name} — ${b.target_url}` 
                                  }))}
                                  showSearch
                                  optionFilterProp="label"
                                />
                              </Form.Item>
                              <DeleteOutlined onClick={() => remove(field.name)} />
                            </Space>
                          ))}
                          <Button 
                            type="dashed" 
                            block 
                            icon={<PlusOutlined />} 
                            onClick={() => add()}
                            disabled={mcpBackends.filter((b) => b.status === 'active').length === 0}
                          >
                            添加 MCP{mcpBackends.filter((b) => b.status === 'active').length === 0 ? '（无可用后端，请先在 MCP 页面创建）' : ''}
                          </Button>
                        </>
                      )}
                    </Form.List>
                  </Form.Item>
                )
              }
              return (
                <Form.Item label="目标模型与权重">
                  <Form.List name="targets">
                    {(fields, { add, remove }) => (
                      <Form.Item noStyle shouldUpdate={(prev, cur) => prev.endpoint !== cur.endpoint || prev.targets !== cur.targets}>
                        {({ getFieldValue }) => {
                          const endpoint = getFieldValue('endpoint') || 'completions'
                          const requiredProtocol = endpoint === 'messages' ? 'messages' : endpoint === 'responses' ? 'responses' : 'completions'
                          const targets = getFieldValue('targets') || []
                          const selectedModelIds = new Set(targets.map((t: any) => t?.model_id).filter(Boolean))
                          const filteredModels = models.filter((m) => {
                            if (m.protocol !== requiredProtocol) return false
                            if (endpoint === 'embedding') return m.type === 'embedding'
                            if (endpoint === 'rerank') return m.type === 'rerank'
                            if (endpoint === 'image') return m.type === 'image'
                            return m.type === 'chat'
                          })
                          
                          return (
                            <>
                              {fields.map((field) => {
                                const currentModelId = getFieldValue(['targets', field.name, 'model_id'])
                                const availableForThisField = filteredModels.filter(
                                  (m) => m.id === currentModelId || !selectedModelIds.has(m.id)
                                )
                                
                                return (
                                  <Space key={field.key} align="baseline" style={{ display: 'flex', marginBottom: 8 }}>
                                    <Form.Item name={[field.name, 'model_id']} rules={[{ required: true, message: '选择模型' }]} noStyle>
                                      <Select
                                        placeholder="选择目标模型"
                                        style={{ width: 320 }}
                                        options={availableForThisField.map((m) => ({ value: m.id, label: modelName(m.id) }))}
                                        showSearch
                                        optionFilterProp="label"
                                      />
                                    </Form.Item>
                                    <Form.Item name={[field.name, 'weight']} initialValue={1} noStyle>
                                      <InputNumber min={1} placeholder="权重" style={{ width: 110 }} />
                                    </Form.Item>
                                    <DeleteOutlined onClick={() => remove(field.name)} />
                                  </Space>
                                )
                              })}
                              <Button 
                                type="dashed" 
                                block 
                                icon={<PlusOutlined />} 
                                onClick={() => add({ weight: 1 })}
                                disabled={filteredModels.length === 0 || selectedModelIds.size >= filteredModels.length}
                              >
                                添加目标模型{filteredModels.length === 0 ? `（无可用的 ${requiredProtocol} 协议模型）` : selectedModelIds.size >= filteredModels.length ? '（所有模型已选择）' : ''}
                              </Button>
                            </>
                          )
                        }}
                      </Form.Item>
                    )}
                  </Form.List>
                </Form.Item>
              )
            }}
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

function endpointPath(ep: string): string {
  return ep === 'messages' ? '/v1/messages' : ep === 'responses' ? '/v1/responses' : ep === 'embedding' ? '/v1/embeddings' : ep === 'rerank' ? '/v1/rerank' : ep === 'image' ? '/v1/images/generations' : '/v1/chat/completions'
}

const VK_KEY = 'vk-你的虚拟密钥'

// 按端点生成示例请求体行（bash 多行美观格式）
function curlBodyLines(model: string, endpoint: string, stream: boolean): string[] {
  const streamLine = stream ? [`    "stream": true,`] : []
  if (endpoint === 'messages') {
    return [
      `    "model": "${model}",`,
      `    "max_tokens": 1024,`,
      ...streamLine,
      `    "messages": [{"role": "user", "content": "你好"}]`,
    ]
  }
  if (endpoint === 'responses') {
    return [
      `    "model": "${model}",`,
      ...streamLine,
      `    "input": "你好"`,
    ]
  }
  return [
    `    "model": "${model}",`,
    ...streamLine,
    `    "messages": [{"role": "user", "content": "你好"}]`,
  ]
}

// 按端点生成单行 JSON（Windows cmd 用，双引号转义）
function curlBodyJson(model: string, endpoint: string, stream: boolean): string {
  const body: Record<string, unknown> = { model }
  if (stream) body.stream = true
  if (endpoint === 'messages') {
    body.max_tokens = 1024
    body.messages = [{ role: 'user', content: '你好' }]
  } else if (endpoint === 'responses') {
    body.input = '你好'
  } else {
    body.messages = [{ role: 'user', content: '你好' }]
  }
  return JSON.stringify(body).replaceAll('"', '\\"')
}

function buildCurl(base: string, model: string, endpoint: string, stream = false): string {
  const path = endpointPath(endpoint)
  const lines = [
    `curl ${stream ? '-N ' : ''}${base}${path} \\`,
    `  -H 'Content-Type: application/json' \\`,
    `  -H 'Authorization: Bearer ${VK_KEY}' \\`,
  ]
  if (endpoint === 'messages') lines.push(`  -H 'anthropic-version: 2023-06-01' \\`)
  lines.push(`  -d '{`, ...curlBodyLines(model, endpoint, stream), `  }'`)
  return lines.join('\n')
}

function buildCurlCmd(base: string, model: string, endpoint: string, stream = false): string {
  const path = endpointPath(endpoint)
  const lines = [
    `rem 中文内容需 UTF-8 编码：先执行 chcp 65001`,
    `curl ${stream ? '-N ' : ''}${base}${path} ^`,
    `  -H "Content-Type: application/json" ^`,
    `  -H "Authorization: Bearer ${VK_KEY}" ^`,
  ]
  if (endpoint === 'messages') lines.push(`  -H "anthropic-version: 2023-06-01" ^`)
  lines.push(`  -d "${curlBodyJson(model, endpoint, stream)}"`)
  return lines.join('\n')
}

function buildPython(base: string, model: string, endpoint: string): string {
  if (endpoint === 'messages') {
    return [
      `from anthropic import Anthropic`,
      ``,
      `# auth_token 生成 Authorization: Bearer 头，供网关虚拟密钥鉴权`,
      `client = Anthropic(base_url="${base}/v1", auth_token="${VK_KEY}")`,
      ``,
      `resp = client.messages.create(`,
      `    model="${model}",`,
      `    max_tokens=1024,`,
      `    messages=[{"role": "user", "content": "你好"}],`,
      `)`,
      `print(resp.content[0].text)`,
    ].join('\n')
  }
  if (endpoint === 'responses') {
    return [
      `from openai import OpenAI`,
      ``,
      `client = OpenAI(base_url="${base}/v1", api_key="${VK_KEY}")`,
      ``,
      `resp = client.responses.create(`,
      `    model="${model}",`,
      `    input="你好",`,
      `)`,
      `print(resp.output_text)`,
    ].join('\n')
  }
  return [
    `from openai import OpenAI`,
    ``,
    `client = OpenAI(base_url="${base}/v1", api_key="${VK_KEY}")`,
    ``,
    `resp = client.chat.completions.create(`,
    `    model="${model}",`,
    `    messages=[{"role": "user", "content": "你好"}],`,
    `)`,
    `print(resp.choices[0].message.content)`,
  ].join('\n')
}

function CodeBlock({ code }: { code: string }) {
  return (
    <div style={{ position: 'relative' }}>
      <Button
        size="small"
        style={{ position: 'absolute', top: 8, right: 8, zIndex: 1 }}
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(code)
            message.success('已复制')
          } catch {
            message.error('复制失败，请手动选择复制')
          }
        }}
      >
        复制
      </Button>
      <pre
        className="mono"
        style={{
          margin: 0,
          background: '#ffffff',
          border: '1px solid #ebebeb',
          borderRadius: 12,
          padding: 16,
          paddingRight: 72,
          overflow: 'auto',
          fontSize: 12,
          lineHeight: '20px',
          color: '#171717',
        }}
      >
        {code}
      </pre>
    </div>
  )
}

function RequestExample({ route, onClose }: { route: Route | null; onClose: () => void }) {
  if (!route) return null
  const base = window.location.origin
  const ep = route.endpoint || 'completions'
  const epPath = endpointPath(ep)
  return (
    <Modal
      title={`请求示例 — ${route.name}`}
      open={!!route}
      onCancel={onClose}
      footer={null}
      width={640}
      destroyOnHidden
    >
      <div style={{ marginBottom: 16 }}>
        <div className="eyebrow" style={{ marginBottom: 4 }}>endpoint</div>
        <Typography.Paragraph copyable={{ text: `${base}${epPath}` }} style={{ marginBottom: 0 }}>
          <code>{base}{epPath}</code>
        </Typography.Paragraph>
        <div className="meta-text" style={{ marginTop: 4 }}>
          代理面通过虚拟密钥鉴权：把 vk-你的虚拟密钥 替换为「虚拟密钥」页创建的 key；model 填路由名
        </div>
      </div>
      <Tabs
        items={[
          { key: 'curl', label: 'curl', children: <CodeBlock code={buildCurl(base, route.name, ep)} /> },
          { key: 'curl-win', label: 'curl (Windows)', children: <CodeBlock code={buildCurlCmd(base, route.name, ep)} /> },
          { key: 'curl-stream', label: 'curl 流式', children: <CodeBlock code={buildCurl(base, route.name, ep, true)} /> },
          { key: 'curl-stream-win', label: 'curl 流式 (Windows)', children: <CodeBlock code={buildCurlCmd(base, route.name, ep, true)} /> },
          { key: 'python', label: 'Python SDK', children: <CodeBlock code={buildPython(base, route.name, ep)} /> },
        ]}
      />
    </Modal>
  )
}
