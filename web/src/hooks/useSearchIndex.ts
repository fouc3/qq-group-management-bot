import { useEffect, useMemo, useRef, useState } from 'react'
import { toApiError } from '../api/client'
import { getBlacklist, getBroadcasts, getCommands, getHighRisk, getJudgements, getPending } from '../api/endpoints'
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
import { buildHits } from '../lib/search'
import type { SearchHit } from '../lib/search'

/**
 * 每张表拉多少条进索引。
 *
 * 搜历史就是要在历史里找到东西，所以按接口允许的上限拉；但这是有界的，下拉框底部
 * 会照实写「只索引了最新 N 条」，不能让人以为搜的就是全部。
 */
export const INDEX_LIMIT = 200

/** 索引多旧算旧：打开搜索时超过这个时间就重拉一遍（别每次聚焦都打六个请求） */
const STALE_MS = 15_000

interface IndexData {
  pending: PendingVerification[]
  judgements: Judgement[]
  watches: HighRiskWatch[]
  blacklist: BlacklistEntry[]
  broadcasts: Broadcast[]
  commands: CommandItem[]
  prefix: string
}

export interface SearchIndexState {
  hits: readonly SearchHit[]
  loading: boolean
  /** 索引没取到（后端不通、没权限）时的一句话；搜索是辅助功能，不该整页报错 */
  error: string
  /** 各表进了多少条 */
  counts: readonly { label: string; value: number }[]
}

/**
 * 打开搜索时才建索引。
 *
 * 六份列表一次拉完（都是同一个后端的本地接口），因为「输入之后立刻要有候选」这件事
 * 不允许按需请求：一个字一个请求既慢又会因为竞态给出错序的结果。
 */
export function useSearchIndex(
  open: boolean,
  input: { groups: readonly Group[]; events: readonly MemberEvent[]; groupLabel: (openid: string) => string },
): SearchIndexState {
  const [data, setData] = useState<IndexData | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const fetchedAt = useRef(0)
  const { groups, events, groupLabel } = input

  useEffect(() => {
    if (!open) {
      return
    }
    if (fetchedAt.current !== 0 && Date.now() - fetchedAt.current < STALE_MS) {
      return
    }
    let alive = true
    setLoading(true)
    Promise.all([
      getPending(),
      getJudgements({ since: 0, limit: INDEX_LIMIT }),
      getHighRisk(),
      getBlacklist({ limit: INDEX_LIMIT }),
      getBroadcasts(INDEX_LIMIT),
      getCommands(),
    ])
      .then(([pending, judgements, watches, blacklist, broadcasts, commands]) => {
        if (!alive) {
          return
        }
        fetchedAt.current = Date.now()
        setData({
          pending: pending.pending,
          judgements: judgements.judgements,
          watches: watches.watches,
          blacklist: blacklist.entries,
          broadcasts: broadcasts.broadcasts,
          commands: commands.commands,
          prefix: commands.prefix,
        })
        setError('')
      })
      .catch((cause: unknown) => {
        if (alive) {
          setError(toApiError(cause).message)
        }
      })
      .finally(() => {
        if (alive) {
          setLoading(false)
        }
      })
    return () => {
      alive = false
    }
  }, [open])

  const hits = useMemo(() => {
    if (data === null) {
      return []
    }
    return buildHits({
      groups,
      events,
      pending: data.pending,
      judgements: data.judgements,
      watches: data.watches,
      blacklist: data.blacklist,
      broadcasts: data.broadcasts,
      commands: data.commands,
      commandPrefix: data.prefix,
      groupLabel,
    })
  }, [data, groups, events, groupLabel])

  const counts = useMemo(() => {
    if (data === null) {
      return []
    }
    return [
      { label: '判定', value: data.judgements.length },
      { label: '待验证', value: data.pending.length },
      { label: '黑名单', value: data.blacklist.length },
      { label: '高风险', value: data.watches.length },
      { label: '广播', value: data.broadcasts.length },
      { label: '命令', value: data.commands.length },
    ]
  }, [data])

  return { hits, loading, error, counts }
}
