import type { ReactNode } from 'react'
import type { StatusResponse } from '../api/types'
import { formatBytes, formatSpan } from '../lib/format'

/** 侧栏 10 项固定顺序（概览 群组 成员事件 入群验证 黑名单 高风险 违规判定 广播审计 配置 日志），
 *  每一项都有真实视图；接口没开放的写操作在页面里画成 disabled 并说明原因。 */
export type ViewKey =
  | 'overview'
  | 'groups'
  | 'member-events'
  | 'join-verification'
  | 'blacklist'
  | 'high-risk'
  | 'judgements'
  | 'broadcasts'
  | 'config'
  | 'logs'

export interface NavItem {
  key: ViewKey
  label: string
  group: '监控' | '风控' | '运维'
}

export const NAV_GROUPS: readonly NavItem['group'][] = ['监控', '风控', '运维']

export const NAV_ITEMS: readonly NavItem[] = [
  { key: 'overview', label: '概览', group: '监控' },
  { key: 'groups', label: '群组', group: '监控' },
  { key: 'member-events', label: '成员事件', group: '监控' },
  { key: 'join-verification', label: '入群验证', group: '监控' },
  { key: 'blacklist', label: '黑名单', group: '风控' },
  { key: 'high-risk', label: '高风险', group: '风控' },
  { key: 'judgements', label: '违规判定', group: '风控' },
  { key: 'broadcasts', label: '广播审计', group: '运维' },
  { key: 'config', label: '配置', group: '运维' },
  { key: 'logs', label: '日志', group: '运维' },
]

const ICONS: Record<ViewKey, ReactNode> = {  overview: (
    <>
      <rect x="3" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="14" width="7" height="7" rx="1" />
      <rect x="3" y="14" width="7" height="7" rx="1" />
    </>
  ),
  groups: (
    <>
      <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M22 21v-2a4 4 0 0 0-3-3.87" />
      <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </>
  ),
  'member-events': (
    <>
      <path d="M22 12h-4l-3 9L9 3l-3 9H2" />
    </>
  ),
  'join-verification': (
    <>
      <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="m16 11 2 2 4-4" />
    </>
  ),
  blacklist: (
    <>
      <circle cx="12" cy="12" r="10" />
      <path d="m4.9 4.9 14.2 14.2" />
    </>
  ),
  'high-risk': (
    <>
      <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
      <path d="M12 8v4" />
      <path d="M12 16h.01" />
    </>
  ),
  judgements: (
    <>
      <path d="m21.7 18-8-14a2 2 0 0 0-3.4 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.7-3z" />
      <path d="M12 9v4" />
      <path d="M12 17h.01" />
    </>
  ),
  broadcasts: (
    <>
      <path d="m3 11 18-5v12L3 14v-3z" />
      <path d="M11.6 16.8a3 3 0 1 1-5.8-1.6" />
    </>
  ),
  config: (
    <>
      <path d="M4 21v-7" />
      <path d="M4 10V3" />
      <path d="M12 21v-9" />
      <path d="M12 8V3" />
      <path d="M20 21v-5" />
      <path d="M20 12V3" />
      <path d="M1 14h6" />
      <path d="M9 8h6" />
      <path d="M17 16h6" />
    </>
  ),
  logs: (
    <>
      <rect x="4" y="3" width="16" height="18" rx="2" />
      <path d="M8 8h8" />
      <path d="M8 12h8" />
      <path d="M8 16h4" />
    </>
  ),
}

interface SidebarProps {
  active: ViewKey
  onNavigate: (key: ViewKey) => void
  status: StatusResponse | null
}

export function Sidebar({ active, onNavigate, status }: SidebarProps) {
  return (
    <aside className="sidebar">
      <div className="brand">
        <div className="brand-mark">
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
            <rect x="4" y="8" width="16" height="12" rx="3" />
            <path d="M12 4v4" />
            <path d="M9 14h.01" />
            <path d="M15 14h.01" />
          </svg>
        </div>
        <div className="brand-text">
          <div className="brand-name">群管控制台</div>
          <div className="brand-sub">QQ Group Management Bot</div>
        </div>
      </div>

      <nav className="nav" aria-label="主导航">
        {NAV_GROUPS.map((group) => (
          <div key={group}>
            <div className="nav-group-label">{group}</div>
            {NAV_ITEMS.filter((item) => item.group === group).map((item) => {
              const on = item.key === active
              return (
                <button
                  key={item.key}
                  type="button"
                  className={`nav-item${on ? ' is-active' : ''}`}
                  aria-current={on ? 'page' : undefined}
                  onClick={() => {
                    onNavigate(item.key)
                  }}
                >
                  <svg
                    width="16"
                    height="16"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    aria-hidden="true"
                  >
                    {ICONS[item.key]}
                  </svg>
                  <span className="nav-label">{item.label}</span>
                </button>
              )
            })}
          </div>
        ))}
      </nav>

      <div className="sidebar-foot">
        <div className="foot-row">
          <span className={`dot ${status === null ? 'is-off' : 'is-live'}`} />
          <span>{status === null ? 'admin 未就绪' : 'admin 服务在线'}</span>
        </div>
        <div className="foot-row">
          <span className="ellipsis">
            {status === null
              ? '数据库状态未知'
              : `${status.database.driver} · ${formatBytes(status.database.size_bytes)}`}
          </span>
        </div>
        {status?.human_verification.configured === true ? (
          <div className="foot-row">
            <span className={`dot ${status.human_verification.connected ? 'is-live' : 'is-warn'}`} />
            <span>{status.human_verification.connected ? '在线验证已连接' : '在线验证未连接'}</span>
          </div>
        ) : null}
        <div className="foot-row">
          <span className="ellipsis">
            {status === null ? '版本未知' : `v${status.version} · 已运行 ${formatSpan(status.uptime_seconds)}`}
          </span>
        </div>
      </div>
    </aside>
  )
}
