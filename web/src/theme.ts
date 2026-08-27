import type { ThemeConfig } from 'antd'

// Vercel Geist 主题 → AntD token（数值见 web/DESIGN.md，勿在此文件外散装覆盖）
// 语义映射：primary=ink #171717（黑）；success/active=link #0070f3；warning/error 用语义色
export const geistTheme: ThemeConfig = {
  token: {
    colorPrimary: '#171717',
    colorInfo: '#0070f3',
    colorSuccess: '#0070f3',
    colorWarning: '#f5a623',
    colorError: '#ee0000',
    colorLink: '#0070f3',
    colorLinkHover: '#0761d1',
    colorTextBase: '#171717',
    colorTextHeading: '#171717',
    colorText: '#4d4d4d',
    colorTextSecondary: '#8f8f8f',
    colorTextTertiary: '#a1a1a1',
    colorBgLayout: '#fafafa',
    colorBgContainer: '#ffffff',
    colorBgElevated: '#ffffff',
    colorBorder: '#ebebeb',
    colorBorderSecondary: '#f2f2f2',
    borderRadius: 6,
    borderRadiusLG: 12,
    fontFamily:
      "'Geist Sans', 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Arial, sans-serif",
    fontFamilyCode: "'Geist Mono', ui-monospace, SFMono-Regular, Menlo, monospace",
    fontSize: 14,
    controlHeight: 36,
    wireframe: false,
  },
  components: {
    Button: {
      borderRadius: 6,
      borderRadiusSM: 6,
      primaryShadow: 'none',
      defaultShadow: 'none',
      dangerShadow: 'none',
      fontWeight: 500,
    },
    Card: {
      paddingLG: 24,
      borderRadiusLG: 12,
    },
    Table: {
      headerBg: '#fafafa',
      headerColor: '#8f8f8f',
      rowHoverBg: '#fafafa',
      borderColor: '#ebebeb',
      headerSplitColor: 'transparent',
    },
    Layout: { headerBg: '#fafafa', headerHeight: 56, bodyBg: '#fafafa' },
    Menu: {
      itemSelectedBg: 'transparent',
      itemSelectedColor: '#171717',
      horizontalItemSelectedColor: '#171717',
      activeBarBorderWidth: 0,
    },
    Tabs: {
      titleFontSize: 14,
      inkBarColor: '#171717',
      itemSelectedColor: '#171717',
    },
    Input: { borderRadius: 6 },
    // 选中项用墨底白字（默认由 #171717 主色推导出 #575757 灰底，与 #4d4d4d 文字对比度仅 ~1.1:1，不可读）
    Select: {
      borderRadius: 6,
      optionSelectedBg: '#171717',
      optionSelectedColor: '#ffffff',
      optionSelectedFontWeight: 500,
    },
    Modal: { borderRadiusLG: 12 },
    Drawer: { borderRadiusLG: 12 },
    Tooltip: { borderRadius: 6 },
  },
}
