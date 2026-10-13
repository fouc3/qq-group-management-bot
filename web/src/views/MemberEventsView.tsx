import { useEffect, useMemo, useState } from 'react'
import { getMemberEvents } from '../api/endpoints'
import type { GroupsResponse, MemberEvent } from '../api/types'
import type { AsyncState } from '../hooks/useAsync'
import { firstError, isAnyLoading, refreshAll, useAsync } from '../hooks/useAsync'
import { downloadCsv } from '../lib/csv'
import {
  formatAgo,
  formatClock,
  formatDateTime,
  matches,
  memberEventLabel,
  qqGroupIdLabel,
  shortId,
  unixNow,
} from '../lib/format'
import { memberEventKey, recordDomId } from '../lib/focus'
import type { RecordFocus } from '../lib/focus'
import { BarChart } from '../components/BarChart'
import type { BarColumn } from '../components/BarChart'
import { Badge } from '../components/Badge'
import { DataTable } from '../components/DataTable'
import type { Column } from '../components/DataTable'
import { EmptyState } from '../components/EmptyState'
import { Notice } from '../components/Notice'
import { SegmentedControl } from '../components/SegmentedControl'
import { Spinner } from '../components/Spinner'
import { StatCard } from '../components/StatCard'

interface MemberEventsProps {
  groups: AsyncState<GroupsResponse>
  groupFilter: string
  onGroupFilter: (openid: string) => void
  groupLabel: (openid: string) => string
  search: string
  /** 顶栏搜索跳过来的那一条：滚过去并高亮（见 lib/focus.ts） */
  focus?: RecordFocus | null
}

type KindFilter = 'all' | 'join' | 'leave'
type RangeFilter = '24h' | '7d' | '30d' | 'all'

const KIND_OPTIONS: readonly { value: KindFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'join', label: '入群' },
  { value: 'leave', label: '退群' },
]

const RANGE_OPTIONS: readonly { value: RangeFilter; label: string }[] = [
  { value: '24h', label: '近 24 小时' },
  { value: '7d', label: '近 7 天' },
  { value: '30d', label: '近 30 天' },
  { value: 'all', label: '不限时间' },
]

const RANGE_SECONDS: Record<RangeFilter, number> = {
  '24h': 24 * 3600,
  '7d': 7 * 24 * 3600,
  '30d': 30 * 24 * 3600,
  all: 0,
}

const LIMIT_OPTIONS: readonly number[] = [200, 500, 1000]

/** 本地日期键：按天分桶用（展示层派生，接口没有日序列） */
function dayKey(unixSeconds: number): string {
  const date = new Date(unixSeconds * 1000)
  const month = date.getMonth() + 1
  const day = date.getDate()
  return `${month < 10 ? '0' : ''}${month}-${day < 10 ? '0' : ''}${day}`
}

export function MemberEventsView({
  groups,
  groupFilter,
  onGroupFilter,
  groupLabel,
  search,
  focus,
}: MemberEventsProps) {
  const [kind, setKind] = useState<KindFilter>('all')
  const [range, setRange] = useState<RangeFilter>('24h')
  const [limit, setLimit] = useState(200)
  const [now] = useState(() => unixNow())

  const focusId = focus?.kind === 'member-event' ? recordDomId('member-event', focus.key) : undefined
  // 跳过来之后把本页自己的过滤放宽：默认只读 24 小时、只看一种类型，
  // 目标很可能刚好被这两条挡在列表外，那就成了「搜到了却看不到」
  useEffect(() => {
    if (focusId === undefined) {
      return
    }
    setRange('all')
    setKind('all')
  }, [focusId, focus?.seq])

  const since = useMemo(() => {
    const span = RANGE_SECONDS[range]
    return span === 0 ? undefined : now - span
  }, [range, now])

  const events = useAsync(
    () =>
      getMemberEvents({
        limit,
        group: groupFilter === '' ? undefined : groupFilter,
        kind: kind === 'all' ? undefined : kind,
        since,
      }),
    [groupFilter, kind, since, limit],
  )

  const error = firstError(groups, events)
  const loading = isAnyLoading(events)

  const rows = useMemo(() => {
    const all = events.data?.events ?? []
    return all
      .filter((row) => matches([row.member_openid, row.group_name, String(row.group_qq_id)], search))
      .sort((a, b) => b.event_at - a.event_at)
  }, [events.data, search])

  const counts = useMemo(() => {
    let joined = 0
    let left = 0
    for (const row of rows) {
      if (row.kind === 'join') {
        joined += 1
      } else if (row.kind === 'leave') {
        left += 1
      }
    }
    return { joined, left, other: rows.length - joined - left }
  }, [rows])

  // 按天分桶：这是从真实 event_at 派生的，不是接口给的序列。所以图上的说明必须
  // 写明它只覆盖本次拉取到的这些行 —— 否则「某天 0 条」会被读成「那天没人进出」。
  const byDay = useMemo(() => {
    const buckets = new Map<string, { join: number; leave: number }>()
    for (const row of rows) {
      const key = dayKey(row.event_at)
      const bucket = buckets.get(key) ?? { join: 0, leave: 0 }
      if (row.kind === 'join') {
        bucket.join += 1
      } else if (row.kind === 'leave') {
        bucket.leave += 1
      }
      buckets.set(key, bucket)
    }
    return [...buckets.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)).slice(-14)
  }, [rows])

  const columns: readonly Column<MemberEvent>[] = [
    {
      key: 'at',
      header: '时间',
      className: 'time',
      render: (row) => <span title={formatDateTime(row.event_at)}>{formatClock(row.event_at, true)}</span>,
    },
    {
      key: 'ago',
      header: '距今',
      className: 'time',
      render: (row) => <span className="muted">{formatAgo(row.event_at, now)}</span>,
    },
    {
      key: 'group',
      header: '群',
      render: (row) => (
        <span className="nowrap">{row.group_name === '' ? groupLabel(row.group_openid) : row.group_name}</span>
      ),
    },
    {
      key: 'qq',
      header: '群号',
      className: 'time',
      render: (row) => qqGroupIdLabel(row.group_qq_id),
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
    {
      key: 'kind',
      header: '类型',
      render: (row) => (
        <Badge tone={row.kind === 'join' ? 'success' : row.kind === 'leave' ? 'coral' : 'neutral'}>
          {memberEventLabel(row.kind)}
        </Badge>
      ),
    },
  ]

  function handleExport(): void {
    downloadCsv(
      `member-events-${String(unixNow())}.csv`,
      ['时间', '群', '群号', '成员 openid', '类型', '群 openid'],
      rows.map((row) => [
        formatDateTime(row.event_at),
        row.group_name === '' ? groupLabel(row.group_openid) : row.group_name,
        qqGroupIdLabel(row.group_qq_id),
        row.member_openid,
        memberEventLabel(row.kind),
        row.group_openid,
      ]),
    )
  }

  const scanned = events.data?.scanned ?? 0
  const truncated = events.data?.truncated === true

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">成员事件</h1>
          <div className="page-sub">进出流水（member_events）：谁进来了、谁走了、机器人读到了多少条</div>
        </div>
        <div className="page-actions">
          {loading ? <Spinner /> : null}
          <button type="button" className="btn btn-outline" onClick={handleExport} disabled={rows.length === 0}>
            导出 CSV
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => {
              refreshAll(groups, events)
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
                refreshAll(groups, events)
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
        <StatCard label="本次入群" value={counts.joined} dot="on" note="当前筛选下的 join" />
        <StatCard label="本次退群" value={counts.left} dot="coral" note="当前筛选下的 leave" />
        <StatCard
          label="后端读取行数"
          value={events.data === null ? '—' : scanned}
          dot="warn"
          note={truncated ? '已读到扫描上限（这批不是全部）' : '未触到扫描上限'}
        />
        <StatCard
          label="本次返回"
          value={rows.length}
          dot="off"
          note={`limit ${String(limit)}${kind === 'all' ? '' : ` · ${memberEventLabel(kind)}`}`}
        />
      </div>

      {truncated ? (
        <Notice tone="warning" title="这批结果已经触到后端的上限">
          后端最多扫描 {scanned} 行，返回的 {rows.length} 条是其中最新的那部分 ——
          更早的事件没有被读到。缩小时间窗或换一个群再看，比把这份列表当成全部更可靠。
        </Notice>
      ) : null}

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
          <span className="fk">类型</span>
          <SegmentedControl<KindFilter> value={kind} options={KIND_OPTIONS} onChange={setKind} label="类型" />
        </span>

        <span className="filter">
          <span className="fk">时间</span>
          <SegmentedControl<RangeFilter> value={range} options={RANGE_OPTIONS} onChange={setRange} label="时间" />
        </span>

        <label className="filter">
          <span className="fk">条数</span>
          <select
            aria-label="条数"
            value={String(limit)}
            onChange={(event) => {
              setLimit(Number(event.target.value))
            }}
          >
            {LIMIT_OPTIONS.map((value) => (
              <option key={value} value={String(value)}>
                {value}
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

        <span className="spacer" />
        <span className="section-note">
          {groupFilter === '' ? '全部群' : `当前群：${groupLabel(groupFilter)}`}
          {range === 'all' ? ' · 不限时间（仍然只返回最新的 N 条）' : ''}
        </span>
      </div>

      <div className="charts is-single">
        <BarChart
          title="按天分布"
          desc={`按本地日期把本次返回的 ${rows.length} 条分桶；接口没有逐小时序列，这张图只覆盖这些行，空着的一天不等于那天没人进出`}
          legend={[
            { tone: 'success', label: '入群', value: counts.joined },
            { tone: 'primary', label: '退群', value: counts.left },
          ]}
          stacked
          columns={byDay.map<BarColumn>(([day, bucket]) => ({
            label: day,
            values: [
              { tone: 'success', value: bucket.join },
              { tone: 'primary', value: bucket.leave },
            ],
            title: `${day} · 入群 ${bucket.join} · 退群 ${bucket.leave}`,
          }))}
          emptyText="这段时间里没有成员事件"
        />
      </div>

      <div className="section-title">
        <h2>进出流水</h2>
        <span className="count-badge">{rows.length}</span>
        {events.data === null ? null : (
          <span className="section-note">
            后端扫描 {scanned} 行{truncated ? '（已到上限）' : ''} · 按时间倒序
          </span>
        )}
      </div>

      {events.data === null && events.loading ? (
        <div className="load-block">
          <Spinner label="正在读取成员事件…" />
        </div>
      ) : (
        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(row, index) => `${row.group_openid}-${row.member_openid}-${String(row.event_at)}-${String(index)}`}
          rowId={(row) => recordDomId('member-event', memberEventKey(row))}
          focusId={focusId}
          focusSeq={focus?.seq}
          empty={
            <EmptyState
              crumb="成员事件"
              message="当前筛选下没有进出记录"
              note="member_events 为空，或这个时间窗 / 这个群里没有人进出。也可能是成员事件记录功能（member_log）没有开启"
            />
          }
        />
      )}
    </>
  )
}
