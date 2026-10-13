import { useEffect, useState } from 'react'

/**
 * 「刚跳到这里」的高亮：亮一段时间再灭。
 *
 * 不做成 CSS 动画，因为那种一闪而过的高亮在截图里几乎看不见，人也不一定抓得住；
 * 保持几秒的底色 + 一圈内描边更容易被看到，而且它自己会消失，不会看起来像被选中。
 */
export function useFocusBlink(domId: string | undefined, seq: number | undefined, ms = 4000): string {
  const [blink, setBlink] = useState('')
  useEffect(() => {
    if (domId === undefined || domId === '') {
      return
    }
    setBlink(domId)
    const timer = window.setTimeout(() => {
      setBlink('')
    }, ms)
    return () => {
      window.clearTimeout(timer)
    }
  }, [domId, seq, ms])
  return blink
}
