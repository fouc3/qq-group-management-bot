import type { ReactNode } from 'react'

interface EmptyStateProps {
  /** 面包屑式的小标题，一般写当前页名 */
  crumb?: string
  message: ReactNode
  note?: ReactNode
  icon?: ReactNode
  /** 嵌在卡片里时用 inline，去掉卡片底 */
  inline?: boolean
}

const DEFAULT_ICON = (
  <svg
    width="18"
    height="18"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
    aria-hidden="true"
  >
    <rect x="3" y="4" width="18" height="16" rx="2" />
    <path d="M3 10h18" />
    <path d="M9 10v10" />
  </svg>
)

export function EmptyState({ crumb, message, note, icon, inline = false }: EmptyStateProps) {
  return (
    <div className={`empty${inline ? ' is-inline' : ''}`}>
      <div className="tile">{icon ?? DEFAULT_ICON}</div>
      {crumb !== undefined ? <div className="crumb">{crumb}</div> : null}
      <p className="msg">{message}</p>
      {note !== undefined ? <div className="tile-cap">{note}</div> : null}
    </div>
  )
}
