import { useEffect, useRef } from 'react'
import * as echarts from 'echarts'

export default function Chart({ option, height = 320 }: { option: any; height?: number }) {
  const divRef = useRef<HTMLDivElement>(null)
  const chartRef = useRef<echarts.ECharts>()

  useEffect(() => {
    if (!divRef.current) return
    chartRef.current = echarts.init(divRef.current)
    const onResize = () => chartRef.current?.resize()
    window.addEventListener('resize', onResize)
    return () => {
      window.removeEventListener('resize', onResize)
      chartRef.current?.dispose()
    }
  }, [])

  useEffect(() => {
    chartRef.current?.setOption(option, true)
  }, [option])

  return <div ref={divRef} style={{ height, width: '100%' }} />
}
