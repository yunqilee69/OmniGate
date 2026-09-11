import { useEffect, useState } from 'react'
import { Button, Form, Input, InputNumber, Modal, Popconfirm, Select, Space, Table, message } from 'antd'
import { PlusOutlined } from '@ant-design/icons'
import { api } from '../api'

interface MCPBackend {
  id: number
  name: string
  target_url: string
  api_key: string
  timeout_ms: number
  status: string
  remark: string
  created_at: number
  updated_at: number
}

export default function McpBackendsPage() {
  const [rows, setRows] = useState<MCPBackend[]>([])
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<MCPBackend | null>(null)
  const [form] = Form.useForm()

  const load = async () => {
    try {
      const data = await api('GET', '/api/mcp-backends')
      setRows(data)
    } catch (e: any) {
      message.error(e.message)
    }
  }

  useEffect(() => { load() }, [])

  const openForm = (b?: MCPBackend) => {
    setEditing(b ?? null)
    form.resetFields()
    if (b) {
      form.setFieldsValue({
        name: b.name,
        target_url: b.target_url,
        api_key: b.api_key,
        timeout_ms: b.timeout_ms,
        status: b.status,
        remark: b.remark,
      })
    }
    setOpen(true)
  }

  const submit = async () => {
    const values = await form.validateFields()
    try {
      if (editing) {
        await api('PUT', `/api/mcp-backends/${editing.id}`, values)
      } else {
        await api('POST', '/api/mcp-backends', values)
      }
      message.success('已保存')
      setOpen(false)
      load()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  const deleteBackend = async (id: number) => {
    try {
      await api('DELETE', `/api/mcp-backends/${id}`)
      message.success('已删除')
      load()
    } catch (e: any) {
      message.error(e.message)
    }
  }

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <h2 style={{ margin: 0 }}>MCP</h2>
        <Button type="primary" icon={<PlusOutlined />} onClick={() => openForm()}>新增 MCP</Button>
      </div>

      <Table<MCPBackend> rowKey="id" dataSource={rows} pagination={{ pageSize: 20 }}>
        <Table.Column title="ID" dataIndex="id" width={60} />
        <Table.Column title="名称" dataIndex="name" render={(v) => <code>{v}</code>} />
        <Table.Column title="目标 URL" dataIndex="target_url" ellipsis />
        <Table.Column title="超时(ms)" dataIndex="timeout_ms" width={100} />
        <Table.Column title="状态" dataIndex="status" width={80} render={(v) => (
          <span style={{ color: v === 'active' ? '#52c41a' : '#999' }}>{v === 'active' ? '启用' : '禁用'}</span>
        )} />
        <Table.Column title="备注" dataIndex="remark" ellipsis />
        <Table.Column title="操作" width={150} render={(_, b: MCPBackend) => (
          <Space>
            <Button size="small" onClick={() => openForm(b)}>编辑</Button>
            <Popconfirm title="确认删除该后端？" onConfirm={() => deleteBackend(b.id)}>
              <Button size="small" danger>删除</Button>
            </Popconfirm>
          </Space>
        )} />
      </Table>

      <Modal
        title={editing ? '编辑 MCP' : '新增 MCP'}
        open={open}
        onOk={submit}
        onCancel={() => setOpen(false)}
        destroyOnHidden
        width={600}
      >
        <Form form={form} layout="vertical" style={{ marginTop: 16 }}>
          <Form.Item
            name="name"
            label="名称"
            rules={[{ required: true, message: '请输入名称' }]}
            extra="唯一标识，不能包含双下划线(__)"
          >
            <Input placeholder="如 filesystem" />
          </Form.Item>
          <Form.Item
            name="target_url"
            label="目标 URL"
            rules={[{ required: true, message: '请输入目标 URL' }]}
            extra="MCP Server 的 Streamable HTTP 端点"
          >
            <Input placeholder="https://mcp-server.example.com" />
          </Form.Item>
          <Form.Item
            name="api_key"
            label="API Key"
            extra="如果后端需要鉴权，填写 Bearer token"
          >
            <Input.Password placeholder="可选" />
          </Form.Item>
          <Form.Item
            name="timeout_ms"
            label="超时(毫秒)"
            initialValue={30000}
            rules={[{ required: true, message: '请输入超时时间' }]}
          >
            <InputNumber min={1000} max={300000} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="status" label="状态" initialValue="active">
            <Select options={[
              { value: 'active', label: '启用' },
              { value: 'disabled', label: '禁用' },
            ]} />
          </Form.Item>
          <Form.Item name="remark" label="备注">
            <Input.TextArea rows={2} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
