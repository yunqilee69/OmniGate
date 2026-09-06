import { useEffect, useState } from 'react'
import {
  Button, Form, Input, InputNumber, Modal, Popconfirm, Select, Space, Table, Tag,
  Tooltip, Typography, message, Switch,
} from 'antd'
import { PlusOutlined, DeleteOutlined, EyeOutlined, ReloadOutlined } from '@ant-design/icons'
import { api } from '../api'

interface VirtualKey {
  id: number
  key_value: string
  name: string
  status: string
  rpm_limit: number
  total_budget: number
  used_usd: number
  allowed_routes: string[]
  created_at: number
  updated_at: string
  total_requests: number
  last_used_at: number
}

interface Route { id: number; name: string }

export default function VirtualKeys() {
  const [rows, setRows] = useState<VirtualKey[]>([])
  const [routes, setRoutes] = useState<Route[]>([])
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<VirtualKey | null>(null)
  const [form] = Form.useForm()

  const load = async () => {
    const [vks, rts] = await Promise.all([
      api('GET', '/api/virtual-keys'),
      api('GET', '/api/routes'),
    ])
    setRows(vks)
    setRoutes(rts)
  }
  useEffect(() => { load() }, [])

  const openForm = (vk?: VirtualKey) => {
    setEditing(vk || null)
    if (vk) {
      form.setFieldsValue({
        name: vk.name,
        rpm_limit: vk.rpm_limit || 0,
        total_budget: vk.total_budget || 0,
        allowed_routes: vk.allowed_routes || [],
      })
    } else {
      form.resetFields()
    }
    setOpen(true)
  }

  const submit = async () => {
    const vals = await form.validateFields()
    try {
      const payload = {
        name: vals.name,
        rpm_limit: vals.rpm_limit || 0,
        total_budget: vals.total_budget || 0,
        allowed_routes: vals.allowed_routes || [],
      }

      if (editing) {
        await api('PUT', `/api/virtual-keys/${editing.id}`, payload)
        message.success('更新成功')
      } else {
        const created = await api('POST', '/api/virtual-keys', payload)
        message.success('创建成功')
        Modal.info({
          title: '虚拟密钥已创建',
          content: (
            <div>
              <p>请妥善保存以下密钥，它只会显示一次：</p>
              <Typography.Paragraph
                copyable
                code
                style={{ background: '#f5f5f5', padding: 8, marginTop: 8 }}
              >
                {created.key_value}
              </Typography.Paragraph>
            </div>
          ),
        })
      }
      setOpen(false)
      load()
    } catch (e: any) {
      message.error(e.message || '操作失败')
    }
  }

  const del = async (id: number) => {
    try {
      await api('DELETE', `/api/virtual-keys/${id}`)
      message.success('删除成功')
      load()
    } catch (e: any) {
      message.error(e.message || '删除失败')
    }
  }

  const toggleStatus = async (vk: VirtualKey) => {
    try {
      const newStatus = vk.status === 'active' ? 'disabled' : 'active'
      await api('PUT', `/api/virtual-keys/${vk.id}`, { status: newStatus })
      message.success(newStatus === 'active' ? '已启用' : '已禁用')
      load()
    } catch (e: any) {
      message.error(e.message || '操作失败')
    }
  }


  const revealKey = async (id: number) => {
    try {
      const data = await api('GET', `/api/virtual-keys/${id}/reveal-key`)
      Modal.info({
        title: '完整密钥',
        content: (
          <Typography.Paragraph copyable code>
            {data.key_value}
          </Typography.Paragraph>
        ),
      })
    } catch (e: any) {
      message.error(e.message || '获取失败')
    }
  }

  const cols = [
    { title: 'ID', dataIndex: 'id', width: 60 },
    {
      title: '名称',
      dataIndex: 'name',
      render: (t: string) => <Typography.Text strong>{t}</Typography.Text>,
    },
    {
      title: '密钥',
      dataIndex: 'key_value',
      render: (key: string, rec: VirtualKey) => (
        <Space>
          <Typography.Text code copyable={{ text: key }}>
            {key}
          </Typography.Text>
          <Tooltip title="查看完整密钥">
            <Button
              size="small"
              icon={<EyeOutlined />}
              onClick={() => revealKey(rec.id)}
            />
          </Tooltip>
        </Space>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 120,
      render: (s: string, rec: VirtualKey) => (
        <Space>
          <Tag color={s === 'active' ? 'green' : 'default'}>
            {s === 'active' ? '启用' : '禁用'}
          </Tag>
          <Switch
            size="small"
            checked={s === 'active'}
            onChange={() => toggleStatus(rec)}
          />
        </Space>
      ),
    },
    {
      title: 'RPM 限制',
      dataIndex: 'rpm_limit',
      render: (rpm: number) => (
        rpm > 0 ? <span>{rpm} 请求/分钟</span> : <span style={{ color: '#999' }}>无限制</span>
      ),
    },
    {
      title: '总限额',
      render: (_: any, rec: VirtualKey) => (
        <Space direction="vertical" size={0}>
          <div>
            {rec.total_budget > 0 ? `$${rec.total_budget.toFixed(2)}` : <span style={{ color: '#999' }}>无限制</span>}
          </div>
          {rec.total_budget > 0 && (
            <div style={{ fontSize: 11, color: '#666' }}>
              已用: ${rec.used_usd.toFixed(2)}
            </div>
          )}
        </Space>
      ),
    },
    {
      title: '使用统计',
      render: (_: any, rec: VirtualKey) => (
        <div style={{ fontSize: 12 }}>
          <div>总请求: {rec.total_requests || 0}</div>
        </div>
      ),
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      width: 160,
      render: (ts: number) => new Date(ts * 1000).toLocaleString('zh-CN', {
        year: 'numeric', 
        month: '2-digit', 
        day: '2-digit', 
        hour: '2-digit', 
        minute: '2-digit' 
      }),
    },
    {
      title: '最近使用',
      dataIndex: 'last_used_at',
      width: 160,
      render: (ts: number) => ts > 0 ? new Date(ts * 1000).toLocaleString('zh-CN', { 
        year: 'numeric', 
        month: '2-digit', 
        day: '2-digit', 
        hour: '2-digit', 
        minute: '2-digit' 
      }) : <span style={{ color: '#999' }}>从未使用</span>,
    },
    {
      title: '允许路由',
      dataIndex: 'allowed_routes',
      render: (routeNames: string[]) => {
        if (!routeNames || routeNames.length === 0) {
          return <span style={{ color: '#999' }}>全部</span>
        }
        return (
          <Space size={[0, 4]} wrap>
            {routeNames.slice(0, 3).map(n => <Tag key={n}>{n}</Tag>)}
            {routeNames.length > 3 && <Tag>+{routeNames.length - 3}</Tag>}
          </Space>
        )
      },
    },
    {
      title: '操作',
      width: 120,
      render: (_: any, rec: VirtualKey) => (
        <Space>
          <Button size="small" onClick={() => openForm(rec)}>编辑</Button>
          <Popconfirm title="确认删除?" onConfirm={() => del(rec.id)}>
            <Button size="small" danger icon={<DeleteOutlined />} />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', justifyContent: 'space-between' }}>
        <Typography.Title level={4}>虚拟密钥</Typography.Title>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={load}>刷新</Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={() => openForm()}>
            新建
          </Button>
        </Space>
      </div>

      <Table
        size="small"
        dataSource={rows}
        columns={cols}
        rowKey="id"
        pagination={{ pageSize: 20, showSizeChanger: true }}
      />

      <Modal
        title={editing ? '编辑虚拟密钥' : '新建虚拟密钥'}
        open={open}
        onOk={submit}
        onCancel={() => setOpen(false)}
        width={600}
      >
        <Form form={form} layout="vertical" style={{ marginTop: 16 }}>
          <Form.Item
            label="名称"
            name="name"
            rules={[{ required: true, message: '请输入名称' }]}
          >
            <Input placeholder="例如: 团队A-生产环境" />
          </Form.Item>

          <Form.Item label="RPM 限制" name="rpm_limit">
            <InputNumber min={0} placeholder="0 = 无限制" style={{ width: '100%' }} />
          </Form.Item>

          <Form.Item label="总限额 (USD)" name="total_budget">
            <InputNumber min={0} step={0.01} placeholder="0 = 无限制" style={{ width: '100%' }} />
          </Form.Item>

          <Form.Item label="允许的路由" name="allowed_routes">
            <Select
              mode="multiple"
              placeholder="留空则允许全部路由"
              options={routes.map(r => ({ label: r.name, value: r.name }))}
            />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
