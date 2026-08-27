import { Layout, Menu } from 'antd'
import {
  DashboardOutlined,
  DatabaseOutlined,
  BranchesOutlined,
  BarChartOutlined,
  FileTextOutlined,
  SettingOutlined,
} from '@ant-design/icons'
import { Routes, Route, Navigate, useLocation, Link } from 'react-router-dom'
import Dashboard from './pages/Dashboard'
import ConfigCenter from './pages/ConfigCenter'
import RoutesPage from './pages/Routes'
import Stats from './pages/Stats'
import Logs from './pages/Logs'
import LogDetail from './pages/LogDetail'
import Settings from './pages/Settings'

const items = [
  { key: '/dashboard', icon: <DashboardOutlined />, label: <Link to="/dashboard">仪表盘</Link> },
  { key: '/config', icon: <DatabaseOutlined />, label: <Link to="/config">配置中心</Link> },
  { key: '/routes', icon: <BranchesOutlined />, label: <Link to="/routes">路由</Link> },
  { key: '/stats', icon: <BarChartOutlined />, label: <Link to="/stats">统计</Link> },
  { key: '/logs', icon: <FileTextOutlined />, label: <Link to="/logs">请求日志</Link> },
  { key: '/settings', icon: <SettingOutlined />, label: <Link to="/settings">设置</Link> },
]

export default function App() {
  const loc = useLocation()
  const selected = '/' + (loc.pathname.split('/')[1] || 'dashboard')
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
      </Layout.Header>
      <Layout.Content style={{ padding: 24 }}>
        <Routes>
          <Route path="/" element={<Navigate to="/dashboard" replace />} />
          <Route path="/dashboard" element={<Dashboard />} />
          <Route path="/config" element={<ConfigCenter />} />
          <Route path="/routes" element={<RoutesPage />} />
          <Route path="/stats" element={<Stats />} />
          <Route path="/logs" element={<Logs />} />
          <Route path="/logs/:request_id" element={<LogDetail />} />
          <Route path="/settings" element={<Settings />} />
        </Routes>
      </Layout.Content>
    </Layout>
  )
}
