import { useEffect, useState } from 'react'
import { Breadcrumb, Button, Card, Col, Descriptions, Row, Space, Spin, Table, Tag, Tooltip, Typography, message } from 'antd'
import { ArrowLeftOutlined } from '@ant-design/icons'
import { useNavigate, useParams, Link } from 'react-router-dom'
import dayjs from 'dayjs'
import { api } from '../api'
import StatusTag from '../components/StatusTag'

interface LogRow {
  id: number
  request_id: string
  route: string
  model: string
  provider: string
  key_id: number
  status: string
  error_code: string
  error_body?: string
  prompt_tokens: number
  completion_tokens: number
  tokens_estimated: boolean
  ttft_ms: number
  total_ms: number
  cost: number
  retries: number
  created_at: number
}

interface Attempt {
  id: number
  attempt: number
  model: string
  provider: string
  key_id: number
  status: string
  http_status: number
  error_code: string
  error_body?: string
  latency_ms: number
  ttft_ms: number
  prompt_tokens: number
  completion_tokens: number
  created_at: number
}

interface DetailResp {
  log: LogRow
  key_value_masked: string
  key_name?: string
  attempts: Attempt[]
}

export default function LogDetail() {
  const { request_id } = useParams<{ request_id: string }>()
  const nav = useNavigate()
  const [data, setData] = useState<DetailResp | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [content, setContent] = useState<string | null>(null)

  useEffect(() => {
    if (!request_id) return
    setData(null); setErr(null); setContent(null)
    api<DetailResp>('GET', `/api/logs/${request_id}`)
      .then((d) => setData(d))
      .catch((e) => setErr(e.message))
    api<{ request_body: string; response_body: string }>('GET', `/api/logs/${request_id}/content`)
      .then((c) => setContent(`请求体:\n${c.request_body}\n\n响应体:\n${c.response_body}`))
      .catch(() => setContent(null))
  }, [request_id])

  if (err) {
    return (
      <div>
        <Link to="/logs">← 返回请求日志</Link>
        <Card style={{ marginTop: 16 }}>{err}</Card>
      </div>
    )
  }
  if (!data) {
    return <div style={{ padding: 40, textAlign: 'center' }}><Spin /></div>
  }

  const { log, key_value_masked, key_name, attempts } = data

  return (
    <div>
      <Row align="middle" justify="space-between" style={{ marginBottom: 16 }}>
        <Col>
          <Space size="middle">
            <Button icon={<ArrowLeftOutlined />} onClick={() => nav('/logs')}>返回</Button>
            <Breadcrumb items={[
              { title: <Link to="/logs">请求日志</Link> },
              { title: <code style={{ fontSize: 13 }}>{log.request_id}</code> },
            ]} />
          </Space>
        </Col>
        <Col><StatusTag tone={log.status === 'success' ? 'ok' : log.status === 'error' ? 'error' : 'mute'}>{log.status}</StatusTag></Col>
      </Row>

      <Card title="请求详情" style={{ marginBottom: 16 }}>
        <Descriptions column={{ xs: 1, sm: 2, md: 3 }} size="small">
          <Descriptions.Item label="路由"><code>{log.route}</code></Descriptions.Item>
          <Descriptions.Item label="提供商/模型">{log.provider ? `${log.provider}/${log.model}` : log.model}</Descriptions.Item>
          <Descriptions.Item label="密钥">
            <code style={{ fontSize: 12, color: '#8f8f8f' }}>{key_name || key_value_masked || (log.key_id ? `#${log.key_id}` : '-')}</code>
          </Descriptions.Item>
          <Descriptions.Item label="错误码">{log.error_code || '-'}</Descriptions.Item>
          <Descriptions.Item label="Tokens">
            ↑ {log.prompt_tokens}{log.tokens_estimated ? '（估算）' : ''} / ↓ {log.completion_tokens}{log.tokens_estimated ? '（估算）' : ''}
          </Descriptions.Item>
          <Descriptions.Item label="首字响应">{log.ttft_ms} ms</Descriptions.Item>
          <Descriptions.Item label="总耗时">{log.total_ms} ms</Descriptions.Item>
          <Descriptions.Item label="费用">{log.cost.toFixed(6)}</Descriptions.Item>
          <Descriptions.Item label="重试转移">{log.retries}</Descriptions.Item>
          <Descriptions.Item label="时间" span={3}>{dayjs(log.created_at * 1000).format('YYYY-MM-DD HH:mm:ss')}</Descriptions.Item>
        </Descriptions>
      </Card>

      {log.error_body ? (
        <Card title="上游错误响应（无条件记录，与内容捕获开关无关）" style={{ marginBottom: 16 }}>
          <pre style={{
            whiteSpace: 'pre-wrap', background: '#fff5f5', border: '1px solid #fbd5d5',
            padding: 12, borderRadius: 8, maxHeight: 320, overflow: 'auto', fontSize: 12, color: '#171717', margin: 0,
          }}>{log.error_body}</pre>
        </Card>
      ) : null}

      {attempts.length > 1 ? (
        <Card title={`尝试链路（共 ${attempts.length} 次，含重试 ${log.retries} 次）`} style={{ marginBottom: 16 }}>
          <Table<Attempt>
            rowKey="id"
            dataSource={attempts}
            size="small"
            pagination={false}
            tableLayout="fixed"
          >
            <Table.Column title="#" dataIndex="attempt" width={60} />
            <Table.Column title="模型" dataIndex="model" width={200} ellipsis
              render={(_, a) => `${a.provider}/${a.model}`} />
            <Table.Column title="密钥" dataIndex="key_id" width={160}
              render={(keyId) => <span className="mono">#{keyId}</span>} />
            <Table.Column title="状态" dataIndex="status" width={90} render={statusTag} />
            <Table.Column title="HTTP" dataIndex="http_status" width={70} />
            <Table.Column title="错误码" dataIndex="error_code" width={90} />
            <Table.Column title="首字/耗时" width={120} render={(_, a) => `${a.ttft_ms}/${a.latency_ms}ms`} />
            <Table.Column
              title="Tokens"
              width={110}
              render={(_, a) => `↑ ${a.prompt_tokens} / ↓ ${a.completion_tokens}`}
            />
            <Table.Column
              title="错误体"
              dataIndex="error_body"
              render={(v) => v ? (
                <Tooltip title={v}><span style={{ color: '#8f8f8f' }}>{v.slice(0, 60)}{v.length > 60 ? '…' : ''}</span></Tooltip>
              ) : <span style={{ color: '#8f8f8f' }}>-</span>}
            />
          </Table>
        </Card>
      ) : null}

      <Card title="请求/响应内容捕获">
        {content ? (
          <pre style={{
            whiteSpace: 'pre-wrap', background: '#ffffff', border: '1px solid #ebebeb',
            padding: 16, borderRadius: 12, maxHeight: 480, overflow: 'auto', fontSize: 12, margin: 0,
          }}>{content}</pre>
        ) : (
          <Typography.Text type="secondary">内容捕获未开启或该请求无捕获内容</Typography.Text>
        )}
      </Card>
    </div>
  )
}

function statusTag(s: string) {
  if (s === 'success') return <StatusTag tone="ok">成功</StatusTag>
  if (s === 'client_error') return <StatusTag tone="mute">客户端错误</StatusTag>
  if (s === 'cooldown') return <StatusTag tone="warn">冷却</StatusTag>
  return <StatusTag tone="error">错误</StatusTag>
}
