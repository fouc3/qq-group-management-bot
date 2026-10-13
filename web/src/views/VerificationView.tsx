import { useEffect, useMemo, useState } from 'react'
import { getPending } from '../api/endpoints'
import type { GroupsResponse, HumanVerificationStatus, PendingVerification } from '../api/types'
import type { AsyncState } from '../hooks/useAsync'
import { firstError, isAnyLoading, refreshAll, useAsync } from '../hooks/useAsync'
import { useTicker } from '../hooks/useTicker'
import { useVerificationActions } from '../hooks/useVerificationActions'
import { downloadCsv } from '../lib/csv'
import {
  formatAgo,
  formatClock,
  formatCountdown,
  formatDateTime,
  formatSpan,
  matches,
  muteModeLabel,
  orDash,
  shortId,
  unixNow,
} from '../lib/format'
import { Badge } from '../components/Badge'
import { DataTable } from '../components/DataTable'
import type { Column } from '../components/DataTable'
import { recordDomId } from '../lib/focus'
import type { RecordFocus } from '../lib/focus'
import { EmptyState } from '../components/EmptyState'
import { Notice } from '../components/Notice'
import { SegmentedControl } from '../components/SegmentedControl'
import { Spinner } from '../components/Spinner'
import { StatCard } from '../components/StatCard'
import { VerificationActionButtons } from '../components/VerificationActionButtons'

interface VerificationProps {
  groups: AsyncState<GroupsResponse>
  groupFilter: string
  onGroupFilter: (openid: string) => void
  groupLabel: (openid: string) => string
  search: string
  /** 顶栏搜索跳过来的那一条：滚过去并高亮（见 lib/focus.ts） */
  focus?: RecordFocus | null
  /**
   * 在线人机验证服务的状态（来自 /status）。
   *
   * 这里只关心一件运行时的事：服务当前连不连得上。连不上时本程序不签发链接，
   * 开了在线验证的群会退回群内按钮 —— 不说这件事，队列看上去一切正常。
   */
  humanVerification?: HumanVerificationStatus | null
}

type ModeFilter = 'all' | 'mute' | 'pseudo'
type DueFilter = 'all' | 'waiting' | 'overdue'

const MODE_OPTIONS: readonly { value: ModeFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'mute', label: '真禁言' },
  { value: 'pseudo', label: '伪禁言' },
]

const DUE_OPTIONS: readonly { value: DueFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'waiting', label: '未到期' },
  { value: 'overdue', label: '已到期' },
]

/** 这条记录要本人怎么证明自己是人（看的是他被按住时冻结的那份设置） */
function verifyChannelLabel(online: boolean | null): string {
  if (online === null) {
    return '记录未写明'
  }
  return online ? '在线验证' : '群内按钮'
}

export function VerificationView({
  groups,
  groupFilter,
  onGroupFilter,
  groupLabel,
  search,
  focus,
  humanVerification,
}: VerificationProps) {
  const now = useTicker(10_000)
  const [mode, setMode] = useState<ModeFilter>('all')
  const [due, setDue] = useState<DueFilter>('all')

  const focusId = focus?.kind === 'pending' ? recordDomId('pending', focus.key) : undefined
  // 跳过来之后先松开本页的过滤（扣留方式 / 是否到期），否则目标可能不在列表里
  useEffect(() => {
    if (focusId === undefined) {
      return
    }
    setMode('all')
    setDue('all')
  }, [focusId, focus?.seq])

  const pending = useAsync(() => getPending(), [])
  const actions = useVerificationActions(() => {
    pending.refresh()
  })
  const error = firstError(groups, pending)
  const loading = isAnyLoading(pending)

  const rows = useMemo(() => {
    const all = pending.data?.pending ?? []
    return all
      .filter((item) => groupFilter === '' || item.group_openid === groupFilter)
      .filter((item) => mode === 'all' || item.mute_mode === mode)
      .filter((item) => {
        if (due === 'all') {
          return true
        }
        const overdue = item.deadline > 0 && item.deadline <= now
        return due === 'overdue' ? overdue : !overdue
      })
      .filter((item) => matches([item.member_openid, item.token, groupLabel(item.group_openid)], search))
      .sort((a, b) => a.deadline - b.deadline)
  }, [pending.data, groupFilter, mode, due, search, now, groupLabel])

  const worst = useMemo(() => {
    const all = pending.data?.pending ?? []
    const overdue = all.filter((item) => item.deadline > 0 && item.deadline <= now).length
    const escalated = all.filter((item) => item.escalated_at > 0).length
    const pseudo = all.filter((item) => item.mute_mode === 'pseudo').length
    const soonest = all.reduce(
      (min, item) => (item.deadline > 0 && item.deadline < min ? item.deadline : min),
      Number.MAX_SAFE_INTEGER,
    )
    return { overdue, escalated, pseudo, soonest }
  }, [pending.data, now])

  const columns: readonly Column<PendingVerification>[] = [
    {
      key: 'member',
      header: '成员 openid',
      width: '230px',
      render: (row) => (
        <span className="oid" title={row.member_openid}>
          {shortId(row.member_openid, 10, 6)}
        </span>
      ),
    },
    {
      key: 'group',
      header: '群',
      width: '190px',
      render: (row) => (
        <span className="cell-clip" title={groupLabel(row.group_openid)}>
          {groupLabel(row.group_openid)}
        </span>
      ),
    },
    {
      key: 'joined',
      header: '加入时间',
      className: 'time',
      width: '210px',
      render: (row) => (
        <span className="nowrap" title={`${formatDateTime(row.joined_at)} · ${formatAgo(row.joined_at, now)}`}>
          {formatClock(row.joined_at, true)}
          <span className="muted"> · {formatAgo(row.joined_at, now)}</span>
        </span>
      ),
    },
    {
      key: 'remain',
      header: '剩余',
      render: (row) => {
        const overdue = row.deadline > 0 && row.deadline <= now
        return (
          <span className="row nowrap">
            <span className={overdue ? 'text-danger' : undefined} title={formatDateTime(row.deadline)}>
              {formatCountdown(row.deadline, now)}
            </span>
            {overdue ? <Badge tone="danger">待处理</Badge> : null}
            {row.reported ? <Badge tone="neutral">期限已处理</Badge> : null}
          </span>
        )
      },
    },
    {
      key: 'channel',
      header: '验证方式',
      render: (row) => (
        <span className="nowrap">
          {row.online_verify === true ? (
            <Badge
              tone="coral"
              title={
                '这个群走浏览器里的在线验证：卡片上是一条只有本人能点开的链接，' +
                '另有一个只有群管理员能点的「管理员跳过」。一条链接只验证一次，' +
                '要再给一次就用「重发验证」；服务不在线时新人退回群内按钮。'
              }
            >
              在线验证
            </Badge>
          ) : row.online_verify === false ? (
            <span className="muted" title="成员在群里按卡片上的按钮验证，结果直接回到机器人">
              群内按钮
            </span>
          ) : (
            <span
              className="muted"
              title={`这条记录的 settings 里没有 OnlineVerify 字段（更早写入的条目），看不出本人当初要怎么验证：\n${row.settings}`}
            >
              记录未写明
            </span>
          )}
        </span>
      ),
    },
    {
      key: 'mode',
      header: '扣留方式',
      render: (row) => (
        <span className="row nowrap">
          {row.mute_mode === '' ? (
            // 解不出来时说的话比 '—' 有用：settings 里没有 MuteMode 字段，真实
            // 扣留方式只能看 held_until（不为 0 就是平台真禁言）。
            <span
              className="muted"
              title={`这条记录的 settings 里没有 MuteMode 字段（更早写入的条目），看不出配置意图；真正被禁到什么时候请看「平台禁言」列：\n${row.settings}`}
            >
              记录未写明
            </span>
          ) : (
            <span>{orDash(muteModeLabel(row.mute_mode))}</span>
          )}
          {row.mute_mode === 'pseudo' ? (
            <Badge tone={row.left_to_talk > 0 ? 'neutral' : 'warn'}>
              {row.left_to_talk > 0 ? `还能发 ${String(row.left_to_talk)} 条` : '已转真禁言'}
            </Badge>
          ) : null}
          {row.escalated_at > 0 ? (
            <Badge tone="warn" title={`${formatDateTime(row.escalated_at)} 已升级为真禁言`}>
              已升级
            </Badge>
          ) : null}
          {row.deadline_hours > 0 ? <span className="muted">期限 {row.deadline_hours}h</span> : null}
        </span>
      ),
    },
    {
      key: 'hold',
      header: '平台禁言',
      render: (row) =>
        row.held_until === 0 ? (
          <span className="row nowrap">
            <Badge tone="neutral">无独立到期</Badge>
            <span className="muted">等到验证为止</span>
          </span>
        ) : (
          <span className="nowrap" title={formatDateTime(row.held_until)}>
            {formatDateTime(row.held_until)}
          </span>
        ),
    },
    {
      key: 'actions',
      header: '操作',
      className: 'col-actions',
      render: (row) => (
        <VerificationActionButtons
          token={row.token}
          busy={actions.busyToken === row.token}
          actions={actions}
          online={row.online_verify === true}
        />
      ),
    },
  ]

  function handleExport(): void {
    downloadCsv(
      `pending-verifications-${String(unixNow())}.csv`,
      [
        'token',
        '群',
        '群 openid',
        '成员 openid',
        '加入时间',
        '到期时间',
        '剩余秒数',
        '扣留方式',
        '验证方式',
        '期限(小时)',
        '还能发几条',
        '平台禁言到',
        '升级时间',
        '期限已处理',
      ],
      rows.map((row) => [
        row.token,
        groupLabel(row.group_openid),
        row.group_openid,
        row.member_openid,
        formatDateTime(row.joined_at),
        formatDateTime(row.deadline),
        String(Math.max(0, row.deadline - now)),
        orDash(muteModeLabel(row.mute_mode)),
        verifyChannelLabel(row.online_verify),
        String(row.deadline_hours),
        String(row.left_to_talk),
        row.held_until === 0 ? '等到验证为止' : formatDateTime(row.held_until),
        row.escalated_at === 0 ? '' : formatDateTime(row.escalated_at),
        row.reported ? '是' : '否',
      ]),
    )
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">入群验证</h1>
          <div className="page-sub">等待验证的成员（pending_verifications）：谁被扣着、还剩多久、机器人打算怎么放行</div>
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
              refreshAll(groups, pending)
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
                refreshAll(groups, pending)
              }}
            >
              重试
            </button>
          }
        >
          {error.message}
        </Notice>
      ) : null}

      {humanVerification?.configured === true && !humanVerification.connected ? (
        <Notice tone="warning" title="在线验证服务当前未连接">
          开了在线验证的群现在签发不出链接：服务方每条链接只推一次，签了没人听就等于把成员按住不放，
          所以本程序宁可退回群内按钮。新成员与这里的「重发验证」都会走群内按钮 —— 扣留不断，成员仍有出路；
          连接恢复之后新发出去的卡片会重新走链接。
          <br />
          地址、令牌与有效期在「配置」页：地址写错、令牌不对、对方服务没起来，都会长这个样子。
        </Notice>
      ) : null}

      <div className="metrics">
        <StatCard
          label="队列长度"
          value={pending.data === null ? '—' : pending.data.pending.length}
          dot="warn"
          decorated
          note={groupFilter === '' ? '全部群' : `当前群：${groupLabel(groupFilter)}`}
        />
        <StatCard
          label="最快到期"
          value={worst.soonest === Number.MAX_SAFE_INTEGER ? '—' : formatSpan(Math.max(0, worst.soonest - now))}
          dot={worst.overdue > 0 ? 'danger' : 'off'}
          note={worst.overdue > 0 ? `其中 ${worst.overdue} 条已到期待处理` : '没有已到期的'}
        />
        <StatCard label="伪禁言扣留" value={worst.pseudo} dot="coral" note="靠撤回消息，不真禁言" />
        <StatCard label="已升级真禁言" value={worst.escalated} dot="danger" note="伪禁言期间继续发言触发的" />
      </div>

      {actions.error ? (
        <Notice
          tone="error"
          title={`动作没有完成（${actions.error.code}）`}
          actions={
            <button
              type="button"
              className="btn btn-outline btn-xs"
              onClick={() => {
                actions.dismiss()
              }}
            >
              知道了
            </button>
          }
        >
          {actions.error.message}
        </Notice>
      ) : null}

      {actions.result ? (
        <Notice
          tone={actions.result.action === 'reject' ? 'warning' : 'success'}
          title={`已处理：${actions.result.member_openid}`}
          actions={
            <button
              type="button"
              className="btn btn-outline btn-xs"
              onClick={() => {
                actions.dismiss()
              }}
            >
              知道了
            </button>
          }
        >
          {actions.result.message}　队列还剩 {actions.result.pending} 条。
        </Notice>
      ) : null}

      <Notice tone="warning" title="机器人无法踢出群成员">
        如果需要移除成员，需要手动踢出：这里的「拉黑」只写黑名单并关闭验证记录，人还在群里。
        <br />
        队列里出现「已到期待处理」是正常的中间态：期限到点后由功能模块按巡检处理，处理完这条记录就从队列里消失。
        另外，{'\u00a0'}
        <code>held_until</code> 与 <code>deadline</code> 是两件事：前者是真实的禁言到期时间（伪禁言为 0，因为它一直持续到验证为止），
        后者是验证期限。后端只返回扣留尚未结束的行。
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
          <span className="fk">扣留方式</span>
          <SegmentedControl<ModeFilter> value={mode} options={MODE_OPTIONS} onChange={setMode} label="扣留方式" />
        </span>

        <span className="filter">
          <span className="fk">期限</span>
          <SegmentedControl<DueFilter> value={due} options={DUE_OPTIONS} onChange={setDue} label="期限" />
        </span>

        <span className="spacer" />
        <span className="section-note">
          显示 {rows.length} 条 · 按剩余时间升序 · 倒计时每 10 秒刷新
        </span>
      </div>

      {pending.data === null && pending.loading ? (
        <div className="load-block">
          <Spinner label="正在读取待验证队列…" />
        </div>
      ) : (
        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(row) => row.token}
          rowId={(row) => recordDomId('pending', row.token)}
          focusId={focusId}
          focusSeq={focus?.seq}
          className="queue"
          scroll
          empty={
            <EmptyState
              crumb="待验证队列"
              message="没有等待验证的成员"
              note="pending_verifications 为空。开启入群验证（join_verification）后，新成员会出现在这里"
            />
          }
        />
      )}
    </>
  )
}
