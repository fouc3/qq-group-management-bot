import { useEffect, useState } from 'react'
import { unixNow } from '../lib/format'

/**
 * 每 intervalMs 返回一个新的 Unix 秒。
 *
 * 倒计时需要它，但不能每帧重渲染：一整页表格每秒重画一次是没有必要的开销，所以
 * 调用方自己决定粒度（概览用 30 秒，验证队列用 10 秒）。
 */
export function useTicker(intervalMs: number): number {
  const [now, setNow] = useState(() => unixNow())
  useEffect(() => {
    const timer = window.setInterval(() => {
      setNow(unixNow())
    }, intervalMs)
    return () => {
      window.clearInterval(timer)
    }
  }, [intervalMs])
  return now
}
