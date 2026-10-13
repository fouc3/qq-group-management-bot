import { useEffect } from 'react'

/**
 * 顶栏搜索跳过来之后的定位：把 DOM id 对应的元素滚进视野。
 *
 * 滚三次而不是一次，是因为切页和取数是同时发生的：第一次滚动时那一行往往还没渲染
 * 出来（列表还在 loading），两次补滚跨过「数据到了 → 行渲染出来」这一拍。这也是它
 * 不把 rows 放进依赖的原因 —— 那样每次自动刷新都会把人拽回去一次。
 */
export function useFocusScroll(domId: string | undefined, seq: number | undefined): void {
  useEffect(() => {
    if (domId === undefined || domId === '') {
      return
    }
    const scroll = (): void => {
      document.getElementById(domId)?.scrollIntoView({ block: 'center', behavior: 'smooth' })
    }
    scroll()
    const first = window.setTimeout(scroll, 150)
    const second = window.setTimeout(scroll, 450)
    return () => {
      window.clearTimeout(first)
      window.clearTimeout(second)
    }
  }, [domId, seq])
}
