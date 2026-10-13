import type { ReactNode } from 'react'
import type { Group, MemberEvent, StatusResponse } from '../api/types'
import type { SearchHit } from '../lib/search'
import { Sidebar } from './Sidebar'
import type { ViewKey } from './Sidebar'
import { Topbar } from './Topbar'

interface LayoutProps {
  active: ViewKey
  onNavigate: (key: ViewKey) => void
  status: StatusResponse | null
  statusLoading: boolean
  groups: readonly Group[]
  groupFilter: string
  onGroupFilter: (openid: string) => void
  events: readonly MemberEvent[]
  groupLabel: (openid: string) => string
  filter: string
  onFilter: (value: string) => void
  onPick: (hit: SearchHit) => void
  onSignOut: () => void
  children: ReactNode
}

/** 应用外壳：左侧导航 + 顶栏 + 内容区 */
export function Layout({
  active,
  onNavigate,
  status,
  statusLoading,
  groups,
  groupFilter,
  onGroupFilter,
  events,
  groupLabel,
  filter,
  onFilter,
  onPick,
  onSignOut,
  children,
}: LayoutProps) {
  return (
    <div className="app">
      <Sidebar active={active} onNavigate={onNavigate} status={status} />
      <div className="main">
        <Topbar
          status={status}
          statusLoading={statusLoading}
          groups={groups}
          groupFilter={groupFilter}
          onGroupFilter={onGroupFilter}
          filter={filter}
          onFilter={onFilter}
          events={events}
          groupLabel={groupLabel}
          currentView={active}
          onPick={onPick}
          onSignOut={onSignOut}
        />
        <div className="content">{children}</div>
      </div>
    </div>
  )
}
