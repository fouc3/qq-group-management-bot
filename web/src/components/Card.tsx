import type { ReactNode } from 'react'

interface CardProps {
  /** 定位用（顶栏搜索跳过来时按它滚动，见 lib/focus.ts） */
  id?: string
  title?: ReactNode
  desc?: ReactNode
  actions?: ReactNode
  /** 全页最“响”的一张卡，一页最多一个 */
  decorated?: boolean
  hover?: boolean
  className?: string
  children?: ReactNode
}

export function Card({ id, title, desc, actions, decorated = false, hover = false, className = '', children }: CardProps) {
  const classes = ['card', decorated ? 'decorated' : '', hover ? 'card-hover' : '', className]
    .filter((part) => part !== '')
    .join(' ')
  const hasHead = title !== undefined || desc !== undefined || actions !== undefined
  return (
    <section id={id} className={classes}>
      {hasHead ? (
        <div className="card-head">
          <div className="grow">
            {title !== undefined ? <h3 className="card-title">{title}</h3> : null}
            {desc !== undefined ? <div className="card-desc">{desc}</div> : null}
          </div>
          {actions}
        </div>
      ) : null}
      {children}
    </section>
  )
}
