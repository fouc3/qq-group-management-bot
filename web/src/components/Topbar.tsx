import type { Group, MemberEvent, StatusResponse } from '../api/types'
import { qqGroupIdLabel, shortId } from '../lib/format'
import type { SearchHit } from '../lib/search'
import { SearchPalette } from './SearchPalette'
import type { ViewKey } from './Sidebar'

/**
 * 顶栏里的运维者展示位（content-spec §1.2 指定的运维者 QQ 号）。
 * 接口没有返回运维者身份，这只是显示用的常量。
 */
const OPERATOR_ID = '2152595244'

interface TopbarProps {
  status: StatusResponse | null
  statusLoading: boolean
  groups: readonly Group[]
  groupFilter: string
  onGroupFilter: (openid: string) => void
  /** 当前页生效的文本筛选（只有从搜索结果里选了「筛选」才有） */
  filter: string
  onFilter: (value: string) => void
  /** 搜索索引要用：成员事件里有群名 */
  events: readonly MemberEvent[]
  groupLabel: (openid: string) => string
  currentView: ViewKey
  onPick: (hit: SearchHit) => void
  onSignOut: () => void
}

function groupOptionLabel(group: Group): string {
  // 0 是「配置里没写群号」的零值，直接拼出「QQ 0」会让人以为有个 0 号群
  return `QQ ${qqGroupIdLabel(group.qq_group_id)}`
}

export function Topbar({
  status,
  statusLoading,
  groups,
  groupFilter,
  onGroupFilter,
  filter,
  onFilter,
  events,
  groupLabel,
  currentView,
  onPick,
  onSignOut,
}: TopbarProps) {
  const connection = statusLoading ? '连接中' : status === null ? '不可用' : '正常'
  const connectionDot = statusLoading ? 'is-warn' : status === null ? 'is-off' : 'is-live'

  return (
    <header className="topbar">
      <div className="conn">
        <span className={`dot ${connectionDot}`} />
        <span className="lbl">admin 服务</span>
        <span>{connection}</span>
      </div>

      <div className="group-picker">
        <select
          title="当前群"
          aria-label="当前群"
          value={groupFilter}
          onChange={(event) => {
            onGroupFilter(event.target.value)
          }}
        >
          <option value="">全部群</option>
          {groups.map((group) => (
            <option key={group.openid} value={group.openid}>
              {groupOptionLabel(group)}
            </option>
          ))}
        </select>
        <svg
          className="chev"
          width="14"
          height="14"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <path d="m6 9 6 6 6-6" />
        </svg>
      </div>

      <SearchPalette
        groups={groups}
        events={events}
        groupLabel={groupLabel}
        currentView={currentView}
        filter={filter}
        onFilter={onFilter}
        onPick={onPick}
      />

      <div className="topbar-right">
        <span className="style-tag" title={status?.config_path ?? '未读取到运行状态'}>
          {status === null ? '版本未知' : `v${status.version}`}
        </span>
        <div className="operator" title={`运维者 ${OPERATOR_ID}`}>
          <button
            type="button"
            className="avatar is-button"
            title="清除本地 token 并退出登录"
            aria-label="退出登录"
            onClick={onSignOut}
          >
            运
          </button>
          <span className="operator-id">
            {OPERATOR_ID}
            {groupFilter === '' ? '' : ` · ${shortId(groupFilter, 6, 4)}`}
          </span>
        </div>
      </div>
    </header>
  )
}
