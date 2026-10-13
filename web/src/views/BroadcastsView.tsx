import { useEffect, useMemo, useState } from 'react'
import { getBroadcasts } from '../api/endpoints'
import type { Broadcast, GroupsResponse } from '../api/types'
import type { AsyncState } from '../hooks/useAsync'
import { firstError, isAnyLoading, refreshAll, useAsync } from '../hooks/useAsync'
import { downloadCsv } from '../lib/csv'
import { formatAgo, formatClock, formatDateTime, matches, shortId, unixNow } from '../lib/format'
import { recordDomId } from '../lib/focus'
import type { RecordFocus } from '../lib/focus'
import { useFocusBlink } from '../hooks/useFocusBlink'
import { useFocusScroll } from '../hooks/useFocusScroll'
import { Badge } from '../components/Badge'
import { Card } from '../components/Card'
import { EmptyState } from '../components/EmptyState'
import { Notice } from '../components/Notice'
import { SegmentedControl } from '../components/SegmentedControl'
import { Spinner } from '../components/Spinner'
import { StatCard } from '../components/StatCard'

interface BroadcastsProps {
  groups: AsyncState<GroupsResponse>
  groupLabel: (openid: string) => string
  search: string
  /** 顶栏搜索跳过来的那一条：滚过去并高亮（见 lib/focus.ts） */
  focus?: RecordFocus | null
}

type FormFilter = 'all' | 'anonymous' | 'named'
type LimitFilter = '50' | '200'

const FORM_OPTIONS: readonly { value: FormFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'anonymous', label: '匿名' },
  { value: 'named', label: '署名' },
]

const WRITE_HINT =
  '发起广播的接口（POST /api/v1/broadcasts）尚未在本期契约中冻结，所以这一页只有审计，没有「新建广播」。'

/** 一条广播的目标群列表，点开才看 */
function TargetList({ broadcast, groupLabel }: { broadcast: Broadcast; groupLabel: (openid: string) => string }) {
  const [open, setOpen] = useState(false)
  if (broadcast.targets.length === 0) {
    return <span className="muted">没有记录到目标群</span>
  }
  return (
    <span className="stack">
      <button
        type="button"
        className="link-btn is-quiet"
        aria-expanded={open}
        onClick={() => {
          setOpen(!open)
        }}
      >
        {open ? '收起目标群' : `展开 ${String(broadcast.targets.length)} 个目标群`}
      </button>
      {open ? (
        <span className="detail-list">
          {broadcast.targets.map((target, index) => (
            <span className="detail-item" key={`${target.group_openid}-${target.message_id}-${String(index)}`}>
              <span className="idx">{index + 1}</span>
              <span>{groupLabel(target.group_openid)}</span>
              <span title={target.message_id}>
                {target.message_id === '' ? '（消息 id 未记录）' : `msg ${shortId(target.message_id, 10, 4)}`}
              </span>
            </span>
          ))}
        </span>
      ) : null}
    </span>
  )
}

export function BroadcastsView({ groups, groupLabel, search, focus }: BroadcastsProps) {
  const [form, setForm] = useState<FormFilter>('all')
  const [limit, setLimit] = useState<LimitFilter>('50')
  const [now] = useState(() => unixNow())

  const focusId = focus?.kind === 'broadcast' ? recordDomId('broadcast', focus.key) : undefined
  // 滚到那一条；顺带把本页过滤放宽（默认只读 50 条、只看匿名/署名），
  // 否则目标可能根本不在列表里
  useFocusScroll(focusId, focus?.seq)
  const blink = useFocusBlink(focusId, focus?.seq)
  useEffect(() => {
    if (focusId === undefined) {
      return
    }
    setForm('all')
    setLimit('200')
  }, [focusId, focus?.seq])

  const broadcasts = useAsync(() => getBroadcasts(Number(limit)), [limit])
  const error = firstError(groups, broadcasts)
  const loading = isAnyLoading(broadcasts)

  const rows = useMemo(() => {
    const all = broadcasts.data?.broadcasts ?? []
    return all
      .filter((row) => {
        if (form === 'all') {
          return true
        }
        return form === 'anonymous' ? row.anonymous : !row.anonymous
      })
      .filter((row) =>
        matches([row.sender_openid, row.content, row.token, groupLabel(row.targets[0]?.group_openid ?? '')], search),
      )
      .sort((a, b) => b.sent_at - a.sent_at)
  }, [broadcasts.data, form, search, groupLabel])

  const counts = useMemo(() => {
    const all = broadcasts.data?.broadcasts ?? []
    const senders = new Set<string>()
    let anonymous = 0
    let deliveries = 0
    for (const row of all) {
      if (row.anonymous) {
        anonymous += 1
      }
      if (row.sender_openid !== '') {
        senders.add(row.sender_openid)
      }
      deliveries += row.targets.length
    }
    return { anonymous, senders: senders.size, deliveries }
  }, [broadcasts.data])

  function handleExport(): void {
    downloadCsv(
      `broadcasts-${String(unixNow())}.csv`,
      ['token', '发送时间', '发送人 openid', '匿名', '格式', '内容', '目标群数', '目标群', '消息 id'],
      rows.flatMap((row) => {
        if (row.targets.length === 0) {
          return [
            [
              row.token,
              formatDateTime(row.sent_at),
              row.sender_openid,
              row.anonymous ? '是' : '否',
              row.markdown ? 'markdown' : '纯文本',
              row.content,
              '0',
              '',
              '',
            ],
          ]
        }
        return row.targets.map((target) => [
          row.token,
          formatDateTime(row.sent_at),
          row.sender_openid,
          row.anonymous ? '是' : '否',
          row.markdown ? 'markdown' : '纯文本',
          row.content,
          String(row.targets.length),
          groupLabel(target.group_openid),
          target.message_id,
        ])
      }),
    )
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">广播审计</h1>
          <div className="page-sub">谁让机器人往哪些群发了什么（broadcasts + broadcast_targets）：匿名广播的追责入口</div>
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
              refreshAll(groups, broadcasts)
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
                refreshAll(groups, broadcasts)
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
          label="广播次数"
          value={broadcasts.data === null ? '—' : broadcasts.data.broadcasts.length}
          dot="coral"
          decorated
          note={`最多读取 ${limit} 条`}
        />
        <StatCard label="投递到群" value={counts.deliveries} dot="on" note="目标群展开后的总条数" />
        <StatCard label="匿名广播" value={counts.anonymous} dot="warn" note="群里看不到是谁发的" />
        <StatCard label="发送人" value={counts.senders} dot="off" note="去重后的 openid 数" />
      </div>

      <Notice tone="info" title="匿名只是对群匿名">
        {WRITE_HINT}
        <br />
        广播记录里始终有 <code>sender_openid</code>，<code>anonymous</code> 只影响群里那条消息的落款。
        这条审计记录的用途就是这个：群里看不到谁发的，这里看得到。
      </Notice>

      <div className="filterbar">
        <span className="filter">
          <span className="fk">形式</span>
          <SegmentedControl<FormFilter> value={form} options={FORM_OPTIONS} onChange={setForm} label="形式" />
        </span>
        <span className="filter">
          <span className="fk">条数</span>
          <SegmentedControl<LimitFilter>
            value={limit}
            options={[
              { value: '50', label: '50' },
              { value: '200', label: '200' },
            ]}
            onChange={setLimit}
            label="条数"
          />
        </span>
        <span className="spacer" />
        <span className="section-note">显示 {rows.length} 条 · 按发送时间倒序</span>
      </div>

      {broadcasts.data?.unrestricted === true ? (
        <Notice tone="warning" title="读不出广播记录：bot.groups 为空">
          广播记录是按目标群读的，没有群名单就无法按群读，所以后端返回 <code>unrestricted: true</code> 和空列表
          —— 这不代表没人发过广播。请在 <code>config.yaml</code> 里写上群 openid。
        </Notice>
      ) : null}

      {broadcasts.data === null && broadcasts.loading ? (
        <div className="load-block">
          <Spinner label="正在读取广播审计…" />
        </div>
      ) : rows.length === 0 ? (
        <EmptyState
          crumb="广播审计"
          message={form === 'all' ? '没有广播记录' : '当前筛选下没有广播'}
          note="broadcasts 为空，或这台机器人还没被用来发过群广播"
        />
      ) : (
        <div className="stack">
          {rows.map((row) => (
            <Card
              key={row.token}
              id={recordDomId('broadcast', row.token)}
              className={`jcard${blink !== '' && recordDomId('broadcast', row.token) === blink ? ' is-focus' : ''}`}
            >
              <div className="jhead">
                {row.anonymous ? <Badge tone="warn">匿名</Badge> : <Badge tone="success">署名</Badge>}
                <Badge tone="neutral">{row.markdown ? 'markdown' : '纯文本'}</Badge>
                <Badge tone="coral">{row.targets.length} 个群</Badge>
                <span className="spacer" />
                <span className="jtime" title={formatDateTime(row.sent_at)}>
                  {formatClock(row.sent_at, true)} · {formatAgo(row.sent_at, now)}
                </span>
              </div>
              <div className="jbody">
                <div className="kv">
                  <div>
                    <div className="k">发送人 openid</div>
                    <div className="v oid">{row.sender_openid === '' ? '（未记录）' : row.sender_openid}</div>
                  </div>
                  <div>
                    <div className="k">广播 token</div>
                    <div className="v code">{row.token}</div>
                  </div>
                  <div className="kv-span2">
                    <div className="k">内容</div>
                    <div className="reason">{row.content === '' ? '（空内容）' : row.content}</div>
                  </div>
                  <div className="kv-span2">
                    <div className="k">目标群</div>
                    <TargetList broadcast={row} groupLabel={groupLabel} />
                  </div>
                </div>
              </div>
            </Card>
          ))}
        </div>
      )}

    </>
  )
}
