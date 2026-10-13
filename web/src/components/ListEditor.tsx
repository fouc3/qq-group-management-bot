import { useState } from 'react'
import { toApiError } from '../api/client'
import { addBlacklist, addWatch, removeBlacklist, removeWatch } from '../api/endpoints'
import type { Group } from '../api/types'
import { unixNow } from '../lib/format'
import { Modal } from './Modal'
import { Notice } from './Notice'

/** 两个名单页共用的新增/撤销：字段不同，但形状一样，所以只有一份实现 */
type Kind = 'blacklist' | 'watch'

const EXPIRY_CHOICES: readonly { label: string; days: number }[] = [
  { label: '永久（只对黑名单）', days: 0 },
  { label: '1 天', days: 1 },
  { label: '7 天', days: 7 },
  { label: '30 天', days: 30 },
  { label: '1 年', days: 365 },
]

interface AddProps {
  kind: Kind
  groups: readonly Group[]
  groupLabel: (openid: string) => string
  /** 新增成功后：刷新列表 */
  onAdded: () => void
}

/** 「新增一条」按钮 + 表单弹窗 */
export function AddEntryButton({ kind, groups, groupLabel, onAdded }: AddProps) {
  const blacklist = kind === 'blacklist'
  const [open, setOpen] = useState(false)
  const [group, setGroup] = useState('')
  const [member, setMember] = useState('')
  const [union, setUnion] = useState('')
  const [reason, setReason] = useState('')
  const [days, setDays] = useState(blacklist ? 0 : 7)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  function submit(): void {
    setBusy(true)
    setError('')
    const expiresAt = days === 0 ? 0 : unixNow() + days * 86400
    const call = blacklist
      ? addBlacklist({ group_openid: group, member_openid: member, union_openid: union, reason, expires_at: expiresAt })
      : addWatch({ group_openid: group, member_openid: member, reason, expires_at: expiresAt })
    call
      .then(() => {
        setOpen(false)
        setMember('')
        setUnion('')
        setReason('')
        onAdded()
      })
      .catch((cause: unknown) => {
        setError(toApiError(cause).message)
      })
      .finally(() => {
        setBusy(false)
      })
  }

  return (
    <>
      <button
        type="button"
        className="btn btn-secondary"
        onClick={() => {
          setError('')
          setOpen(true)
        }}
      >
        {blacklist ? '新增黑名单' : '标记高风险'}
      </button>

      {open ? (
        <Modal
          title={blacklist ? '新增黑名单条目' : '标记高风险成员'}
          desc={
            blacklist
              ? '拦住以后用这个身份申请加群的人；已经在群里的不会被移出。'
              : '标记后该成员的每条消息都会被判定；到期时间必填 —— 否则这条标记没人会再看它一眼。'
          }
          onClose={() => {
            setOpen(false)
          }}
          footNote="写入会记一行日志，added_by 记为 management console"
          actions={
            <button type="button" className="btn btn-secondary btn-xs" disabled={busy} onClick={submit}>
              {busy ? '提交中…' : blacklist ? '写入' : '标记'}
            </button>
          }
        >
          {error === '' ? null : (
            <Notice tone="error" title="没有写进去">
              {error}
            </Notice>
          )}
          <div className="stack">
            <label className="stack">
              <span className="section-note">群（留空 = 所有群）</span>
              <select
                className="text-input"
                value={group}
                onChange={(event) => {
                  setGroup(event.target.value)
                }}
              >
                <option value="">全部群 / 全局条目</option>
                {groups.map((item) => (
                  <option key={item.openid} value={item.openid}>
                    {groupLabel(item.openid)}
                  </option>
                ))}
              </select>
            </label>
            <label className="stack">
              <span className="section-note">成员 openid（必填）</span>
              <input
                className="text-input"
                value={member}
                placeholder="从「成员事件」或「违规判定」里复制"
                onChange={(event) => {
                  setMember(event.target.value)
                }}
              />
            </label>
            {blacklist ? (
              <label className="stack">
                <span className="section-note">union openid（可选，用来拦还没申请过的人）</span>
                <input
                  className="text-input"
                  value={union}
                  onChange={(event) => {
                    setUnion(event.target.value)
                  }}
                />
              </label>
            ) : null}
            <label className="stack">
              <span className="section-note">原因（可选）</span>
              <input
                className="text-input"
                value={reason}
                onChange={(event) => {
                  setReason(event.target.value)
                }}
              />
            </label>
            <label className="stack">
              <span className="section-note">到期</span>
              <select
                className="text-input"
                value={String(days)}
                onChange={(event) => {
                  setDays(Number(event.target.value))
                }}
              >
                {EXPIRY_CHOICES.filter((choice) => blacklist || choice.days > 0).map((choice) => (
                  <option key={choice.days} value={String(choice.days)}>
                    {choice.label}
                  </option>
                ))}
              </select>
            </label>
          </div>
        </Modal>
      ) : null}
    </>
  )
}

interface RemoveProps {
  kind: Kind
  /** 黑名单：条目 id；高风险：成员 openid */
  id: string
  group: string
  member: string
  onRemoved: () => void
}

/** 行内的「撤销」按钮 */
export function RemoveEntryButton({ kind, id, group, member, onRemoved }: RemoveProps) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  function submit(): void {
    setBusy(true)
    setError('')
    const call = kind === 'blacklist' ? removeBlacklist(id, group) : removeWatch(group, member)
    call
      .then(() => {
        onRemoved()
      })
      .catch((cause: unknown) => {
        setError(toApiError(cause).message)
      })
      .finally(() => {
        setBusy(false)
      })
  }

  return (
    <span className="row-actions">
      <button type="button" className="btn btn-outline btn-xs" disabled={busy} onClick={submit} title={error}>
        {busy ? '撤销中…' : '撤销'}
      </button>
    </span>
  )
}
