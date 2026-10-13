/* =========================================================================
   顶栏搜索的索引与匹配。

   搜索不是一个字符串过滤器：输入之后要给出「待选择」的候选，每条候选自带一个动作
   —— 群 → 切到那个群；记录（判定、待验证、黑名单、高风险、成员事件、广播）→ 跳到
   那一页并定位到它；命令 → 打开配置页并按它筛选；页面 → 直接过去。

   两条规则值得写在这里，因为它们决定了候选长什么样：

   1. 同一个字段在多个地方出现时只给一个动作。群 id 就是这样：搜 1076275041 时，
      每一条属于这个群的判定/成员事件都能匹配上，但把它们全列出来等于把「换个群看」
      这件事拆成五十条噪声，所以群命中之后，只靠群字段命中的记录会让位给「切换到群」。
   2. 记录本身仍然是可点的目标：搜一个判定 id 就该能把人送到那条判定上，所以一条
      记录一个候选，不合并、不折叠。
   ========================================================================= */

import type {
  BlacklistEntry,
  Broadcast,
  CommandItem,
  Group,
  HighRiskWatch,
  Judgement,
  MemberEvent,
  PendingVerification,
} from '../api/types'
import { NAV_ITEMS } from '../components/Sidebar'
import type { ViewKey } from '../components/Sidebar'
import type { FocusRecord, RecordKind } from './focus'
import { memberEventKey } from './focus'
import {
  categoryDisplay,
  formatDateTime,
  memberEventLabel,
  orDash,
  preview,
  qqGroupIdLabel,
  shortId,
  verdictLabel,
} from './format'

/* ------------------------------------------------------------------ 类型 */

export type HitAction =
  /** 切换「当前群」 */
  | { type: 'group'; openid: string }
  /** 跳到某一页 */
  | { type: 'view'; view: ViewKey }
  /** 跳到某一页并按文本筛选它 */
  | { type: 'filter'; view: ViewKey; filter: string }
  /** 跳到某条记录所在页并定位到它 */
  | { type: 'record'; kind: RecordKind; key: string }

export interface SearchHit {
  /** React key，稳定且唯一 */
  id: string
  /** 候选左边的短标签：群 / 判定 / 待验证 / 命令 / 页面 … */
  kind: string
  /** 排序权重，小的在前 */
  rank: number
  title: string
  subtitle: string
  meta: string
  /** 悬停全文（title 属性） */
  hint: string
  /** 记录所属的群；跳过去时顺手把「当前群」切到它，否则目标可能被群筛选挡住 */
  group?: string
  /** 记录自己那一份：页面窗口装不下它时（判定就是这样）页面直接用它 */
  record?: FocusRecord
  /** 群身份字段（openid / 群号 / 群名） */
  groupTerms: readonly string[]
  /** 记录自己的字段 */
  ownTerms: readonly string[]
  action: HitAction
}

export interface SearchIndexInput {
  groups: readonly Group[]
  events: readonly MemberEvent[]
  pending: readonly PendingVerification[]
  judgements: readonly Judgement[]
  watches: readonly HighRiskWatch[]
  blacklist: readonly BlacklistEntry[]
  broadcasts: readonly Broadcast[]
  commands: readonly CommandItem[]
  commandPrefix: string
  groupLabel: (openid: string) => string
}

const RANK = {
  group: 0,
  pending: 1,
  judgement: 2,
  watch: 3,
  blacklist: 4,
  'member-event': 5,
  broadcast: 6,
  command: 7,
  view: 8,
  action: 9,
} as const

/* ------------------------------------------------------------ 建索引片段 */

/**
 * 群的三个身份字段。
 *
 * 这三个也是「只靠群命中的记录」的判定依据：记录里凡是从群来的字段都从这里算出来，
 * 这样同一次搜索里「群命中」和「记录只是因为属于这个群才命中」用的是同一套词。
 */
function groupTermsOf(openid: string, qq: number, label: string): string[] {
  return [openid, qq > 0 ? String(qq) : '', qqGroupIdLabel(qq), label].filter((item) => item !== '')
}

/** 页面导航候选（空查询时先给这个列表）。 */
export function navHits(): SearchHit[] {
  return NAV_ITEMS.map((item) => ({
    id: `view:${item.key}`,
    kind: '页面',
    rank: RANK.view,
    title: item.label,
    subtitle: `${item.group} · 页面`,
    meta: `#${item.key}`,
    hint: `跳到「${item.label}」`,
    groupTerms: [],
    ownTerms: [item.label, item.key, item.group],
    action: { type: 'view', view: item.key },
  }))
}

/** 页面中文名（筛选项里用它说清是在哪一页筛）。 */
export function viewLabel(view: ViewKey): string {
  return NAV_ITEMS.find((item) => item.key === view)?.label ?? view
}

/** 「在某一页按文本筛选」的候选。 */
export function filterHit(view: ViewKey, filter: string, title?: string): SearchHit {
  return {
    id: `filter:${view}:${filter}`,
    kind: '筛选',
    rank: RANK.action,
    title: title ?? `在「${viewLabel(view)}」里筛选`,
    subtitle: `只保留含有「${filter}」的行；清除用顶栏那个「筛选」小标`,
    meta: '',
    hint: filter,
    groupTerms: [],
    ownTerms: [],
    action: { type: 'filter', view, filter },
  }
}

/* -------------------------------------------------------------- 建索引 */

export function buildHits(input: SearchIndexInput): SearchHit[] {
  const { groupLabel } = input
  const hits: SearchHit[] = []

  for (const group of input.groups) {
    const label = groupLabel(group.openid)
    hits.push({
      id: `group:${group.openid}`,
      kind: '群',
      rank: RANK.group,
      title: label,
      subtitle: group.openid,
      meta: group.qq_group_id > 0 ? `QQ ${String(group.qq_group_id)}` : '未登记群号',
      hint: `切换到群 ${label}`,
      groupTerms: [],
      ownTerms: groupTermsOf(group.openid, group.qq_group_id, label),
      action: { type: 'group', openid: group.openid },
    })
  }

  for (const row of input.pending) {
    const name = groupLabel(row.group_openid)
    hits.push({
      id: `pending:${row.token}`,
      kind: '待验证',
      rank: RANK.pending,
      title: `待验证 · ${name}`,
      subtitle: `成员 ${shortId(row.member_openid, 10, 6)} · token ${shortId(row.token, 8, 4)}`,
      meta: formatDateTime(row.joined_at),
      hint: `跳到入群验证队列里的这一条\n成员 ${row.member_openid}\ntoken ${row.token}`,
      group: row.group_openid,
      groupTerms: [...groupTermsOf(row.group_openid, 0, name)],
      record: row,
      ownTerms: [row.token, row.member_openid, '待验证', '入群验证'],
      action: { type: 'record', kind: 'pending', key: row.token },
    })
  }

  for (const row of input.judgements) {
    const name = groupLabel(row.group_openid)
    const category = categoryDisplay(row.category, row.category_label)
    hits.push({
      id: `judgement:${row.id}`,
      kind: '判定',
      rank: RANK.judgement,
      title: `${verdictLabel(row.verdict)} · ${name}`,
      subtitle: `判定 id ${row.id}`,
      meta: formatDateTime(row.created_at),
      hint: `判定 id ${row.id}\n${category} · ${orDash(row.reason)}`,
      group: row.group_openid,
      groupTerms: [...groupTermsOf(row.group_openid, 0, name)],
      record: row,
      ownTerms: [
        row.id,
        row.subject_openid,
        row.reporter_openid,
        row.category,
        row.category_label,
        row.verdict,
        row.action,
        row.reason,
        row.reasoning,
        row.model,
        ...row.message_ids,
        '判定',
        '违规判定',
        '举报',
        '回执',
      ],
      action: { type: 'record', kind: 'judgement', key: row.id },
    })
  }

  for (const row of input.watches) {
    const name = groupLabel(row.group_openid)
    hits.push({
      id: `watch:${row.id}`,
      kind: '高风险',
      rank: RANK.watch,
      title: `高风险 · ${name}`,
      subtitle: `成员 ${shortId(row.member_openid, 10, 6)} · ${orDash(preview(row.reason, 24))}`,
      meta: formatDateTime(row.expires_at),
      hint: `成员 ${row.member_openid}\n原因 ${orDash(row.reason)}`,
      group: row.group_openid,
      groupTerms: [...groupTermsOf(row.group_openid, 0, name)],
      record: row,
      ownTerms: [row.id, row.member_openid, row.reason, row.added_by, '高风险'],
      action: { type: 'record', kind: 'watch', key: row.id },
    })
  }

  for (const row of input.blacklist) {
    const name = groupLabel(row.group_openid)
    hits.push({
      id: `blacklist:${row.id}`,
      kind: '黑名单',
      rank: RANK.blacklist,
      title: `黑名单 · ${name}`,
      subtitle: `成员 ${shortId(row.member_openid, 10, 6)} · ${orDash(preview(row.reason, 24))}`,
      meta: formatDateTime(row.expires_at),
      hint: `成员 ${row.member_openid}\nunion ${orDash(row.union_openid)}\n原因 ${orDash(row.reason)}`,
      group: row.group_openid,
      groupTerms: [...groupTermsOf(row.group_openid, 0, name)],
      record: row,
      ownTerms: [row.id, row.member_openid, row.union_openid, row.reason, row.added_by, '黑名单'],
      action: { type: 'record', kind: 'blacklist', key: row.id },
    })
  }

  for (const row of input.events) {
    const name = row.group_name === '' ? groupLabel(row.group_openid) : row.group_name
    const key = memberEventKey(row)
    hits.push({
      id: `member-event:${key}`,
      kind: '成员事件',
      rank: RANK['member-event'],
      title: `${memberEventLabel(row.kind)} · ${name}`,
      subtitle: `成员 ${shortId(row.member_openid, 10, 6)}`,
      meta: formatDateTime(row.event_at),
      hint: `成员 ${row.member_openid}\n群 ${name}（${row.group_openid}）`,
      group: row.group_openid,
      groupTerms: [...groupTermsOf(row.group_openid, row.group_qq_id, name)],
      record: row,
      ownTerms: [row.member_openid, row.kind, memberEventLabel(row.kind), '成员事件', '进群', '退群'],
      action: { type: 'record', kind: 'member-event', key },
    })
  }

  for (const row of input.broadcasts) {
    const target = row.targets[0]?.group_openid ?? ''
    const name = target === '' ? '未投递到群' : groupLabel(target)
    hits.push({
      id: `broadcast:${row.token}`,
      kind: '广播',
      rank: RANK.broadcast,
      title: `广播 · ${name}`,
      subtitle: preview(row.content, 42),
      meta: formatDateTime(row.sent_at),
      hint: `token ${row.token}\n发送人 ${orDash(row.sender_openid)}\n${row.content}`,
      group: target === '' ? undefined : target,
      groupTerms: target === '' ? [] : groupTermsOf(target, 0, name),
      record: row,
      ownTerms: [
        row.token,
        row.sender_openid,
        row.content,
        ...row.targets.map((item) => item.message_id),
        '广播',
        row.anonymous ? '匿名' : '实名',
      ],
      action: { type: 'record', kind: 'broadcast', key: row.token },
    })
  }

  for (const row of input.commands) {
    hits.push({
      id: `command:${row.name}`,
      kind: '命令',
      rank: RANK.command,
      title: `${input.commandPrefix}${row.name}`,
      subtitle: `${row.usage} · ${preview(row.desc, 34)}`,
      meta: orDash(row.audience),
      hint: `${row.usage}\n${row.desc}`,
      groupTerms: [],
      ownTerms: [row.name, ...row.aliases, row.usage, row.desc, '命令'],
      action: { type: 'filter', view: 'config', filter: row.name },
    })
  }

  return hits
}

/* -------------------------------------------------------------- 匹配 */

function scoreOf(hit: SearchHit, query: string): number {
  // 0 = 整个字段就是这个词，1 = 以它开头，2 = 包含它，-1 = 没沾上
  let best = -1
  for (const term of [...hit.groupTerms, ...hit.ownTerms]) {
    const lower = term.toLowerCase()
    if (lower === query) {
      return 0
    }
    if (lower.startsWith(query)) {
      best = best === -1 ? 1 : Math.min(best, 1)
    } else if (lower.includes(query)) {
      best = best === -1 ? 2 : Math.min(best, 2)
    }
  }
  return best
}

export interface MatchResult {
  hits: SearchHit[]
  /** 匹配到但没显示的条数（超过 limit） */
  more: number
  /** 被「群命中」折叠掉的记录条数：它们只是属于那个群，动作已经在切群里了 */
  folded: number
}

/**
 * 按 q 挑候选。
 *
 * 排序是「像的程度」优先，其次才是类别：同一个词在一个判定 id 里精确命中，比它在
 * 某个群名里被包含更值得排在前面。
 */
export function matchHits(hits: readonly SearchHit[], raw: string, limit: number): MatchResult {
  const query = raw.trim().toLowerCase()
  if (query === '') {
    return { hits: [], more: 0, folded: 0 }
  }

  const scored: { hit: SearchHit; score: number }[] = []
  for (const hit of hits) {
    const score = scoreOf(hit, query)
    if (score >= 0) {
      scored.push({ hit, score })
    }
  }

  // 群命中之后，只靠群字段命中的记录让位给「切换到群」这一条
  const groupHitOpenids = new Set<string>()
  for (const item of scored) {
    if (item.hit.action.type === 'group') {
      groupHitOpenids.add(item.hit.action.openid)
    }
  }
  const kept = scored.filter((item) => {
    const { hit } = item
    if (hit.action.type === 'group') {
      return true
    }
    if (hit.group === undefined || !groupHitOpenids.has(hit.group)) {
      return true
    }
    // 自己的字段也命中了吗？没有的话这条命中只是因为「它属于这个群」
    return hit.ownTerms.some((term) => term.toLowerCase().includes(query))
  })

  kept.sort((a, b) => (a.score === b.score ? a.hit.rank - b.hit.rank : a.score - b.score))

  const visible = kept.slice(0, limit).map((item) => item.hit)
  return {
    hits: visible,
    more: Math.max(0, kept.length - visible.length),
    folded: scored.length - kept.length,
  }
}
