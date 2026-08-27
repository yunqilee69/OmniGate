import { useEffect, useState } from 'react'
import { Alert, Button, Card, Form, InputNumber, Select, Switch, message } from 'antd'
import { api } from '../api'

type Settings = Record<string, any>

const LADDER_PRESETS = ['10s', '30s', '1m', '3m', '5m', '15m', '30m']

const numericRanges: Record<string, [number, number]> = {
  'breaker.disable_threshold': [1, 100],
  'breaker.max_hops': [1, 10],
  'ratelimit.key_cooldown_s': [1, 86400],
  'stream.idle_timeout_s': [1, 86400],
  'capture.retention_days': [1, 365],
  'log.retention_days': [0, 3650],
}

export default function Settings() {
  const [settings, setSettings] = useState<Settings | null>(null)
  const [form] = Form.useForm()
  const captureOn = Form.useWatch('capture.enabled', form)

  const load = async () => {
    try {
      const s = await api('GET', '/api/settings')
      setSettings(s)
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

  if (!settings) return null

  return (
    <Card title="运行配置（保存即热生效）" style={{ maxWidth: 720 }}>
      <Form form={form} layout="vertical">
        <Form.Item label="熔断阶梯（按顺序升级，末档重复）" name="breaker.cooldown_ladder">
          <Select mode="tags" placeholder="时长" tokenSeparators={[',']} options={LADDER_PRESETS.map((v) => ({ value: v, label: v }))} />
        </Form.Item>
        <Form.Item label="禁用阈值（连续失败次数）" name="breaker.disable_threshold"
          extra="默认 3：失败1→30s，失败2→1m，失败3→禁用（需手动解禁或改阈值让第3档生效）">
          <InputNumber min={numericRanges['breaker.disable_threshold'][0]} max={numericRanges['breaker.disable_threshold'][1]} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label="单请求最大转移次数" name="breaker.max_hops">
          <InputNumber min={1} max={10} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label="429 缺省冷却秒数（有 Retry-After 时优先）" name="ratelimit.key_cooldown_s">
          <InputNumber min={1} max={86400} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label="流式空闲超时秒数" name="stream.idle_timeout_s">
          <InputNumber min={1} max={86400} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label="自动注入 include_usage 获取精确 token" name="stream.inject_usage" valuePropName="checked">
          <Switch />
        </Form.Item>

        <Form.Item label="内容捕获（默认关闭，仅元数据落库）" name="capture.enabled" valuePropName="checked">
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
        <Form.Item label="捕获路由白名单（空 = 全部路由）" name="capture.routes">
          <Select mode="tags" placeholder="逻辑 modelId" tokenSeparators={[',']} />
        </Form.Item>
        <Form.Item label="内容日志保留天数" name="capture.retention_days">
          <InputNumber min={1} max={365} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item label="请求日志保留天数（0 = 永久）" name="log.retention_days">
          <InputNumber min={0} max={3650} style={{ width: '100%' }} />
        </Form.Item>

        <Button type="primary" onClick={save}>保存</Button>
      </Form>
    </Card>
  )
}
