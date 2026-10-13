import type { ReactNode } from 'react'

type NoticeTone = 'info' | 'success' | 'warning' | 'error'

interface NoticeProps {
  tone?: NoticeTone
  title?: ReactNode
  children?: ReactNode
  actions?: ReactNode
}

export function Notice({ tone = 'info', title, children, actions }: NoticeProps) {
  return (
    <div className={`notice is-${tone}`} role={tone === 'error' ? 'alert' : 'status'}>
      <div className="grow">
        {title !== undefined ? <div className="n-title">{title}</div> : null}
        {children !== undefined ? <div className="n-body">{children}</div> : null}
      </div>
      {actions}
    </div>
  )
}
