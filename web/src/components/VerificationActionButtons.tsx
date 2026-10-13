import { useState } from 'react'
import type { VerificationActions } from '../hooks/useVerificationActions'
import { Modal } from './Modal'

interface VerificationActionsProps {
  token: string
  /** 这个令牌正在执行某个动作 */
  busy: boolean
  actions: VerificationActions
  /**
   * 这条记录在等本人过浏览器里那个人机验证（设置里 online_verify 开着）。
   *
   * 只影响一句话：重发在这类记录上是「再申请一条链接」，而不是「把群内按钮再发一次」。
   */
  online?: boolean
}

/**
 * 待验证队列上的三个动作按钮。
 *
 * 「拉黑」先开一个确认窗而不是直接动手：它写黑名单、关验证记录，两件事都会改变状态，
 * 而这一列的按钮挨得很近。窗口里可以关掉写黑名单 ——「只是关掉这条待验证」和「这个人
 * 以后都不许申请」不是同一个决定。
 */
export function VerificationActionButtons({ token, busy, actions, online = false }: VerificationActionsProps) {
  const [rejectOpen, setRejectOpen] = useState(false)
  const [blacklist, setBlacklist] = useState(true)
  const [reason, setReason] = useState('')

  return (
    <span className="row-actions">
      <button
        type="button"
        className="btn btn-outline btn-xs"
        disabled={busy}
        title={
          online
            ? '这条记录在等本人过在线验证：再向服务申请一条链接并把卡片重发一次（一条链接只能验证一次）。' +
              '服务当前连不上的话，会退回群内按钮——扣留不断，成员仍有出路；成员不会被重新扣留'
            : '再发一次验证卡片；成员不会被重新扣留，原按钮继续有效'
        }
        onClick={() => {
          actions.run('resend', token)
        }}
      >
        重发验证
      </button>
      <button
        type="button"
        className="btn btn-secondary btn-xs"
        disabled={busy}
        title="解除扣留并关闭这条记录，不需要本人按按钮；群里会发一条「管理员已跳过 …」（skip_message）"
        onClick={() => {
          actions.run('approve', token)
        }}
      >
        立即放行
      </button>
      <button
        type="button"
        className="btn btn-warn btn-xs"
        disabled={busy}
        title="拉黑：加入本群黑名单并关闭验证记录。不解除禁言；机器人不能踢出成员，要移除请手动踢出。"
        onClick={() => {
          setBlacklist(true)
          setReason('')
          setRejectOpen(true)
        }}
      >
        拉黑
      </button>

      {rejectOpen ? (
        <Modal
          title="拉黑并关闭验证"
          desc="加入本群黑名单：以后用这个身份申请加群会被拒。已经在群里的人不会被移出。"
          onClose={() => {
            setRejectOpen(false)
          }}
          footNote="机器人无法踢出群成员 —— 要移除本人，请在群里手动踢出"
          actions={
            <button
              type="button"
              className="btn btn-warn btn-xs"
              disabled={busy}
              onClick={() => {
                setRejectOpen(false)
                actions.run('reject', token, { blacklist, reason })
              }}
            >
              确认拉黑
            </button>
          }
        >
          <div className="stack">
            <label className="row">
              <input
                type="checkbox"
                checked={blacklist}
                onChange={(event) => {
                  setBlacklist(event.target.checked)
                }}
              />
              <span>同时加入本群黑名单（以后申请加群会被拒）</span>
            </label>
            <label className="stack">
              <span className="section-note">原因（可选，写进黑名单条目）</span>
              <input
                type="text"
                className="text-input"
                value={reason}
                placeholder="例如：广告号"
                disabled={!blacklist}
                onChange={(event) => {
                  setReason(event.target.value)
                }}
              />
            </label>
            <div className="section-note">
              机器人无法踢出群成员：这条操作只写黑名单、并把验证记录关掉，人是留在群里的，
              要移除请手动踢出。如果后端连移出都试过并失败，它会在结果里照实说「没能移出」。
            </div>
          </div>
        </Modal>
      ) : null}
    </span>
  )
}
