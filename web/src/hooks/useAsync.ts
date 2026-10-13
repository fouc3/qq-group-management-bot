import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, toApiError } from '../api/client'

/** 极简数据获取状态：loading / error / data / refresh */
export interface AsyncState<T> {
  data: T | null
  error: ApiError | null
  loading: boolean
  refresh: () => void
}

interface Snapshot<T> {
  data: T | null
  error: ApiError | null
  loading: boolean
}

/**
 * 跑一次 loader 并维护状态。
 * deps 变化或 refresh() 时重新请求；组件卸载后不再 setState。
 * 刷新过程中保留上一次的 data，避免页面闪烁成空态。
 */
export function useAsync<T>(loader: () => Promise<T>, deps: readonly unknown[]): AsyncState<T> {
  const [snapshot, setSnapshot] = useState<Snapshot<T>>({ data: null, error: null, loading: true })
  const [nonce, setNonce] = useState(0)

  const loaderRef = useRef(loader)
  useEffect(() => {
    loaderRef.current = loader
  })

  useEffect(() => {
    let alive = true
    setSnapshot((prev) => ({ data: prev.data, error: null, loading: true }))
    loaderRef.current().then(
      (data) => {
        if (alive) {
          setSnapshot({ data, error: null, loading: false })
        }
      },
      (cause: unknown) => {
        if (alive) {
          setSnapshot((prev) => ({ data: prev.data, error: toApiError(cause), loading: false }))
        }
      },
    )
    return () => {
      alive = false
    }
    // deps 由调用方保证长度固定
  }, [...deps, nonce])

  const refresh = useCallback(() => {
    setNonce((n) => n + 1)
  }, [])

  return { data: snapshot.data, error: snapshot.error, loading: snapshot.loading, refresh }
}

/** 取一组状态里第一个错误，用于视图顶部统一的错误提示 */
export function firstError(...states: readonly AsyncState<unknown>[]): ApiError | null {
  for (const state of states) {
    if (state.error) {
      return state.error
    }
  }
  return null
}

/** 取一组状态里是否还有任何请求在跑 */
export function isAnyLoading(...states: readonly AsyncState<unknown>[]): boolean {
  return states.some((state) => state.loading)
}

/** 依次刷新多个状态（用于「刷新」按钮） */
export function refreshAll(...states: readonly AsyncState<unknown>[]): void {
  states.forEach((state) => {
    state.refresh()
  })
}
