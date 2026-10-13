import { useCallback, useState } from 'react'
import { toApiError } from '../api/client'
import type { ApiError } from '../api/client'
import { approveVerification, rejectVerification, resendVerification } from '../api/endpoints'
import type { VerificationActionResponse } from '../api/types'

/** 待验证队列上的三个动作 */
export type VerificationActionName = 'resend' | 'approve' | 'reject'

export interface VerificationActionOptions {
  /** 仅 reject 用：是否同时写进本群黑名单 */
  blacklist?: boolean
  /** 仅 reject 用：黑名单条目的原因 */
  reason?: string
}

export interface VerificationActions {
  /** 正在执行的那个令牌，空串表示没有在跑 */
  busyToken: string
  /** 最近一次成功的结果 */
  result: VerificationActionResponse | null
  error: ApiError | null
  run: (action: VerificationActionName, token: string, options?: VerificationActionOptions) => void
  dismiss: () => void
}

/**
 * 三个动作共用的执行状态。
 *
 * 两个页面（概览、入群验证）渲染同一份队列、同一组按钮，所以状态只能有一份实现：
 * 分开写会出现「概览页的按钮会刷新、验证页的不会」这类只在一边出现的毛病。
 *
 * 动作成功后才回调 onSettled：刷新要在动手之后，不然拿到的是动手前的队列。
 */
export function useVerificationActions(onSettled: () => void): VerificationActions {
  const [busyToken, setBusyToken] = useState('')
  const [result, setResult] = useState<VerificationActionResponse | null>(null)
  const [error, setError] = useState<ApiError | null>(null)

  const run = useCallback(
    (action: VerificationActionName, token: string, options?: VerificationActionOptions) => {
      setBusyToken(token)
      setError(null)
      const call =
        action === 'resend'
          ? resendVerification(token)
          : action === 'approve'
            ? approveVerification(token)
            : rejectVerification(token, {
                blacklist: options?.blacklist ?? false,
                reason: options?.reason ?? '',
              })
      call
        .then((response) => {
          setResult(response)
          onSettled()
        })
        .catch((cause: unknown) => {
          // 404/409 是「这个人已经不在队列里了」，那是答案不是故障：原样显示后端的
          // 那句话，同时刷新一次队列，让页面和后端重新对上。
          setError(toApiError(cause))
          onSettled()
        })
        .finally(() => {
          setBusyToken('')
        })
    },
    [onSettled],
  )

  const dismiss = useCallback(() => {
    setResult(null)
    setError(null)
  }, [])

  return { busyToken, result, error, run, dismiss }
}
