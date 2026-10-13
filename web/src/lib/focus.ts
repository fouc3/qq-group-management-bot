/* =========================================================================
   「从搜索结果跳到某一条记录」要用到的定位信息。

   顶栏搜索给出的是候选（群 / 记录 / 页面），选中一条记录之后要把人带到那一页、并在
   那一页里把这条记录滚进视野、标出来。这里只放三件事：记录种类、它自己的 key、
   以及这个 key 对应到页面里的 DOM id —— 两侧必须用同一个函数算，否则高亮永远找不到。
   ========================================================================= */

import type {
  BlacklistEntry,
  Broadcast,
  HighRiskWatch,
  Judgement,
  MemberEvent,
  PendingVerification,
} from '../api/types'
import type { ViewKey } from '../components/Sidebar'

/** 能被「跳过去」的记录种类；每一种都有自己的页面。 */
export type RecordKind = 'pending' | 'judgement' | 'watch' | 'blacklist' | 'member-event' | 'broadcast'

/** 记录本身。页面列表里可能没有它（时间窗、条数上限），所以带着一份。 */
export type FocusRecord =
  | Judgement
  | MemberEvent
  | PendingVerification
  | BlacklistEntry
  | HighRiskWatch
  | Broadcast

export interface RecordFocus {
  kind: RecordKind
  /** 记录自己的 key：判定 id / token / 黑名单 id / 成员事件复合键 */
  key: string
  /** 每跳一次加一：同一个目标连点两次也要重新滚一次、再闪一次 */
  seq: number
  /** 索引里那一份记录；判定详情在页面窗口外时会用到 */
  record?: FocusRecord
}

const VIEW_OF: Record<RecordKind, ViewKey> = {
  pending: 'join-verification',
  judgement: 'judgements',
  watch: 'high-risk',
  blacklist: 'blacklist',
  'member-event': 'member-events',
  broadcast: 'broadcasts',
}

/** 这种记录在哪一页。 */
export function viewOfRecord(kind: RecordKind): ViewKey {
  return VIEW_OF[kind]
}

/** 记录在页面里的 DOM id。搜索侧和页面侧都调它。 */
export function recordDomId(kind: RecordKind, key: string): string {
  return `rec-${kind}-${key.replace(/[^A-Za-z0-9_-]/g, '_')}`
}

/**
 * 成员事件的 key。
 *
 * 这张表没有主键返回给接口（后端按时间倒序给行），而「同一个人同一秒进出」不是不可能，
 * 所以四个字段一起当 key；页面里的行和搜索命中都调这一个函数。
 */
export function memberEventKey(row: Pick<MemberEvent, 'group_openid' | 'member_openid' | 'event_at' | 'kind'>): string {
  return `${row.group_openid}|${row.member_openid}|${String(row.event_at)}|${row.kind}`
}
