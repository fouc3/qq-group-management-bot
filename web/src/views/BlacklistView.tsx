import { useEffect, useMemo, useState } from 'react'
import { getBlacklist } from '../api/endpoints'
import type { BlacklistEntry, GroupsResponse } from '../api/types'
import type { AsyncState } from '../hooks/useAsync'
import { firstError, isAnyLoading, refreshAll, useAsync } from '../hooks/useAsync'
import { useTicker } from '../hooks/useTicker'
import { downloadCsv } from '../lib/csv'
import {
  expiryNote,
  expiryState,
  expiryStateLabel,
  expiryStateTone,
  formatDateTime,
  matches,
  orDash,
  shortId,
  unixNow,
} from '../lib/format'
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

interface BlacklistProps {
  groups: AsyncState<GroupsResponse>
  groupFilter: string
  onGroupFilter: (openid: string) => void
  groupLabel: (openid: string) => string
  search: string
  /** 顶栏搜索跳过来的那一条：滚过去并高亮（见 lib/focus.ts） */
  focus?: RecordFocus | null
}

type StateFilter = 'all' | 'active' | 'expired'
type LimitFilter = '200' | '500'

const STATE_OPTIONS: readonly { value: StateFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'active', label: '生效中' },
  { value: 'expired', label: '已过期' },
]

export function BlacklistView({ groups, groupFilter, onGroupFilter, groupLabel, search, focus }: BlacklistProps) {
  const now = useTicker(60_000)
  const [state, setState] = useState<StateFilter>('all')
  const [limit, setLimit] = useState<LimitFilter>('200')

  const focusId = focus?.kind === 'blacklist' ? recordDomId('blacklist', focus.key) : undefined
  // 跳过来之后先取消「生效中 / 已过期」的筛选，否则目标可能不在列表里
  useEffect(() => {
    if (focusId === undefined) {
      return
    }
    setState('all')
  }, [focusId, focus?.seq])

  const blacklist = useAsync(
    () => getBlacklist({ group: groupFilter === '' ? undefined : groupFilter, limit: Number(limit) }),
    [groupFilter, limit],
  )

  const error = firstError(groups, blacklist)
  const loading = isAnyLoading(blacklist)

  const rows = useMemo(() => {
    const all = blacklist.data?.entries ?? []
    return all
      .filter((row) => {
        if (state === 'all') {
          return true
        }
        const current = expiryState(row.expires_at, now)
        return state === 'expired' ? current === 'expired' : current !== 'expired'
      })
      .filter((row) =>
        matches(
          [row.member_openid, row.union_openid, row.reason, row.added_by, groupLabel(row.group_openid)],
          search,
        ),
      )
      .sort((a, b) => b.added_at - a.added_at)
  }, [blacklist.data, state, search, now, groupLabel])

  const counts = useMemo(() => {
    const all = blacklist.data?.entries ?? []
    let active = 0
    let expired = 0
    let permanent = 0
    const perGroup = new Map<string, number>()
    for (const row of all) {
      const current = expiryState(row.expires_at, now)
      if (current === 'expired') {
        expired += 1
      } else {
        active += 1
      }
      if (current === 'permanent') {
        permanent += 1
      }
      const key = row.group_openid === '' ? '' : row.group_openid
      perGroup.set(key, (perGroup.get(key) ?? 0) + 1)
    }
    return { active, expired, permanent, perGroup }
  }, [blacklist.data, now])

  const distRows = useMemo(() => {
    const rows: DistRow[] = []
    for (const [openid, count] of counts.perGroup) {
      const label = openid === '' ? '全局条目' : groupLabel(openid)
      rows.push({
        label,
        value: count,
        tone: openid === '' ? 'warning' : 'primary',
        title: `${openid === '' ? '全局条目（拦所有群）' : label} · ${String(count)} 条`,
      })
    }
    return rows.sort((a, b) => b.value - a.value)
  }, [counts.perGroup, groupLabel])

  const columns: readonly Column<BlacklistEntry>[] = [
    {
      key: 'group',
      header: '范围',
      render: (row) =>
        row.group_openid === '' ? (
          <span className="row nowrap">
            <Badge tone="warn">全局</Badge>
            <span className="muted">拦所有群</span>
          </span>
        ) : (
          <span className="nowrap">{groupLabel(row.group_openid)}</span>
        ),
    },
    {
      key: 'member',
      header: '成员 openid',
      render: (row) =>
        row.member_openid === '' ? (
          <span className="muted">—（只认 union openid）</span>
        ) : (
          <span className="oid" title={row.member_openid}>
            {shortId(row.member_openid, 10, 6)}
          </span>
        ),
    },
    {
      key: 'union',
      header: 'union openid',
      render: (row) =>
        row.union_openid === '' ? (
          <span className="muted">—</span>
        ) : (
          <span className="oid" title={row.union_openid}>
            {shortId(row.union_openid, 8, 4)}
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
    { key: 'added_at', header: '加入时间', className: 'time', render: (row) => formatDateTime(row.added_at) },
    {
      key: 'expires_at',
      header: '到期',
      className: 'time',
      render: (row) => (
        <span title={row.expires_at === 0 ? 'expires_at = 0：永久条目' : formatDateTime(row.expires_at)}>
          {row.expires_at === 0 ? '永久' : formatDateTime(row.expires_at)}
          <span className="muted"> · {expiryNote(row.expires_at, now)}</span>
        </span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      className: 'col-actions',
      render: (row) => (
        <RemoveEntryButton
          kind="blacklist"
          id={row.id}
          group={row.group_openid}
          member={row.member_openid}
          onRemoved={() => { blacklist.refresh() }}
        />
      ),
    },
    {
      key: 'state',
      header: '状态',
      render: (row) => {
        const current = expiryState(row.expires_at, now)
        return <Badge tone={expiryStateTone(current)}>{expiryStateLabel(current)}</Badge>
      },
    },
  ]

  function handleExport(): void {
    downloadCsv(
      `blacklist-${String(unixNow())}.csv`,
      ['id', '范围', '群 openid', '成员 openid', 'union openid', '原因', '发起人', '加入时间', '到期时间', '状态'],
      rows.map((row) => [
        row.id,
        row.group_openid === '' ? '全局（所有群）' : groupLabel(row.group_openid),
        row.group_openid,
        row.member_openid,
        row.union_openid,
        row.reason,
        row.added_by,
        formatDateTime(row.added_at),
        row.expires_at === 0 ? '永久' : formatDateTime(row.expires_at),
        expiryStateLabel(expiryState(row.expires_at, now)),
      ]),
    )
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">黑名单</h1>
          <div className="page-sub">禁止加群名单（join_blacklist）：谁被拦在门外、因为什么、到什么时候</div>
        </div>
        <div className="page-actions">
          {loading ? <Spinner /> : null}
          <button type="button" className="btn btn-outline" onClick={handleExport} disabled={rows.length === 0}>
            导出 CSV
          </button>
          <AddEntryButton
            kind="blacklist"
            groups={groups.data?.groups ?? []}
            groupLabel={groupLabel}
            onAdded={() => { blacklist.refresh() }}
          />
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => {
              refreshAll(groups, blacklist)
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
                refreshAll(groups, blacklist)
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
          label="名单条数"
          value={blacklist.data === null ? '—' : blacklist.data.entries.length}
          dot="danger"
          decorated
          note={groupFilter === '' ? '全部群 + 全局条目' : `含全局条目 · ${groupLabel(groupFilter)}`}
        />
        <StatCard label="生效中" value={counts.active} dot="on" note="含永久条目与未到期的" />
        <StatCard label="已过期" value={counts.expired} dot="off" note="后端不按到期过滤，所以这里看得到" />
        <StatCard
          label="永久条目"
          value={counts.permanent}
          dot="warn"
          note="expires_at = 0，永不过期"
        />
      </div>

      <Notice tone="info" title="已过期的老条目也会返回">
        <code>/api/v1/blacklist</code> 不在 SQL 里按到期时间过滤，所以<b>已过期的老条目仍然会返回</b>，
        上面的「已过期」计数是本地按 <code>expires_at</code> 算的。这是真实的表内容，不是页面出错；
        不需要的条目可以直接用行内的「撤销」删掉。
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
          <span className="fk">状态</span>
          <SegmentedControl<StateFilter> value={state} options={STATE_OPTIONS} onChange={setState} label="状态" />
        </span>

        <span className="filter">
          <span className="fk">条数</span>
          <SegmentedControl<LimitFilter>
            value={limit}
            options={[
              { value: '200', label: '200' },
              { value: '500', label: '500' },
            ]}
            onChange={setLimit}
            label="条数"
          />
        </span>

        <span className="spacer" />
        <span className="section-note">显示 {rows.length} 条 · 按加入时间倒序</span>
      </div>

      <div className="charts is-single">
        <DistChart
          title="按范围分布"
          desc="每个群各自的条目，以及拦所有群的全局条目；同一个 id 只算一次"
          rows={distRows}
          emptyText="名单为空"
        />
      </div>

      <div className="section-title">
        <h2>名单</h2>
        <span className="count-badge">{rows.length}</span>
      </div>
      {blacklist.data === null && blacklist.loading ? (
        <div className="load-block">
          <Spinner label="正在读取黑名单…" />
        </div>
      ) : (
        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(row) => row.id}
          rowId={(row) => recordDomId('blacklist', row.id)}
          focusId={focusId}
          focusSeq={focus?.seq}
          scroll
          empty={
            <EmptyState
              crumb="黑名单"
              message={state === 'all' ? '黑名单为空' : '当前状态下没有条目'}
              note="join_blacklist 为空，或从未拉黑过成员。用「入群验证」页的「拉黑」按钮、或群里的「/黑名单 add」加进来的条目都会出现在这里"
            />
          }
        />
      )}
    </>
  )
}
