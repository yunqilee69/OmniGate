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

interface Route {
  id: number
  name: string
  remark: string
  targets: Target[]
}

interface Model { id: number; name: string; provider_id: number }
interface Provider { id: number; name: string }

export default function RoutesPage() {
  const [rows, setRows] = useState<Route[]>([])
  const [models, setModels] = useState<Model[]>([])
  const [providers, setProviders] = useState<Provider[]>([])
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Route | null>(null)
  const [exampleRoute, setExampleRoute] = useState<Route | null>(null)
  const [form] = Form.useForm()

  const load = async () => {
    try {
      const [rs, ms, ps] = await Promise.all([
        api('GET', '/api/routes'), api('GET', '/api/models'), api('GET', '/api/providers'),
      ])
      setRows(rs)
      setModels(ms)
      setProviders(ps)
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
        remark: r.remark,
        targets: r.targets.map((t) => ({ model_id: t.model_id, weight: t.weight })),
      })
    }
    setOpen(true)
  }

  const submit = async () => {
    const values = await form.validateFields()
    const payload = {
      ...values,
      targets: (values.targets ?? []).map((t: any) => ({
        model_id: Number(t.model_id),
        weight: Number(t.weight),
      })),
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
        expandedRowRender: (r) => (
          <Table<Target> rowKey="id" dataSource={r.targets} pagination={false} size="small">
            <Table.Column title="目标模型" render={(_, t: Target) => `${t.provider_name} / ${t.model_name}`} />
            <Table.Column title="权重" dataIndex="weight" width={80} />
            <Table.Column title="流量占比" width={200} render={(_, t: Target) => (
              <Progress percent={targetPercent(t, r.targets)} size="small" />
            )} />
          </Table>
        ),
      }}>
        <Table.Column title="ID" dataIndex="id" width={60} />
        <Table.Column title="逻辑 modelId" dataIndex="name" render={(v) => <code>{v}</code>} />
        <Table.Column title="目标数" render={(_, r: Route) => r.targets.length} width={80} />
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

      <Modal title={editing ? '编辑路由' : '新增路由'} open={open} onOk={submit} onCancel={() => setOpen(false)} destroyOnClose width={640}>
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="逻辑 modelId（客户端请求时填写）" rules={[{ required: true }]}>
            <Input placeholder="如 glm-pool" />
          </Form.Item>
          <Form.Item name="remark" label="备注"><Input /></Form.Item>
          <Form.Item label="目标模型与权重">
            <Form.List name="targets">
              {(fields, { add, remove }) => (
                <>
                  {fields.map((field) => (
                    <Space key={field.key} align="baseline" style={{ display: 'flex', marginBottom: 8 }}>
                      <Form.Item name={[field.name, 'model_id']} rules={[{ required: true, message: '选择模型' }]} noStyle>
                        <Select
                          placeholder="选择目标模型"
                          style={{ width: 320 }}
                          options={models.map((m) => ({ value: m.id, label: modelName(m.id) }))}
                          showSearch
                          optionFilterProp="label"
                        />
                      </Form.Item>
                      <Form.Item name={[field.name, 'weight']} initialValue={1} noStyle>
                        <InputNumber min={1} placeholder="权重" style={{ width: 110 }} />
                      </Form.Item>
                      <DeleteOutlined onClick={() => remove(field.name)} />
                    </Space>
                  ))}
                  <Button type="dashed" block icon={<PlusOutlined />} onClick={() => add({ weight: 1 })}>
                    添加目标模型
                  </Button>
                </>
              )}
            </Form.List>
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

function buildCurl(base: string, model: string): string {
  return [
    `curl ${base}/v1/chat/completions \\`,
    `  -H 'Content-Type: application/json' \\`,
    `  -d '{`,
    `    "model": "${model}",`,
    `    "messages": [{"role": "user", "content": "你好"}]`,
    `  }'`,
  ].join('\n')
}

function buildCurlStream(base: string, model: string): string {
  return [
    `curl -N ${base}/v1/chat/completions \\`,
    `  -H 'Content-Type: application/json' \\`,
    `  -d '{`,
    `    "model": "${model}",`,
    `    "stream": true,`,
    `    "messages": [{"role": "user", "content": "你好"}]`,
    `  }'`,
  ].join('\n')
}

function buildPython(base: string, model: string): string {
  return [
    `from openai import OpenAI`,
    ``,
    `client = OpenAI(base_url="${base}/v1", api_key="unused")`,
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
  return (
    <Modal
      title={`请求示例 — ${route.name}`}
      open={!!route}
      onCancel={onClose}
      footer={null}
      width={640}
      destroyOnClose
    >
      <div style={{ marginBottom: 16 }}>
        <div className="eyebrow" style={{ marginBottom: 4 }}>endpoint</div>
        <Typography.Paragraph copyable={{ text: `${base}/v1/chat/completions` }} style={{ marginBottom: 0 }}>
          <code>{base}/v1/chat/completions</code>
        </Typography.Paragraph>
        <div className="meta-text" style={{ marginTop: 4 }}>
          代理面无需鉴权；把 model 换成任意逻辑 modelId 即可直接调用
        </div>
      </div>
      <Tabs
        items={[
          { key: 'curl', label: 'curl', children: <CodeBlock code={buildCurl(base, route.name)} /> },
          { key: 'curl-stream', label: 'curl 流式', children: <CodeBlock code={buildCurlStream(base, route.name)} /> },
          { key: 'python', label: 'Python SDK', children: <CodeBlock code={buildPython(base, route.name)} /> },
        ]}
      />
    </Modal>
  )
}
