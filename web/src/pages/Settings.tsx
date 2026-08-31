import { useEffect, useState } from 'react'
import { Alert, Button, Card, Form, Input, InputNumber, Modal, Select, Switch, Tooltip, message } from 'antd'
import { QuestionCircleOutlined } from '@ant-design/icons'
import { api } from '../api'

type Settings = Record<string, any>
type Model = { id: number; name: string; type: string; protocol: string; provider_id: number; status: string }

const fmtCounts = (m: any) =>
  Object.entries(m ?? {})
    .map(([k, v]) => `${k} ${v} 条`)
    .join('，')

const LADDER_PRESETS = ['10s', '30s', '1m', '3m', '5m', '15m', '30m']

const HelpIcon = ({ tip }: { tip: string }) => (
  <Tooltip title={tip}>
    <QuestionCircleOutlined style={{ marginLeft: 4, color: '#8f8f8f', cursor: 'help' }} />
  </Tooltip>
)

const numericRanges: Record<string, [number, number]> = {
  'breaker.disable_threshold': [1, 100],
  'breaker.max_hops': [1, 10],
  'ratelimit.key_cooldown_s': [1, 86400],
  'stream.idle_timeout_s': [1, 86400],
  'capture.retention_days': [1, 365],
  'log.retention_days': [0, 3650],
  'affinity.ttl_s': [10, 86400],
  'fallback.model_id': [0, 9999999],
}

export default function Settings() {
  const [settings, setSettings] = useState<Settings | null>(null)
  const [models, setModels] = useState<Model[]>([])
  const [form] = Form.useForm()
  const captureOn = Form.useWatch('capture.enabled', form)
  const fallbackOn = Form.useWatch('fallback.enabled', form)

  const load = async () => {
    try {
      const [s, m] = await Promise.all([
        api('GET', '/api/settings'),
        api<Model[]>('GET', '/api/models')
      ])
      setSettings(s)
      setModels(m)
      form.setFieldsValue(s)
    } catch (e: any) {
      message.error(e.message)
    }
  }
  useEffect(() => { load() }, [])

  const save = async () => {
    const values = await form.validateFields()
    const payload: Settings = {}
    for (const [k, v] of Object.entries(values)) {
      if (v !== undefined && v !== null) payload[k] = v
    }
    try {
      const updated = await api('PUT', '/api/settings', payload)
      setSettings(updated)
      message.success('已保存，即时生效')
    } catch (e: any) {
      message.error(e.message)
    }
  }

  const confirmCleanup = () => {
    Modal.confirm({
      title: '确认立即清理过期数据？',
      content: '将按当前保留天数立即删除过期的请求日志、尝试日志、统计与内容日志，不等下一轮自动清理。该操作不可恢复。',
      okText: '立即清理',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        try {
          const r = await api('POST', '/api/maintenance/cleanup')
          message.success(`已清理：${fmtCounts(r.deleted) || '无过期数据'}`)
        } catch (e: any) {
          message.error(e.message)
        }
      },
    })
  }

  const confirmClearStats = () => {
    Modal.confirm({
      title: '确认清空全部统计数据？',
      content: '将删除全部请求日志、尝试日志与每日统计（内容日志保留）。此操作不可恢复。',
      okText: '清空统计',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        try {
          const r = await api('POST', '/api/maintenance/clear-stats', { confirm: true })
          message.success(`已清空：${fmtCounts(r.cleared)}`)
        } catch (e: any) {
          message.error(e.message)
        }
      },
    })
  }

  if (!settings) return null

  return (
    <>
    <Card title="运行配置（保存即热生效）" style={{ maxWidth: 720, margin: '0 auto' }}>
      <Form form={form} layout="vertical">
        <Form.Item label={<span>熔断阶梯（按顺序升级，末档重复）</span>} name="breaker.cooldown_ladder">
          <Select mode="tags" placeholder="时长" tokenSeparators={[',']} options={LADDER_PRESETS.map((v) => ({ value: v, label: v }))} />
        </Form.Item>
        <Form.Item
          label={<span>禁用阈值（连续失败次数）<HelpIcon tip="默认 3：失败1→30s，失败2→1m，失败3→禁用（需手动解禁或改阈值让第3档生效）" /></span>}
          name="breaker.disable_threshold"
        >
          <InputNumber min={numericRanges['breaker.disable_threshold'][0]} max={numericRanges['breaker.disable_threshold'][1]} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label={<span>单请求最大转移次数</span>} name="breaker.max_hops">
          <InputNumber min={1} max={10} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label={<span>429 缺省冷却秒数（有 Retry-After 时优先）</span>} name="ratelimit.key_cooldown_s">
          <InputNumber min={1} max={86400} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label={<span>流式空闲超时秒数</span>} name="stream.idle_timeout_s">
          <InputNumber min={1} max={86400} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label={<span>自动注入 include_usage 获取精确 token</span>} name="stream.inject_usage" valuePropName="checked">
          <Switch />
        </Form.Item>

        <Form.Item label={<span>内容捕获（默认关闭，仅元数据落库）</span>} name="capture.enabled" valuePropName="checked">
          <Switch />
        </Form.Item>
        {captureOn && (
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 16 }}
            message="开启后将记录请求/响应全文到本地 SQLite（content_log 表），注意敏感数据暴露风险"
          />
        )}
        <Form.Item label={<span>捕获路由白名单（空 = 全部路由）</span>} name="capture.routes">
          <Select mode="tags" placeholder="逻辑 modelId" tokenSeparators={[',']} />
        </Form.Item>
        <Form.Item label={<span>内容日志保留天数</span>} name="capture.retention_days">
          <InputNumber min={1} max={365} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label={<span>请求日志保留天数（0 = 永久）</span>} name="log.retention_days">
          <InputNumber min={0} max={3650} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item
          label={<span>会话亲和（同会话粘住上次成功的模型，最大化上游缓存命中）<HelpIcon tip="粘住同模型同 key，保证会话内请求路由到相同后端" /></span>}
          name="affinity.enabled"
          valuePropName="checked"
        >
          <Switch />
        </Form.Item>
        <Form.Item label={<span>会话 ID 请求头（未传时按消息前缀哈希自动识别会话）</span>} name="affinity.header">
          <Input placeholder="X-Session-ID" />
        </Form.Item>
        <Form.Item label={<span>会话亲和记忆时长（秒）</span>} name="affinity.ttl_s">
          <InputNumber min={10} max={86400} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item
          label={<span>美元兑人民币汇率（1 USD = ? CNY）<HelpIcon tip="人民币定价的模型按此汇率折算为美元计费入库；仪表盘/统计页可切换展示币种" /></span>}
          name="pricing.usd_cny"
        >
          <InputNumber min={0.01} max={10000} step={0.01} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item
          label={<span>启用兜底模型<HelpIcon tip="当路由配置的所有模型失败时，自动使用兜底模型（单次尝试，不重试）" /></span>}
          name="fallback.enabled"
          valuePropName="checked"
        >
          <Switch />
        </Form.Item>
        {fallbackOn && (
          <>
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 16 }}
              message="兜底模型是最后的保险，建议选择稳定且成本较低的模型（如 gpt-3.5-turbo）"
            />
            <Form.Item
              label={<span>兜底模型<HelpIcon tip="当所有配置模型失败后，将使用此模型（需确保模型状态为 active 且有可用 key）" /></span>}
              name="fallback.model_id"
            >
              <Select
                showSearch
                placeholder="选择兜底模型"
                optionFilterProp="children"
                filterOption={(input, option) =>
                  (option?.label ?? '').toLowerCase().includes(input.toLowerCase())
                }
                options={models
                  .filter(m => m.status === 'active')
                  .map(m => ({
                    value: m.id,
                    label: `${m.name} (${m.type} / ${m.protocol})`,
                  }))}
              />
            </Form.Item>
          </>
        )}

        <Button type="primary" onClick={save}>保存</Button>
      </Form>
    </Card>
    <Card title="危险操作" style={{ maxWidth: 720, marginTop: 16, margin: '16px auto 0' }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 16 }}>
          <div style={{ flex: 1 }}>
            <div>立即清理过期数据</div>
            <div style={{ color: '#8f8f8f', fontSize: 12 }}>
              按保留天数立即删除过期的请求日志 / 尝试日志 / 统计 / 内容日志，不等下一轮自动清理
            </div>
          </div>
          <Button danger ghost onClick={confirmCleanup}>立即清理</Button>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 16 }}>
          <div style={{ flex: 1 }}>
            <div>清空统计数据</div>
            <div style={{ color: '#8f8f8f', fontSize: 12 }}>删除全部请求日志与每日统计（内容日志保留），不可恢复</div>
          </div>
          <Button danger onClick={confirmClearStats}>清空统计</Button>
        </div>
      </div>
    </Card>
    </>
  )
}
