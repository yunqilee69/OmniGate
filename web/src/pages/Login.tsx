import { useEffect, useState } from 'react'
import { Button, Card, Input, message } from 'antd'
import { api, setToken } from '../api'

type Mode = 'checking' | 'password' | 'open'

export default function Login() {
  const [mode, setMode] = useState<Mode>('checking')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    api<{ mode: Mode }>('GET', '/api/auth-info')
      .then((r) => {
        if (r.mode === 'open') {
          window.location.href = '/dashboard'
          return
        }
        setMode(r.mode)
      })
      .catch(() => setMode('password'))
  }, [])

  const submit = async () => {
    setLoading(true)
    try {
      const res = await api<{ token: string }>('POST', '/api/login', {
        username: username.trim(),
        password,
      })
      setToken(res.token)
      message.success('登录成功')
      // 整页跳转让 App 认证门重新校验,不依赖组件内状态传递
      window.location.href = '/dashboard'
    } catch (e: any) {
      message.error(`登录失败:${e.message}`)
    } finally {
      setLoading(false)
    }
  }

  if (mode === 'checking' || mode === 'open') {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center' }} />
    )
  }

  const form = (
    <>
      <Input
        placeholder="账号"
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        size="large"
        autoFocus
      />
      <Input.Password
        placeholder="密码"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        onPressEnter={submit}
        size="large"
        style={{ marginTop: 12 }}
      />
      <div style={{ color: '#8f8f8f', fontSize: 12, marginTop: 8 }}>
        API 调用:Authorization: Basic base64(账号:密码),或 api_key 填编码串/原文/config 中的 api_key
      </div>
    </>
  )

  return (
    <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: '#fafafa' }}>
      <Card style={{ width: 380 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 24 }}>
          <div style={{ width: 10, height: 10, borderRadius: 2, background: '#171717' }} />
          <span className="page-title" style={{ fontSize: 18 }}>OmniGate</span>
        </div>
        {form}
        <Button type="primary" block size="large" style={{ marginTop: 16 }} loading={loading} onClick={submit}>
          登录
        </Button>
      </Card>
    </div>
  )
}
