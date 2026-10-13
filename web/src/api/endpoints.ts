/* =========================================================================
   每个接口一个函数，形状严格对应 docs/frontend/api-contract.md 里的冻结契约。
   前端只调用这里的函数；没有出现在契约里的接口一律不摸。
   ========================================================================= */

import { request } from './client'
import type {
  BlacklistEntry,
  BlacklistResponse,
  BroadcastsResponse,
  CommandsResponse,
  ConfigResponse,
  GroupsResponse,
  HighRiskResponse,
  HighRiskWatch,
  JudgementsResponse,
  LogsResponse,
  MemberEventsResponse,
  OverviewResponse,
  PendingResponse,
  ReloadResponse,
  RemovalResponse,
  StatusResponse,
  VerificationActionResponse,
} from './types'

export function getStatus(): Promise<StatusResponse> {
  return request<StatusResponse>('/api/v1/status')
}

/** since 为 Unix 秒；概览视图传今天 0 点以得到「今日」口径 */
export function getOverview(since?: number): Promise<OverviewResponse> {
  return request<OverviewResponse>('/api/v1/overview', { query: { since } })
}

export function getGroups(): Promise<GroupsResponse> {
  return request<GroupsResponse>('/api/v1/groups')
}

export function getPending(): Promise<PendingResponse> {
  return request<PendingResponse>('/api/v1/pending')
}

/* ---------------------------------- 待验证队列的三个动作（写接口，会真的动手） */

function verificationPath(token: string, action: string): string {
  return `/api/v1/verifications/${encodeURIComponent(token)}/${action}`
}

/** 重发验证卡片：按钮还是原来那个，成员不会被重新扣留 */
export function resendVerification(token: string): Promise<VerificationActionResponse> {
  return request<VerificationActionResponse>(verificationPath(token, 'resend'), { method: 'POST' })
}

/** 立即放行：解除扣留并关闭记录，不管本人有没有按过按钮 */
export function approveVerification(token: string): Promise<VerificationActionResponse> {
  return request<VerificationActionResponse>(verificationPath(token, 'approve'), { method: 'POST' })
}

/** 拒绝：移出群聊；blacklist 为 true 时同时写进本群黑名单 */
export function rejectVerification(
  token: string,
  body: { blacklist: boolean; reason: string },
): Promise<VerificationActionResponse> {
  return request<VerificationActionResponse>(verificationPath(token, 'reject'), { method: 'POST', body })
}

export interface MemberEventQuery {
  limit?: number
  group?: string
  kind?: string
  since?: number
}

export function getMemberEvents(query: MemberEventQuery = {}): Promise<MemberEventsResponse> {
  return request<MemberEventsResponse>('/api/v1/member-events', {
    query: { limit: query.limit, group: query.group, kind: query.kind, since: query.since },
  })
}

export interface JudgementQuery {
  group?: string
  since?: number
  limit?: number
}

export function getJudgements(query: JudgementQuery = {}): Promise<JudgementsResponse> {
  return request<JudgementsResponse>('/api/v1/judgements', {
    query: { group: query.group, since: query.since, limit: query.limit },
  })
}

export interface BlacklistQuery {
  group?: string
  limit?: number
}

export function getBlacklist(query: BlacklistQuery = {}): Promise<BlacklistResponse> {
  return request<BlacklistResponse>('/api/v1/blacklist', {
    query: { group: query.group, limit: query.limit },
  })
}

export interface BlacklistInput {
  group_openid: string
  member_openid: string
  union_openid: string
  reason: string
  /** Unix 秒；0 = 永久 */
  expires_at: number
}

/** 加一条黑名单（真的写库） */
export function addBlacklist(input: BlacklistInput): Promise<BlacklistEntry> {
  return request<BlacklistEntry>('/api/v1/blacklist', { method: 'POST', body: input })
}

/** 撤销一条黑名单；group 是「谁在撤销」，全局条目传空串 */
export function removeBlacklist(id: string, group: string): Promise<RemovalResponse> {
  return request<RemovalResponse>(`/api/v1/blacklist/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    query: { group },
  })
}

export interface WatchInput {
  group_openid: string
  member_openid: string
  reason: string
  /** Unix 秒，必填且不能为 0 */
  expires_at: number
}

/** 标记高风险（真的让功能把这个人列进内存名单） */
export function addWatch(input: WatchInput): Promise<HighRiskWatch> {
  return request<HighRiskWatch>('/api/v1/high-risk', { method: 'POST', body: input })
}

/** 撤销高风险标记；返回 removed=false 表示本来就不在名单上 */
/**
 * 撤销高风险标记。
 *
 * group 走查询参数而不是路径段：不标群的历史条目 group_openid 是空串，而路径里
 * 的空段会被规范化掉（`/high-risk//成员` → `/high-risk/成员`），最后落到「没有这个
 * 接口」—— 查询参数能原样携带空值。
 */
export function removeWatch(group: string, member: string): Promise<RemovalResponse> {
  return request<RemovalResponse>('/api/v1/high-risk', { method: 'DELETE', query: { group, member } })
}

export function getHighRisk(): Promise<HighRiskResponse> {
  return request<HighRiskResponse>('/api/v1/high-risk')
}

export function getBroadcasts(limit = 50): Promise<BroadcastsResponse> {
  return request<BroadcastsResponse>('/api/v1/broadcasts', { query: { limit } })
}

export function getCommands(): Promise<CommandsResponse> {
  return request<CommandsResponse>('/api/v1/commands')
}

export function getConfig(): Promise<ConfigResponse> {
  return request<ConfigResponse>('/api/v1/config')
}

export function reloadConfig(): Promise<ReloadResponse> {
  return request<ReloadResponse>('/api/v1/config/reload', { method: 'POST' })
}

/* ------------------------------------------------------------------- logs */

/** 日志流地址。用 readEventStream 消费，不能用 EventSource（它带不了 token 头）。 */
export const LOG_STREAM_PATH = '/api/v1/logs/stream'

export interface LogQuery {
  limit?: number
  /** debug / info / warn / error（不区分大小写；不认识的取值会被后端当成 info） */
  level?: string
  /** 只要 seq 大于它的记录；省略 = 只要最新的那批，0 = 要缓冲区里的全部 */
  after?: number
}

export function getLogs(query: LogQuery = {}): Promise<LogsResponse> {
  return request<LogsResponse>('/api/v1/logs', {
    query: { limit: query.limit, level: query.level, after: query.after },
  })
}
