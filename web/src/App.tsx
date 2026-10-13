import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { ApiError, clearToken, hasToken, saveToken, subscribeToken } from './api/client'
import { getGroups, getMemberEvents, getStatus } from './api/endpoints'
import type { GroupsResponse, StatusResponse } from './api/types'
import { useAsync } from './hooks/useAsync'
import { shortId } from './lib/format'
import { viewOfRecord } from './lib/focus'
import type { RecordFocus } from './lib/focus'
import type { SearchHit } from './lib/search'
import { Layout } from './components/Layout'
import { NAV_ITEMS } from './components/Sidebar'
import type { ViewKey } from './components/Sidebar'
import { Spinner } from './components/Spinner'
import { TokenPrompt } from './components/TokenPrompt'
import { BlacklistView } from './views/BlacklistView'
import { BroadcastsView } from './views/BroadcastsView'
import { ConfigView } from './views/ConfigView'
import { GroupsView } from './views/GroupsView'
import { HighRiskView } from './views/HighRiskView'
import { JudgementsView } from './views/JudgementsView'
import { LogsView } from './views/LogsView'
import { MemberEventsView } from './views/MemberEventsView'
import { OverviewView } from './views/OverviewView'
import { VerificationView } from './views/VerificationView'

/**
 * 地址栏里的视图（`#logs` 这类）。
 *
 * 控制台不是多页应用，但「刷新之后就回到概览」和「把某一页发给别人」都是运维会
 * 真的想做的事，而这两件事只需要一个 hash。认不出来的 hash 一律回概览，不报错：
 * 一个打错字的链接应该看到首页，而不是一个错误页。
 */
function viewFromHash(): ViewKey {
  const raw = window.location.hash.replace(/^#\/?/, '')
  const item = NAV_ITEMS.find((nav) => nav.key === raw)
  return item === undefined ? 'overview' : item.key
}

/**
 * 控制台主体。只有拿到 token 时才挂载 —— 所有请求都从 here 往下发，
 * 未登录时不会向后端打任何请求。
 */
function Console({ onSignOut }: { onSignOut: () => void }) {
  const [view, setView] = useState<ViewKey>(viewFromHash)
  const [groupFilter, setGroupFilter] = useState('')
  // 页面文本筛选：不再由输入框直接写，只有从搜索候选里选「筛选」才会设置
  const [search, setSearch] = useState('')
  // 从搜索跳过来的那一条记录：页面收到后滚过去、打开它
  const [focus, setFocus] = useState<RecordFocus | null>(null)
  const focusSeq = useRef(0)

  // 地址栏跟着视图走，这样刷新、收藏、粘贴给别人都有意义；返回键也回到上一页。
  useEffect(() => {
    const wanted = `#${view}`
    if (window.location.hash !== wanted) {
      window.history.pushState(null, '', wanted)
    }
  }, [view])

  useEffect(() => {
    const onHashChange = (): void => {
      setFocus(null)
      setView(viewFromHash())
    }
    window.addEventListener('hashchange', onHashChange)
    return () => {
      window.removeEventListener('hashchange', onHashChange)
    }
  }, [])

  const status = useAsync<StatusResponse>(() => getStatus(), [])
  const groups = useAsync<GroupsResponse>(() => getGroups(), [])
  // 成员事件里带 group_name，用它把 openid 显示成群名（/groups 只有 QQ 群号）
  const roster = useAsync(() => getMemberEvents({ limit: 200 }), [])

  const nameByOpenid = useMemo(() => {
    const map = new Map<string, string>()
    for (const event of roster.data?.events ?? []) {
      if (event.group_name !== '' && !map.has(event.group_openid)) {
        map.set(event.group_openid, event.group_name)
      }
    }
    return map
  }, [roster.data])

  const qqByOpenid = useMemo(() => {
    const map = new Map<string, number>()
    for (const group of groups.data?.groups ?? []) {
      map.set(group.openid, group.qq_group_id)
    }
    return map
  }, [groups.data])

  const groupLabel = useCallback(
    (openid: string): string => {
      const name = nameByOpenid.get(openid)
      if (name !== undefined) {
        return name
      }
      const qq = qqByOpenid.get(openid)
      // 0 是配置里没写群号的零值，写成「QQ 0」会像是真有个 0 号群
      if (qq !== undefined && qq > 0) {
        return `QQ ${String(qq)}`
      }
      return shortId(openid, 8, 4)
    },
    [nameByOpenid, qqByOpenid],
  )

  const memberEvents = roster.data?.events ?? []

  // 侧栏翻页：手动翻页不该把上一次搜索的定位带过来（否则回到那一页会自己弹窗）
  const goTo = useCallback((key: ViewKey): void => {
    setFocus(null)
    setView(key)
  }, [])

  /**
   * 选中一条搜索候选。
   *
   * 群 = 切换当前群；记录 = 跳到它那一页并定位到它；筛选 = 在某一页按文本过滤；
   * 页面 = 直接过去。跳记录时顺手把「当前群」对齐到记录自己的群，并把页面文本筛选
   * 清掉 —— 否则目标可能刚好被这两个过滤条件挡在列表外面，人就以为「搜到了但看不到」。
   */
  const runHit = useCallback((hit: SearchHit): void => {
    const action = hit.action
    switch (action.type) {
      case 'group':
        setGroupFilter(action.openid)
        break
      case 'view':
        setSearch('')
        goTo(action.view)
        break
      case 'filter':
        setSearch(action.filter)
        goTo(action.view)
        break
      case 'record':
        setSearch('')
        setGroupFilter(hit.group ?? '')
        focusSeq.current += 1
        setFocus({ kind: action.kind, key: action.key, seq: focusSeq.current, record: hit.record })
        setView(viewOfRecord(action.kind))
        break
    }
  }, [goTo])

  let body: ReactNode
  switch (view) {
    case 'overview':
      body = (
        <OverviewView
          status={status}
          groups={groups}
          groupFilter={groupFilter}
          groupLabel={groupLabel}
          search={search}
          onNavigate={goTo}
        />
      )
      break
    case 'groups':
      body = (
        <GroupsView
          groups={groups}
          groupFilter={groupFilter}
          onGroupFilter={setGroupFilter}
          groupLabel={groupLabel}
          search={search}
          onReloaded={() => {
            status.refresh()
            roster.refresh()
            groups.refresh()
          }}
        />
      )
      break
    case 'member-events':
      body = (
        <MemberEventsView
          groups={groups}
          groupFilter={groupFilter}
          onGroupFilter={setGroupFilter}
          groupLabel={groupLabel}
          search={search}
          focus={focus}
        />
      )
      break
    case 'join-verification':
      body = (
        <VerificationView
          groups={groups}
          groupFilter={groupFilter}
          onGroupFilter={setGroupFilter}
          groupLabel={groupLabel}
          search={search}
          focus={focus}
          humanVerification={status.data?.human_verification ?? null}
        />
      )
      break
    case 'blacklist':
      body = (
        <BlacklistView
          groups={groups}
          groupFilter={groupFilter}
          onGroupFilter={setGroupFilter}
          groupLabel={groupLabel}
          search={search}
          focus={focus}
        />
      )
      break
    case 'high-risk':
      body = (
        <HighRiskView
          groups={groups}
          groupFilter={groupFilter}
          onGroupFilter={setGroupFilter}
          groupLabel={groupLabel}
          search={search}
          focus={focus}
        />
      )
      break
    case 'judgements':
      body = (
        <JudgementsView
          groups={groups}
          groupFilter={groupFilter}
          onGroupFilter={setGroupFilter}
          groupLabel={groupLabel}
          search={search}
          focus={focus}
        />
      )
      break
    case 'broadcasts':
      body = <BroadcastsView groups={groups} groupLabel={groupLabel} search={search} focus={focus} />
      break
    case 'config':
      body = (
        <ConfigView
          search={search}
          onReloaded={() => {
            status.refresh()
            groups.refresh()
          }}
        />
      )
      break
    case 'logs':
      body = <LogsView search={search} />
      break
  }

  return (
    <Layout
      active={view}
      onNavigate={goTo}
      status={status.data}
      statusLoading={status.loading}
      groups={groups.data?.groups ?? []}
      groupFilter={groupFilter}
      onGroupFilter={setGroupFilter}
      events={memberEvents}
      groupLabel={groupLabel}
      filter={search}
      onFilter={setSearch}
      onPick={runHit}
      onSignOut={onSignOut}
    >
      {body}
    </Layout>
  )
}

export function App() {
  // 'probing' 是一个真实状态，不是一个可以省掉的中间态：后端在回环地址上可以
  // 根本没有 token，那时界面上就不该出现登录框。而"本地没存 token"和"后端要
  // token"是两件事——把它们当成一件事，就会给一个不需要登录的控制台挡上一道
  // 登录墙。
  const [auth, setAuth] = useState<'probing' | 'open' | 'needs-token'>(
    () => (hasToken() ? 'open' : 'probing'),
  )
  const [notice, setNotice] = useState('')
  // 登录后换 key 强制重挂 Console，保证所有数据按新凭证重新拉取
  const [session, setSession] = useState(0)

  useEffect(() => {
    return subscribeToken(() => {
      const ok = hasToken()
      setAuth(ok ? 'open' : 'needs-token')
      if (ok) {
        setNotice('')
        setSession((value) => value + 1)
      } else {
        setNotice('后端返回 401，本地 token 已清除，请重新输入。')
      }
    })
  }, [])

  useEffect(() => {
    if (auth !== 'probing') {
      return
    }
    let cancelled = false
    // 拿一个不需要凭证也存在的接口去问：它 401 就说明这个部署要 token，200 就
    // 说明不要。除了 401 以外的失败（网络不通、后端没起来）也放行到控制台——
    // 那种情况下该看到的是"连不上"，而不是一个输入 token 也没用的登录框。
    getStatus()
      .then(() => {
        if (!cancelled) {
          setAuth('open')
        }
      })
      .catch((error: unknown) => {
        if (cancelled) {
          return
        }
        setAuth(error instanceof ApiError && error.status === 401 ? 'needs-token' : 'open')
      })
    return () => {
      cancelled = true
    }
  }, [auth])

  if (auth === 'probing') {
    return (
      <div className="splash">
        <Spinner label="正在连接机器人…" large />
      </div>
    )
  }

  if (auth === 'needs-token') {
    return (
      <TokenPrompt
        notice={notice}
        onSubmit={(token) => {
          saveToken(token)
        }}
      />
    )
  }

  return (
    <Console
      key={session}
      onSignOut={() => {
        clearToken()
      }}
    />
  )
}
