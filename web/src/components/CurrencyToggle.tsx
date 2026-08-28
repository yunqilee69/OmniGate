import { useCallback, useState } from 'react'
import { Segmented } from 'antd'

export type Currency = 'USD' | 'CNY'

const STORAGE_KEY = 'display_currency'

export function loadCurrency(): Currency {
  return localStorage.getItem(STORAGE_KEY) === 'CNY' ? 'CNY' : 'USD'
}

// 展示币种偏好（localStorage 持久化，跨页面共享）
export function useCurrency(): [Currency, (c: Currency) => void] {
  const [currency, setCurrency] = useState<Currency>(loadCurrency)
  const change = useCallback((c: Currency) => {
    localStorage.setItem(STORAGE_KEY, c)
    setCurrency(c)
  }, [])
  return [currency, change]
}

export function CurrencyToggle({ value, onChange }: { value: Currency; onChange: (c: Currency) => void }) {
  return (
    <Segmented
      value={value}
      onChange={(v) => onChange(v as Currency)}
      options={[
        { value: 'USD', label: '$ 美元' },
        { value: 'CNY', label: '¥ 人民币' },
      ]}
    />
  )
}
