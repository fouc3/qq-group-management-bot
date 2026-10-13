import { useMemo } from 'react'
import { getMemberEvents, getOverview, getPending } from '../api/endpoints'
import type {
  GroupsResponse,
  MemberEvent,
  OverviewResponse,
  PendingVerification,
  StatusResponse,
} from '../api/types'
import type { AsyncState } from '../hooks/useAsync'
import { firstError, isAnyLoading, refreshAll, useAsync } from '../hooks/useAsync'
import { useTicker } from '../hooks/useTicker'
import { useVerificationActions } from '../hooks/useVerificationActions'
import {
  featureLabel,
  formatBytes,
  formatClock,
  formatCountdown,
  formatSpan,
  matches,
  memberEventLabel,
  muteModeLabel,
  orDash,
  qqGroupIdLabel,
  shortId,
  startOfTodayUnix,
} from '../lib/format'
import { BarChart } from '../components/BarChart'
import { Badge } from '../components/Badge'
import { Card } from '../components/Card'
import { CopyChip } from '../components/CopyChip'
import { DataTable } from '../components/DataTable'
import type { Column } from '../components/DataTable'
import { DistChart } from '../components/DistChart'
import { EmptyState } from '../components/EmptyState'
import { Notice } from '../components/Notice'
import { Spinner } from '../components/Spinner'
import { StatCard } from '../components/StatCard'
import { VerificationActionButtons } from '../components/VerificationActionButtons'
import type { ViewKey } from '../components/Sidebar'

interface OverviewViewProps {
  status: AsyncState<StatusResponse>
  groups: AsyncState<GroupsResponse>
  groupFilter: string
  groupLabel: (openid: string) => string
  search: string
  onNavigate: (key: ViewKey) => void
}

const OPERATIONS_HINT =
  '机器人无法踢出群成员：「拉黑」只写黑名单并关闭验证记录，要移除本人需要手动踢出。完整的队列与说明在「入群验证」页。'

export function OverviewView({ status, groups, groupFilter, groupLabel, search, onNavigate }: OverviewViewProps) {
  const now = useTicker(30_000)

  const overview = useAsync<OverviewResponse>(() => getOverview(startOfTodayUnix()), [])
  const pending = useAsync(() => getPending(), [])
  const actions = useVerificationActions(() => {
    pending.refresh()
  })
  const events = useAsync(() => getMemberEvents({ limit: 6, group: groupFilter === '' ? undefined : groupFilter }), [
    groupFilter,
  ])

  const error = firstError(status, groups, overview, pending, events)
  const loading = isAnyLoading(overview, pending, events)
  const stats = overview.data

  const groupList = useMemo(() => {
    const all = groups.data?.groups ?? []
    return all.filter((group) => matches([group.openid, String(group.qq_group_id), groupLabel(group.openid)], search))
  }, [groups.data, search, groupLabel])

  const pendingRows = useMemo(() => {
    const all = pending.data?.pending ?? []
    return all
      .filter((item) => groupFilter === '' || item.group_openid === groupFilter)
      .filter((item) => matches([item.member_openid, groupLabel(item.group_openid)], search))
      .sort((a, b) => a.deadline - b.deadline)
  }, [pending.data, groupFilter, search, groupLabel])

  const eventRows = useMemo(() => {
    const all = events.data?.events ?? []
    return all.filter((event) => matches([event.member_openid, event.group_name, String(event.group_qq_id)], search))
  }, [events.data, search])

  const pendingColumns: readonly Column<PendingVerification>[] = [
    {
      key: 'member',
      header: '成员 openid',
      width: '260px',
      render: (row) => (
        <span className="oid" title={row.member_openid}>
          {shortId(row.member_openid, 10, 6)}
        </span>
      ),
    },
    {
      key: 'group',
      header: '群',
      render: (row) => <span className="nowrap">{groupLabel(row.group_openid)}</span>,
    },
    {
      key: 'joined',
      header: '加入时间',
      className: 'time',
      render: (row) => formatClock(row.joined_at, true),
    },
    {
      key: 'remain',
      header: '剩余',
      render: (row) => (
        <span className="row">
          <span className="nowrap">{formatCountdown(row.deadline, now)}</span>
          {row.escalated_at > 0 ? <Badge tone="warn">已升级</Badge> : null}
          {row.reported ? <Badge tone="neutral">已上报</Badge> : null}
        </span>
      ),
    },
    {
      key: 'mode',
      header: '扣留方式',
      render: (row) => (
        <span className="row nowrap">
          <span>{orDash(muteModeLabel(row.mute_mode))}</span>
          {row.mute_mode === 'pseudo' && row.left_to_talk > 0 ? (
            <Badge tone="warn" title="伪禁言下还能发这么多条，超过就转真禁言">
              还能发 {row.left_to_talk} 条
            </Badge>
          ) : null}
          {row.online_verify === true ? (
            <Badge tone="coral" title="本群走浏览器里的在线验证：卡片上是一条只有本人能点开的链接">
              在线验证
            </Badge>
          ) : null}
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

  const eventColumns: readonly Column<MemberEvent>[] = [
    {
      key: 'at',
      header: '时间',
      className: 'time',
      render: (row) => formatClock(row.event_at, true),
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

  const eventCounts = stats?.member_events ?? null
  const judgementCounts = stats?.judgements ?? null

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">概览</h1>
          <div className="page-sub">QQ 群管理机器人 · 运维控制台（数据口径：今日 00:00 起）</div>
        </div>
        <div className="page-actions">
          {loading ? <Spinner /> : null}
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => {
              refreshAll(status, groups, overview, pending, events)
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
                refreshAll(status, groups, overview, pending, events)
              }}
            >
              重试
            </button>
          }
        >
          {error.message}
        </Notice>
      ) : null}

      {status.data === null && status.error === null ? (
        <div className="load-block">
          <Spinner label="正在读取运行状态…" large />
        </div>
      ) : (
        <div className="strip">
          <div className="strip-item">
            <span className="k">admin 服务</span>
            <span className="v">
              <span className={`dot ${status.data === null ? 'is-off' : 'is-live'}`} />
              {status.data === null ? '不可用' : `在线 · ${formatSpan(status.data.uptime_seconds)}`}
            </span>
          </div>
          <div className="strip-item">
            <span className="k">运行版本</span>
            <span className="v">
              <span className="ellipsis mono">{status.data === null ? '—' : status.data.version}</span>
            </span>
          </div>
          <div className="strip-item">
            <span className="k">数据库</span>
            <span className="v">
              {status.data === null
                ? '—'
                : `${status.data.database.driver} · ${formatBytes(status.data.database.size_bytes)}`}
            </span>
          </div>
          <div className="strip-item">
            <span className="k">Redis</span>
            <span className="v">
              <span className="ellipsis mono">{status.data === null ? '—' : status.data.redis.addr}</span>
            </span>
            {status.data === null ? null : (
              <span className="k">
                db{status.data.redis.db} · 前缀 {status.data.redis.prefix}
              </span>
            )}
          </div>
          <div className="strip-item">
            <span className="k">配置文件</span>
            <span className="v">
              <span className="ellipsis mono">{status.data === null ? '—' : status.data.config_path}</span>
            </span>
          </div>
          <div className="strip-item">
            <span className="k">命令前缀 / 判定模型</span>
            <span className="v">
              {status.data === null ? '—' : `${status.data.command_prefix} · ${status.data.judged_model}`}
            </span>
          </div>
        </div>
      )}

      {status.data !== null && status.data.features.length > 0 ? (
        <Card className="mt-16" title="功能运行态" desc="来自 /api/v1/status 的 features：enabled 为配置开关，running 为进程内实际运行状态">
          <div className="switches">
            {status.data.features.map((feature) => (
              <span key={feature.name} className={`chip${feature.enabled ? '' : ' is-off'}`}>
                <span className={`dot ${feature.running ? 'is-on' : 'is-off'}`} />
                <span>{featureLabel(feature.name)}</span>
                <span className="note">
                  {feature.enabled ? '已启用' : '未启用'} · {feature.running ? '运行中' : '未运行'}
                </span>
              </span>
            ))}
          </div>
        </Card>
      ) : null}

      <div className="metrics">
        <StatCard
          label="受管群"
          value={stats === null ? '—' : stats.groups}
          note={<span className="mono-sm">bot.groups</span>}
          dot="coral"
        />
        <StatCard
          label="待验证"
          value={stats === null ? '—' : stats.pending}
          note={
            pendingRows.length === 0
              ? '队列为空'
              : `队列中最近到期：${formatCountdown(
                  pendingRows.reduce((min, row) => Math.min(min, row.deadline), Number.MAX_SAFE_INTEGER),
                  now,
                )}`
          }
          dot="warn"
          decorated
        />
        <StatCard
          label="今日成员事件"
          value={stats === null ? '—' : stats.member_events.total}
          note={
            eventCounts === null
              ? undefined
              : `入群 ${eventCounts.joined} · 退群 ${eventCounts.left} · 扫描 ${eventCounts.scanned}`
          }
          dot="on"
        />
        <StatCard
          label="高风险在册"
          value={stats === null ? '—' : stats.high_risk}
          note={stats === null ? undefined : `黑名单 ${stats.blacklist.total} 条`}
          dot="danger"
        />
      </div>

      {stats !== null && stats.notes !== undefined && stats.notes.length > 0 ? (
        <Notice tone="warning" title="计数口径（后端 notes）：哪个数字是下界、哪个问题问不出来">
          <ul className="note-list">
            {stats.notes.map((note) => (
              <li key={note}>{note}</li>
            ))}
          </ul>
        </Notice>
      ) : null}

      <div className="charts">
        <BarChart
          title="今日成员事件"
          desc={
            eventCounts === null
              ? '按类型汇总'
              : `入群 / 退群 · 共 ${eventCounts.total} 条（另扫描 ${eventCounts.scanned} 条消息）；接口未提供逐小时序列，不做伪造时间轴`
          }
          legend={[
            { tone: 'success', label: '入群', value: eventCounts?.joined },
            { tone: 'primary', label: '退群', value: eventCounts?.left },
          ]}
          columns={
            eventCounts === null
              ? []
              : [
                  {
                    label: '入群',
                    values: [{ tone: 'success', value: eventCounts.joined }],
                    title: `入群 ${eventCounts.joined}`,
                  },
                  {
                    label: '退群',
                    values: [{ tone: 'primary', value: eventCounts.left }],
                    title: `退群 ${eventCounts.left}`,
                  },
                ]
          }
          emptyText="今日还没有成员事件"
        />
        <DistChart
          title="违规判定分布"
          desc={
            judgementCounts === null
              ? '今日判定结果'
              : `今日 · 判定 ${judgementCounts.total} 条${judgementCounts.truncated ? '（每群只读了最近一批，可能还有更早的）' : ''}`
          }
          rows={
            judgementCounts === null
              ? []
              : [
                  { label: '违规', value: judgementCounts.violations, tone: 'danger' },
                  { label: '放行', value: judgementCounts.ok, tone: 'success' },
                  { label: '判定错误', value: judgementCounts.errors, tone: 'warning' },
                ]
          }
          emptyText="今日没有判定记录"
        />
      </div>

      <div className="section-title">
        <h2>受管群</h2>
        <span className="count-badge">{groupList.length}</span>
        {groupFilter === '' ? null : <span className="section-note">已按顶栏当前群筛选</span>}
      </div>
      {groups.data?.unrestricted === true ? (
        <Notice tone="warning" title="bot.groups 为空：机器人对所有加入的群生效">
          群名单没有配置，因此这里列不出具体群 —— 这不代表「没有受管群」。要按群看开关，请在{' '}
          <code>config.yaml</code> 里写上群 openid。
        </Notice>
      ) : null}
      {groupList.length === 0 ? (
        <EmptyState
          crumb="受管群"
          message={groups.data?.unrestricted === true ? '群名单为空（全群生效模式）' : '还没有受管群'}
          note={
            groups.data?.unrestricted === true
              ? 'unrestricted=true：机器人对它加入的每个群都生效'
              : 'bot.groups 为空：在 config.yaml 里配置群后触发配置重载'
          }
        />
      ) : (
        <div className="group-grid">
          {groupList.map((group) => (
            <div className="card card-hover" key={group.openid}>
              <div>
                <h3 className="group-name">{groupLabel(group.openid)}</h3>
                <div className="group-qq">QQ {qqGroupIdLabel(group.qq_group_id)}</div>
              </div>
              <CopyChip value={group.openid} />
              <div className="switches">
                {group.features.map((feature) => (
                  <span
                    key={feature.name}
                    className={`chip${feature.effective ? '' : ' is-off'}`}
                    title={`全局 ${feature.global_enabled ? '开' : '关'} · 本群覆盖 ${
                      feature.group_override === null ? '继承全局' : feature.group_override ? '开' : '关'
                    } · 生效 ${feature.effective ? '开' : '关'}`}
                  >
                    <span className={`dot ${feature.effective ? 'is-on' : 'is-off'}`} />
                    <span>{featureLabel(feature.name)}</span>
                    <span className="mark">{feature.effective ? '✓' : '✗'}</span>
                    {feature.group_override === null ? null : <span className="note">本群覆盖</span>}
                  </span>
                ))}
              </div>
              <div className="card-foot">
                <span className="spacer" />
                <button
                  type="button"
                  className="link-btn"
                  onClick={() => {
                    onNavigate('groups')
                  }}
                >
                  查看详情
                  <svg
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
                    <path d="M5 12h14" />
                    <path d="m12 5 7 7-7 7" />
                  </svg>
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      <div className="section-title">
        <h2>待验证队列</h2>
        <span className="count-badge">{pendingRows.length}</span>
        <span className="section-note">按剩余时间升序{groupFilter === '' ? '' : ' · 已按当前群筛选'}</span>
      </div>
      <Notice tone="info">{OPERATIONS_HINT}</Notice>

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

      {actions.error ? (
        <Notice tone="error" title={`动作没有完成（${actions.error.code}）`}>
          {actions.error.message}
        </Notice>
      ) : null}
      {pending.data === null && pending.loading ? (
        <div className="load-block">
          <Spinner label="正在读取待验证队列…" />
        </div>
      ) : (
        <DataTable
          columns={pendingColumns}
          rows={pendingRows}
          rowKey={(row) => row.token}
          empty={
            <EmptyState
              crumb="待验证队列"
              message="没有等待验证的成员"
              note="pending_verifications 为空。开启入群验证后，新成员会出现在这里"
            />
          }
        />
      )}

      <div className="section-title">
        <h2>最近成员事件</h2>
        <span className="count-badge">{eventRows.length}</span>
        {events.data === null ? null : (
          <span className="section-note">
            扫描 {events.data.scanned} 条{events.data.truncated ? ' · 结果已截断' : ''}
          </span>
        )}
      </div>
      {events.data === null && events.loading ? (
        <div className="load-block">
          <Spinner label="正在读取成员事件…" />
        </div>
      ) : (
        <DataTable
          columns={eventColumns}
          rows={eventRows}
          rowKey={(row, index) => `${row.group_openid}-${row.member_openid}-${row.event_at}-${index}`}
          empty={
            <EmptyState
              crumb="成员事件"
              message="没有成员进出记录"
              note="member_events 为空，或从未开启成员事件记录功能"
            />
          }
        />
      )}
    </>
  )
}
