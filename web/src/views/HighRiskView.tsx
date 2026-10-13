import { useEffect, useMemo, useState } from 'react'
import { getHighRisk } from '../api/endpoints'
import type { GroupsResponse, HighRiskWatch } from '../api/types'
import type { AsyncState } from '../hooks/useAsync'
import { firstError, isAnyLoading, refreshAll, useAsync } from '../hooks/useAsync'
import { useTicker } from '../hooks/useTicker'
import { downloadCsv } from '../lib/csv'
import { formatAgo, formatDateTime, formatSpan, matches, orDash, shortId, unixNow } from '../lib/format'
import { Badge } from '../components/Badge'
import { DataTable } from '../components/DataTable'
import type { Column } from '../components/DataTable'
import { DistChart } from '../components/DistChart'
import type { DistRow } from '../components/DistChart'
import { EmptyState } from '../components/EmptyState'
import { AddEntryButton, RemoveEntryButton } from '../components/ListEditor'
import { Notice } from '../components/Notice'
import { SegmentedControl } from '../components/SegmentedControl'
import { Spinner } from '../components/Spinner'
import { StatCard } from '../components/StatCard'
import { recordDomId } from '../lib/focus'
import type { RecordFocus } from '../lib/focus'

interface HighRiskProps {
  groups: AsyncState<GroupsResponse>
  groupFilter: string
  onGroupFilter: (openid: string) => void
  groupLabel: (openid: string) => string
  search: string
  /** 顶栏搜索跳过来的那一条：滚过去并高亮（见 lib/focus.ts） */
  focus?: RecordFocus | null
}

type UrgencyFilter = 'all' | 'soon' | 'later'

const URGENCY_OPTIONS: readonly { value: UrgencyFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'soon', label: '24 小时内到期' },
  { value: 'later', label: '24 小时以上' },
]

export function HighRiskView({ groups, groupFilter, onGroupFilter, groupLabel, search, focus }: HighRiskProps) {
  const now = useTicker(60_000)
  const [urgency, setUrgency] = useState<UrgencyFilter>('all')

  const focusId = focus?.kind === 'watch' ? recordDomId('watch', focus.key) : undefined
  // 跳过来之后先取消「即将到期 / 还早」的筛选，否则目标可能不在列表里
  useEffect(() => {
    if (focusId === undefined) {
      return
    }
    setUrgency('all')
  }, [focusId, focus?.seq])

  const watches = useAsync(() => getHighRisk(), [])
  const error = firstError(groups, watches)
  const loading = isAnyLoading(watches)

  const sorted = useMemo(() => {
    const all = watches.data?.watches ?? []
    return all
      .filter((row) => groupFilter === '' || row.group_openid === groupFilter)
      .filter((row) => matches([row.member_openid, row.reason, row.added_by, groupLabel(row.group_openid)], search))
      .sort((a, b) => a.expires_at - b.expires_at)
  }, [watches.data, groupFilter, search, groupLabel])

  const rows = useMemo(
    () =>
      sorted.filter((row) => {
        if (urgency === 'all') {
          return true
        }
        const remain = row.expires_at - now
        return urgency === 'soon' ? remain <= 24 * 3600 : remain > 24 * 3600
      }),
    [sorted, urgency, now],
  )

  const counts = useMemo(() => {
    const perGroup = new Map<string, number>()
    let soon = 0
    for (const row of sorted) {
      perGroup.set(row.group_openid, (perGroup.get(row.group_openid) ?? 0) + 1)
      if (row.expires_at - now <= 24 * 3600) {
        soon += 1
      }
    }
    return {
      soon,
      perGroup,
      soonest: sorted.reduce((min, row) => (row.expires_at < min ? row.expires_at : min), Number.MAX_SAFE_INTEGER),
    }
  }, [sorted, now])

  const distRows = useMemo<DistRow[]>(() => {
    return [...counts.perGroup.entries()]
      .map(([openid, count]) => ({
        label: openid === '' ? '未标群' : groupLabel(openid),
        value: count,
        tone: 'danger' as const,
        title: `${openid === '' ? '未标群（更早写入、没记 group_openid 的条目）' : groupLabel(openid)} · ${String(count)} 人`,
      }))
      .sort((a, b) => b.value - a.value)
  }, [counts.perGroup, groupLabel])

  const columns: readonly Column<HighRiskWatch>[] = [
    {
      key: 'group',
      header: '群',
      render: (row) =>
        row.group_openid === '' ? (
          <span className="muted">未标群（历史条目）</span>
        ) : (
          <span className="nowrap">{groupLabel(row.group_openid)}</span>
        ),
    },
    {
      key: 'member',
      header: '成员 openid',
      render: (row) => (
        <span className="oid" title={row.member_openid}>
          {shortId(row.member_openid, 10, 6)}
        </span>
      ),
    },
    { key: 'reason', header: '原因', render: (row) => orDash(row.reason) },
    {
      key: 'added_by',
      header: '发起人',
      render: (row) =>
        row.added_by === '' ? <span className="muted">未记录</span> : <span className="mono-sm">{row.added_by}</span>,
    },
    {
      key: 'added_at',
      header: '加入时间',
      className: 'time',
      render: (row) => (
        <span title={formatDateTime(row.added_at)}>
          {formatDateTime(row.added_at)}
          <span className="muted"> · {formatAgo(row.added_at, now)}</span>
        </span>
      ),
    },
    { key: 'expires_at', header: '到期', className: 'time', render: (row) => formatDateTime(row.expires_at) },
    {
      key: 'remain',
      header: '剩余',
      render: (row) => (
        <span className="row nowrap">
          <span>{formatSpan(row.expires_at - now)}</span>
          {row.expires_at - now <= 24 * 3600 ? <Badge tone="warn">即将到期</Badge> : null}
        </span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      className: 'col-actions',
      render: (row) => (
        <RemoveEntryButton
          kind="watch"
          id={row.id}
          group={row.group_openid}
          member={row.member_openid}
          onRemoved={() => { watches.refresh() }}
        />
      ),
    },
    {
      key: 'id',
      header: '记录 id',
      render: (row) => (
        <span className="mono-sm muted" title={row.id}>
          {shortId(row.id, 8, 4)}
        </span>
      ),
    },
  ]

  function handleExport(): void {
    downloadCsv(
      `high-risk-${String(unixNow())}.csv`,
      ['id', '群', '群 openid', '成员 openid', '原因', '发起人', '加入时间', '到期时间', '剩余秒数'],
      rows.map((row) => [
        row.id,
        row.group_openid === '' ? '未标群' : groupLabel(row.group_openid),
        row.group_openid,
        row.member_openid,
        row.reason,
        row.added_by,
        formatDateTime(row.added_at),
        formatDateTime(row.expires_at),
        String(Math.max(0, row.expires_at - now)),
      ]),
    )
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">高风险</h1>
          <div className="page-sub">每条消息都送检的成员（high_risk_watches）：为什么被盯上、还要盯多久</div>
        </div>
        <div className="page-actions">
          {loading ? <Spinner /> : null}
          <button type="button" className="btn btn-outline" onClick={handleExport} disabled={rows.length === 0}>
            导出 CSV
          </button>
          <AddEntryButton
            kind="watch"
            groups={groups.data?.groups ?? []}
            groupLabel={groupLabel}
            onAdded={() => { watches.refresh() }}
          />
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => {
              refreshAll(groups, watches)
            }}
          >
            刷新
          </button>
        </div>
      </div>

      {error ? (
        <Notice
          tone="error"
          title={`接口请求失败（${error.code}）`}
          actions={
            <button
              type="button"
              className="btn btn-outline btn-xs"
              onClick={() => {
                refreshAll(groups, watches)
              }}
            >
              重试
            </button>
          }
        >
          {error.message}
        </Notice>
      ) : null}

      <div className="metrics">
        <StatCard
          label="在册"
          value={watches.data === null ? '—' : sorted.length}
          dot="danger"
          decorated
          note={groupFilter === '' ? '全部群' : `当前群：${groupLabel(groupFilter)}`}
        />
        <StatCard
          label="最近到期"
          value={counts.soonest === Number.MAX_SAFE_INTEGER ? '—' : formatSpan(counts.soonest - now)}
          dot="warn"
          note="这条到期后就不再自动送检"
        />
        <StatCard label="24 小时内到期" value={counts.soon} dot="coral" note="需要有人决定续不续" />
        <StatCard label="覆盖群" value={counts.perGroup.size} dot="off" note="按 group_openid 去重" />
      </div>

      <Notice tone="info" title="这里没有「永久」">
        每条记录都有到期时间，这是数据层强制要求的：<code>high_risk_watches.expires_at</code> 是{' '}
        <code>NOT NULL</code>，写 0（永久）会被 store 拒绝 —— 理由是「一条永不失效的监视会让每条消息都付一次
        判定成本，而且没人会再想起它」。因此这一页不返回已经到期的记录，也就不存在「永久在册」。
      </Notice>

      <div className="filterbar">
        <label className="filter">
          <span className="fk">群</span>
          <select
            aria-label="群"
            value={groupFilter}
            onChange={(event) => {
              onGroupFilter(event.target.value)
            }}
          >
            <option value="">全部</option>
            {(groups.data?.groups ?? []).map((group) => (
              <option key={group.openid} value={group.openid}>
                {groupLabel(group.openid)}
              </option>
            ))}
          </select>
          <svg
            width="13"
            height="13"
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
        </label>

        <span className="filter">
          <span className="fk">到期</span>
          <SegmentedControl<UrgencyFilter>
            value={urgency}
            options={URGENCY_OPTIONS}
            onChange={setUrgency}
            label="到期"
          />
        </span>

        <span className="spacer" />
        <span className="section-note">显示 {rows.length} 条 · 按到期时间升序（先到期的先看）</span>
      </div>

      <div className="charts is-single">
        <DistChart
          title="按群分布"
          desc="每群在册人数；这是「每条消息都要过一遍判定」的成本来源"
          rows={distRows}
          emptyText="没有在册成员"
        />
      </div>

      <div className="section-title">
        <h2>在册成员</h2>
        <span className="count-badge">{rows.length}</span>
      </div>
      {watches.data === null && watches.loading ? (
        <div className="load-block">
          <Spinner label="正在读取高风险名单…" />
        </div>
      ) : (
        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(row) => row.id}
          rowId={(row) => recordDomId('watch', row.id)}
          focusId={focusId}
          focusSeq={focus?.seq}
          scroll
          empty={
            <EmptyState
              crumb="高风险"
              message={urgency === 'all' ? '没有高风险在册成员' : '当前筛选下没有成员'}
              note="high_risk_watches 为空，或标记都已到期 —— 后端只返回仍然生效的记录"
            />
          }
        />
      )}
    </>
  )
}
