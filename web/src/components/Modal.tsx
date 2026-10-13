import { useEffect, useId, useRef } from 'react'
import type { ReactNode } from 'react'

interface ModalProps {
  title: ReactNode
  desc?: ReactNode
  /** 标题右边的徽标位 */
  badges?: ReactNode
  onClose: () => void
  /** 底部左侧的补充说明，右侧放 actions 与关闭按钮 */
  footNote?: ReactNode
  /** 底部的动作按钮（确认之类的），排在关闭按钮左边 */
  actions?: ReactNode
  children: ReactNode
}

/**
 * 悬浮窗（对话框）。
 *
 * 三件必须做对的事，这里都做了：
 * 1. **Esc 能关**、点遮罩能关（按下时落在遮罩上才算，免得在窗内选中文字拖到外面就关掉）；
 * 2. **背景不滚动**，否则滚轮会把底下的页面滚走，看起来像弹窗没盖住；
 * 3. **焦点进得去、出得来**：打开时焦点进对话框，关闭时还给刚才那个元素 —— 键盘用户
 *    不会在关掉弹窗后"掉到页面开头"。
 *
 * 不引入任何 UI 库：这一版的敌人是装不上依赖，而对话框是最不值得为它引入依赖的组件。
 */
export function Modal({ title, desc, badges, onClose, footNote, actions, children }: ModalProps) {
  const panel = useRef<HTMLDivElement | null>(null)
  const opener = useRef<Element | null>(null)
  const titleId = useId()

  useEffect(() => {
    opener.current = document.activeElement
    panel.current?.focus()
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') {
        event.stopPropagation()
        onClose()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.body.style.overflow = previousOverflow
      const element = opener.current
      if (element instanceof HTMLElement) {
        element.focus()
      }
    }
  }, [onClose])

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        // 只有按下时就落在遮罩上的才算"点外面"，避免窗内拖选文字到窗外被当成关闭
        if (event.target === event.currentTarget) {
          onClose()
        }
      }}
    >
      <div className="modal" role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1} ref={panel}>
        <div className="modal-head">
          <div className="grow">
            <h2 className="modal-title" id={titleId}>
              {title}
            </h2>
            {desc === undefined ? null : <div className="modal-desc">{desc}</div>}
          </div>
          {/* 徽标在右上角：标题左、结论右，一眼就能对上；关闭按钮只在底部留一个，
              两个「关闭」挨在一起是纯噪声。 */}
          {badges === undefined ? null : <div className="modal-badges">{badges}</div>}
        </div>
        <div className="modal-body">{children}</div>
        <div className="modal-foot">
          <span className="section-note">{footNote}</span>
          <span className="spacer" />
          {actions}
          <button type="button" className="btn btn-outline btn-xs" onClick={onClose}>
            关闭
          </button>
        </div>
      </div>
    </div>
  )
}
