import { useEffect, useState } from 'react'
import { Layout, Menu, Spin } from 'antd'
import {
  DashboardOutlined,
  DatabaseOutlined,
  BranchesOutlined,
  BarChartOutlined,
  FileTextOutlined,
  SettingOutlined,
  LogoutOutlined,
  KeyOutlined,
  MessageOutlined,
  ApiOutlined,
} from '@ant-design/icons'
import { Routes, Route, Navigate, useLocation, Link } from 'react-router-dom'
import { api, clearToken, uiPath } from './api'
import McpBackends from './pages/McpBackends'
import Dashboard from './pages/Dashboard'
import ConfigCenter from './pages/ConfigCenter'
import RoutesPage from './pages/Routes'
import VirtualKeys from './pages/VirtualKeys'
import Stats from './pages/Stats'
import Playground from './pages/Playground'
import Logs from './pages/Logs'
import LogDetail from './pages/LogDetail'
import Settings from './pages/Settings'
import Login from './pages/Login'

const items = [
  { key: '/dashboard', icon: <DashboardOutlined />, label: <Link to="/dashboard">仪表盘</Link> },
  { key: '/config', icon: <DatabaseOutlined />, label: <Link to="/config">配置中心</Link> },
  { key: '/routes', icon: <BranchesOutlined />, label: <Link to="/routes">路由</Link> },
  { key: '/keys', icon: <KeyOutlined />, label: <Link to="/keys">虚拟密钥</Link> },
  { key: '/mcp-backends', icon: <ApiOutlined />, label: <Link to="/mcp-backends">MCP</Link> },
  { key: '/playground', icon: <MessageOutlined />, label: <Link to="/playground">模型测试</Link> },
  { key: '/stats', icon: <BarChartOutlined />, label: <Link to="/stats">统计</Link> },
  { key: '/logs', icon: <FileTextOutlined />, label: <Link to="/logs">请求日志</Link> },
  { key: '/settings', icon: <SettingOutlined />, label: <Link to="/settings">设置</Link> },
]

type Gate = 'checking' | 'ok' | 'login'

export default function App() {
  const loc = useLocation()
  const [gate, setGate] = useState<Gate>('checking')
  const [authMode, setAuthMode] = useState<'' | 'open' | 'password'>('')
  const selected = '/' + (loc.pathname.split('/')[1] || 'dashboard')

  useEffect(() => {
    api('GET', '/api/health').then(() => setGate('ok')).catch(() => setGate('login'))
  }, [])

  // 免登录模式（未配置 admin 账号密码）下无会话可注销，隐藏退出入口
  useEffect(() => {
    api<{ mode: 'open' | 'password' }>('GET', '/api/auth-info')
      .then((r) => setAuthMode(r.mode))
      .catch(() => setAuthMode(''))
  }, [])

  if (gate === 'checking') {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <Spin size="large" />
      </div>
    )
  }

  // 登录页独立渲染(无导航壳);已登录访问 /login 直接进仪表盘
  if (loc.pathname === '/login') {
    return gate === 'ok' ? <Navigate to="/dashboard" replace /> : <Login />
  }
  if (gate === 'login') {
    return <Navigate to="/login" replace />
  }

  const logout = () => {
    api('POST', '/api/logout').catch(() => {})
    clearToken()
    window.location.href = uiPath('/login')
  }

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Layout.Header
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 24,
          padding: '0 24px',
          borderBottom: '1px solid #ebebeb',
          position: 'sticky',
          top: 0,
          zIndex: 100,
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexShrink: 0 }}>
          <div style={{ width: 10, height: 10, borderRadius: 2, background: '#171717' }} />
          <span className="page-title">OmniGate</span>
        </div>
        <Menu
          mode="horizontal"
          selectedKeys={[selected]}
          items={items}
          style={{ flex: 1, minWidth: 0, borderBottom: 'none', background: 'transparent' }}
        />
        {authMode === 'password' && (
          <LogoutOutlined
            title="退出登录"
            onClick={logout}
            style={{ fontSize: 16, color: '#4d4d4d', cursor: 'pointer', flexShrink: 0 }}
          />
        )}
      </Layout.Header>
      <Layout.Content style={{ padding: 24 }}>
        <Routes>
          <Route path="/" element={<Navigate to="/dashboard" replace />} />
          <Route path="/login" element={<Login />} />
          <Route path="/dashboard" element={<Dashboard />} />
          <Route path="/config" element={<ConfigCenter />} />
          <Route path="/keys" element={<VirtualKeys />} />
          <Route path="/routes" element={<RoutesPage />} />
          <Route path="/playground" element={<Playground />} />
          <Route path="/stats" element={<Stats />} />
          <Route path="/logs" element={<Logs />} />
          <Route path="/logs/:request_id" element={<LogDetail />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="/mcp-backends" element={<McpBackends />} />
        </Routes>
      </Layout.Content>
    </Layout>
  )
}
