import { useEffect, useMemo, useState } from 'react'
import { getBlacklist, getHighRisk, getJudgements } from '../api/endpoints'
import type {
  BlacklistEntry,
  GroupsResponse,
  HighRiskWatch,
  Judgement,
  JudgementRecall,
} from '../api/types'
import type { AsyncState } from '../hooks/useAsync'
import { firstError, isAnyLoading, refreshAll, useAsync } from '../hooks/useAsync'
import { downloadCsv } from '../lib/csv'
import {
  actionLabel,
  actionTone,
  categoryDisplay,
  categoryTone,
  formatClock,
  formatDateTime,
  formatDuration,
  matches,
  orDash,
  shortId,
  unixNow,
  verdictLabel,
  verdictTone,
} from '../lib/format'
import type { RecordFocus } from '../lib/focus'
import { Badge } from '../components/Badge'
import { DataTable } from '../components/DataTable'
import type { Column } from '../components/DataTable'
import { EmptyState } from '../components/EmptyState'
import { Modal } from '../components/Modal'
import { Notice } from '../components/Notice'
import { SegmentedControl } from '../components/SegmentedControl'
import { Spinner } from '../components/Spinner'

interface JudgementsViewProps {
  groups: AsyncState<GroupsResponse>
  groupFilter: string
  onGroupFilter: (openid: string) => void
  groupLabel: (openid: string) => string
  search: string
  /** 顶栏搜索跳过来的那一条判定：直接打开它的详情 */
  focus?: RecordFocus | null
}

type CategoryFilter = 'all' | 'ad' | 'nsfw' | 'abuse' | 'other'
type VerdictFilter = 'all' | 'violation' | 'ok' | 'error'
type RangeFilter = '24h' | '7d' | 'all'

const CATEGORY_OPTIONS: readonly { value: CategoryFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'ad', label: '广告' },
  { value: 'nsfw', label: '色情' },
  { value: 'abuse', label: '辱骂' },
  { value: 'other', label: '其他' },
]

const VERDICT_OPTIONS: readonly { value: VerdictFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'violation', label: '违规' },
  { value: 'ok', label: '放行' },
  { value: 'error', label: '错误' },
]

const RANGE_OPTIONS: readonly { value: RangeFilter; label: string }[] = [
  { value: '24h', label: '近 24 小时' },
  { value: '7d', label: '近 7 天' },
  { value: 'all', label: '不限时间（上限 90 天）' },
]

/**
 * 时间窗对应的 since。
 *
 * 「不限时间」必须**显式传 0**，不能省掉 since：后端 `sinceOf` 在参数缺席时用的
 * 是**默认窗口 24 小时**，所以「不传」和「不限」是两件完全不同的事 —— 一个把结果
 * 限制在一天内，另一个才是尽可能早（后端把过老的 since 收到 90 天上限）。
 * 这个坑的第一版就是省掉 since，于是「不限时间」和「近 24 小时」返回同样的东西。
 */
const RANGE_SECONDS: Record<RangeFilter, number> = {
  '24h': 24 * 3600,
  '7d': 7 * 24 * 3600,
  // 0 是 Unix 纪元的起点：后端会把它夹到自己的 maxWindow（90 天）
  all: 0,
}

/** 导出当前筛选结果（纯本地生成 CSV，不调用任何接口） */
function exportJudgements(rows: readonly Judgement[], groupLabel: (openid: string) => string): void {
  const header = [
    'id',
    '群',
    '类别',
    '判定',
    '模型',
    '被判定人',
    '发起人',
    '处置',
    '禁言秒数',
    '消息 id',
    '理由',
    '生成时间',
  ]
  const body = rows.map((row) => [
    row.id,
    groupLabel(row.group_openid),
    categoryDisplay(row.category, row.category_label),
    verdictLabel(row.verdict),
    row.model,
    row.subject_openid,
    row.reporter_openid === '' ? '系统自动' : row.reporter_openid,
    actionLabel(row.action),
    String(row.mute_seconds),
    row.message_ids.join(' '),
    row.reason,
    formatDateTime(row.created_at),
  ])
  downloadCsv(`judgements-${String(unixNow())}.csv`, header, body)
}

function RecallList({ recalls }: { recalls: readonly JudgementRecall[] }) {
  return (
    <div className="recall-list">
      {recalls.map((recall) => (
        <div className="recall-item" key={recall.id}>
          <Badge tone={recall.recalled ? 'success' : 'warn'}>{recall.recalled ? '已撤回' : '未撤回'}</Badge>
          <span className="rtext" title={recall.text}>
            {recall.text === '' ? '（无文本）' : recall.text}
          </span>
          {recall.reason === '' ? null : <span className="muted nowrap">{recall.reason}</span>}
        </div>
      ))}
    </div>
  )
}

/** 详情卡上那一行「处理结果」：处置动作，带上禁言时长 */
function processResult(row: Judgement): string {
  const action = actionLabel(row.action)
  if (row.mute_seconds > 0) {
    return `${action} · 禁言 ${formatDuration(row.mute_seconds)}`
  }
  return action
}

/** 悬浮窗里的全量信息：卡片上放不下的都在这里 */
function JudgementDetail({ row, groupLabel }: { row: Judgement; groupLabel: (openid: string) => string }) {
  return (
    <div className="kv">
      <div>
        <div className="k">模型</div>
        <div className="v code">{orDash(row.model)}</div>
      </div>
      <div>
        <div className="k">禁言时长</div>
        <div className="v code">{formatDuration(row.mute_seconds)}</div>
      </div>
      <div>
        <div className="k">QQ 群</div>
        <div className="v">{groupLabel(row.group_openid)}</div>
      </div>
      <div>
        <div className="k">发起人</div>
        <div className="v oid">{row.reporter_openid === '' ? '系统自动' : row.reporter_openid}</div>
      </div>
      <div className="kv-span2">
        <div className="k">被判定人 openid</div>
        <div className="v oid">{orDash(row.subject_openid)}</div>
      </div>
      <div className="kv-span2">
        <div className="k">理由</div>
        <div className="reason">{orDash(row.reason)}</div>
      </div>
      {row.reasoning === '' ? null : (
        <div className="kv-span2">
          <div className="k">判定推理</div>
          <div className="reason">{row.reasoning}</div>
        </div>
      )}
      <div className="kv-span2">
        <div className="k">关联消息 id</div>
        {/* 这些 id 每条近百字符，一行铺开会把弹窗撑成一堵墙：一行一条、限高可滚 */}
        {row.message_ids.length === 0 ? (
          <div className="v oid">—</div>
        ) : (
          <div className="v oid idlist">
            {row.message_ids.map((id) => (
              <div key={id} title={id}>
                {id}
              </div>
            ))}
          </div>
        )}
      </div>
      {row.recalls.length === 0 ? null : (
        <div className="kv-span2">
          <div className="k">撤回记录</div>
          <RecallList recalls={row.recalls} />
        </div>
      )}
      {row.recall_reason === '' ? null : (
        <div className="kv-span2">
          <div className="k">撤回说明</div>
          <div className="reason">{row.recall_reason}</div>
        </div>
      )}
      <div className="kv-span2">
        <div className="k">群 openid</div>
        <div className="v oid">{row.group_openid}</div>
      </div>
      <div className="kv-span2">
        <div className="k">判定 id · 生成时间</div>
        <div className="v code">
          {row.id} · {formatDateTime(row.created_at)}
        </div>
      </div>
    </div>
  )
}

export function JudgementsView({
  groups,
  groupFilter,
  onGroupFilter,
  groupLabel,
  search,
  focus,
}: JudgementsViewProps) {
  const [category, setCategory] = useState<CategoryFilter>('all')
  const [verdict, setVerdict] = useState<VerdictFilter>('all')
  const [range, setRange] = useState<RangeFilter>('24h')
  // 被点开的那一条：null = 没有弹窗。只存 id 会多一次查找，存对象最省事。
  const [detail, setDetail] = useState<Judgement | null>(null)

  // 顶栏搜索里点了一条判定，就把它的详情直接开出来。
  // 记录跟着 focus 一起来，不靠 id 去本页列表里找：索引读 200 条，而这一页一次
  // 只列 50 条，老的那条很可能根本不在列表里 —— 那就成了「跳过去了但什么都没有」。
  useEffect(() => {
    if (focus === undefined || focus === null || focus.kind !== 'judgement') {
      return
    }
    const carried = focus.record
    if (carried !== undefined && 'verdict' in carried) {
      setDetail(carried)
    }
  }, [focus])

  // 时间基准只在挂载时取一次，避免每次渲染都变导致重复请求
  const [now] = useState(() => unixNow())
  // 永远传一个有值的 since：0 表示「尽可能早」，缺席表示「后端默认窗口（24 小时）」
  const since = useMemo(() => {
    const span = RANGE_SECONDS[range]
    return span === 0 ? 0 : now - span
  }, [range, now])

  const judgements = useAsync(() => getJudgements({ group: groupFilter === '' ? undefined : groupFilter, since, limit: 50 }), [
    groupFilter,
    since,
  ])
  const highRisk = useAsync(() => getHighRisk(), [])
  const blacklist = useAsync(
    () => getBlacklist({ group: groupFilter === '' ? undefined : groupFilter, limit: 200 }),
    [groupFilter],
  )

  const error = firstError(groups, judgements, highRisk, blacklist)
  const loading = isAnyLoading(judgements, highRisk, blacklist)
  // 后端：没指定群、且 config.yaml 里也没有群 → 这个问题问不出来（不是「没有违规」）
  const groupRequired = judgements.data?.group_required === true

  const rows = useMemo(() => {
    const all = judgements.data?.judgements ?? []
    return all
      .filter((row) => category === 'all' || row.category === category)
      .filter((row) => verdict === 'all' || row.verdict === verdict)
      .filter((row) =>
        matches([row.subject_openid, row.reporter_openid, row.reason, row.id, groupLabel(row.group_openid)], search),
      )
      .sort((a, b) => b.created_at - a.created_at)
  }, [judgements.data, category, verdict, search, groupLabel])

  const watchRows = useMemo(() => {
    const all = highRisk.data?.watches ?? []
    return all.filter((row) => matches([row.member_openid, row.reason, groupLabel(row.group_openid)], search))
  }, [highRisk.data, search, groupLabel])

  const blacklistRows = useMemo(() => {
    const all = blacklist.data?.entries ?? []
    return all.filter((row) =>
      matches([row.member_openid, row.union_openid, row.reason, groupLabel(row.group_openid)], search),
    )
  }, [blacklist.data, search, groupLabel])

  const watchColumns: readonly Column<HighRiskWatch>[] = [
    {
      key: 'group',
      header: '群',
      render: (row) => <span className="nowrap">{groupLabel(row.group_openid)}</span>,
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
    { key: 'added_by', header: '发起人', render: (row) => <span className="mono-sm">{orDash(row.added_by)}</span> },
    { key: 'added_at', header: '加入时间', className: 'time', render: (row) => formatDateTime(row.added_at) },
    { key: 'expires_at', header: '到期', className: 'time', render: (row) => formatDateTime(row.expires_at) },
  ]

  const blacklistColumns: readonly Column<BlacklistEntry>[] = [
    {
      key: 'group',
      header: '群',
      render: (row) => <span className="nowrap">{groupLabel(row.group_openid)}</span>,
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
      key: 'union',
      header: 'union openid',
      render: (row) => (
        <span className="oid" title={row.union_openid}>
          {row.union_openid === '' ? '—' : shortId(row.union_openid, 8, 4)}
        </span>
      ),
    },
    { key: 'reason', header: '原因', render: (row) => orDash(row.reason) },
    { key: 'added_by', header: '发起人', render: (row) => <span className="mono-sm">{orDash(row.added_by)}</span> },
    { key: 'added_at', header: '加入时间', className: 'time', render: (row) => formatDateTime(row.added_at) },
    { key: 'expires_at', header: '到期', className: 'time', render: (row) => formatDateTime(row.expires_at) },
  ]

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">违规判定</h1>
          <div className="page-sub">AI 判定流水、高风险在册与黑名单（判定结果只有 verdict / action，没有置信度分数）</div>
        </div>
        <div className="page-actions">
          {loading ? <Spinner /> : null}
          <button
            type="button"
            className="btn btn-outline"
            onClick={() => {
              exportJudgements(rows, groupLabel)
            }}
            disabled={rows.length === 0}
            title="导出当前筛选结果（本地生成 CSV，不调用接口）"
          >
            导出 CSV
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => {
              refreshAll(groups, judgements, highRisk, blacklist)
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
                refreshAll(groups, judgements, highRisk, blacklist)
              }}
            >
              重试
            </button>
          }
        >
          {error.message}
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
          <span className="fk">类别</span>
          <SegmentedControl<CategoryFilter>
            value={category}
            options={CATEGORY_OPTIONS}
            onChange={setCategory}
            label="类别"
          />
        </span>

        <span className="filter">
          <span className="fk">判定</span>
          <SegmentedControl<VerdictFilter>
            value={verdict}
            options={VERDICT_OPTIONS}
            onChange={setVerdict}
            label="判定"
          />
        </span>

        <span className="filter">
          <span className="fk">时间</span>
          <SegmentedControl<RangeFilter> value={range} options={RANGE_OPTIONS} onChange={setRange} label="时间" />
        </span>

        <span className="spacer" />
        <span className="section-note">
          显示 {rows.length} 条
          {judgements.data === null ? '' : ` · 拉取 ${judgements.data.judgements.length} 条（limit 50）`}
          {range === 'all' ? ' · 从最早算起（后端上限 90 天）' : ''}
          {judgements.data?.truncated === true ? ' · 某群结果已截断，这是最新的那部分' : ''}
        </span>
      </div>

      {groupRequired ? (
        <Notice tone="warning" title="无法统计：需要先指定一个群">
          判定表按群存储，没有全局查询；当前没有指定群，且 <code>config.yaml</code> 里的 <code>bot.groups</code>{' '}
          为空，所以这个问题问不出来 —— 这不代表「没有违规」。请在顶栏的「当前群」里选一个群。
        </Notice>
      ) : judgements.data === null && judgements.loading ? (
        <div className="load-block">
          <Spinner label="正在读取判定流水…" />
        </div>
      ) : rows.length === 0 ? (
        <EmptyState
          crumb="违规判定"
          message="当前筛选下没有判定记录"
          note="moderation_judgements 为空，或时间/类别过滤过窄"
        />
      ) : (
        <div className="jgrid">
          {rows.map((row) => (
            <div
              className="card jcard is-clickable"
              key={row.id}
              role="button"
              tabIndex={0}
              aria-haspopup="dialog"
              title="点击查看完整判定信息"
              onClick={() => {
                setDetail(row)
              }}
              onKeyDown={(event) => {
                if (event.key === 'Enter' || event.key === ' ') {
                  event.preventDefault()
                  setDetail(row)
                }
              }}
            >
              <div className="jhead">
                {/* 判定为「放行」的记录没有类别，画一个「—」徽标只会多一个噪声 */}
                {row.category === '' ? null : (
                  <Badge tone={categoryTone(row.category)}>{categoryDisplay(row.category, row.category_label)}</Badge>
                )}
                <Badge tone={verdictTone(row.verdict)}>{verdictLabel(row.verdict)}</Badge>
                <Badge tone={actionTone(row.action)}>{actionLabel(row.action)}</Badge>
                <span className="spacer" />
                <span className="jtime" title={formatDateTime(row.created_at)}>
                  {formatClock(row.created_at)}
                </span>
              </div>
              <div className="jbody">
                <div className="kv">
                  <div className="kv-span2">
                    <div className="k">QQ 群</div>
                    <div className="v">{groupLabel(row.group_openid)}</div>
                  </div>
                  <div className="kv-span2">
                    <div className="k">判定 id · 生成时间</div>
                    <div className="v code">
                      {row.id} · {formatDateTime(row.created_at)}
                    </div>
                  </div>
                  <div className="kv-span2">
                    <div className="k">处理结果</div>
                    <div className="v">{processResult(row)}</div>
                  </div>
                </div>
              </div>
              <div className="jfoot">
                <span>点击查看完整判定信息</span>
                <span className="spacer" />
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
                  <path d="M5 12h14" />
                  <path d="m12 5 7 7-7 7" />
                </svg>
              </div>
            </div>
          ))}
        </div>
      )}

      {detail === null ? null : (
        <Modal
          title={groupLabel(detail.group_openid)}
          desc={`判定 id ${detail.id} · ${formatDateTime(detail.created_at)}`}
          badges={
            <>
              <Badge tone={categoryTone(detail.category)}>
                {categoryDisplay(detail.category, detail.category_label)}
              </Badge>
              <Badge tone={verdictTone(detail.verdict)}>{verdictLabel(detail.verdict)}</Badge>
              <Badge tone={actionTone(detail.action)}>{actionLabel(detail.action)}</Badge>
            </>
          }
          onClose={() => {
            setDetail(null)
          }}
          footNote="这一页只读：判定结果由机器人写入，后台不修改它"
        >
          <JudgementDetail row={detail} groupLabel={groupLabel} />
        </Modal>
      )}

      <div className="section-title">
        <h2>高风险在册</h2>
        <span className="count-badge">{watchRows.length}</span>
        <span className="section-note">每条消息都会送检的成员</span>
      </div>
      {highRisk.data === null && highRisk.loading ? (
        <div className="load-block">
          <Spinner label="正在读取高风险名单…" />
        </div>
      ) : (
        <DataTable
          columns={watchColumns}
          rows={watchRows}
          rowKey={(row) => row.id}
          empty={
            <EmptyState
              crumb="高风险在册"
              message="没有高风险在册成员"
              note="high_risk_watches 为空"
            />
          }
        />
      )}

      <div className="section-title">
        <h2>黑名单</h2>
        <span className="count-badge">{blacklistRows.length}</span>
        <span className="section-note">禁止加群名单</span>
      </div>
      {blacklist.data === null && blacklist.loading ? (
        <div className="load-block">
          <Spinner label="正在读取黑名单…" />
        </div>
      ) : (
        <DataTable
          columns={blacklistColumns}
          rows={blacklistRows}
          rowKey={(row) => row.id}
          scroll
          empty={
            <EmptyState crumb="黑名单" message="黑名单为空" note="join_blacklist 为空，或从未拉黑过成员" />
          }
        />
      )}
    </>
  )
}
