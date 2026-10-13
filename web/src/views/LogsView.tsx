import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { readEventStream, toApiError } from '../api/client'
import type { ApiError } from '../api/client'
import { LOG_STREAM_PATH, getConfig, getLogs } from '../api/endpoints'
import type { LogField, LogRecord } from '../api/types'
import { useAsync } from '../hooks/useAsync'
import { useTicker } from '../hooks/useTicker'
import { downloadCsv } from '../lib/csv'
import { formatAgo, formatClock, formatDateTime, matches, unixNow } from '../lib/format'
import { EmptyState } from '../components/EmptyState'
import { Notice } from '../components/Notice'
import { SegmentedControl } from '../components/SegmentedControl'
import { Spinner } from '../components/Spinner'
import { StatCard } from '../components/StatCard'

interface LogsProps {
  search: string
}

type LevelFilter = 'debug' | 'info' | 'warn' | 'error'
type Connection = 'connecting' | 'live' | 'retrying' | 'error' | 'paused'

const LEVEL_OPTIONS: readonly { value: LevelFilter; label: string }[] = [
  { value: 'debug', label: '全部' },
  { value: 'info', label: 'INFO' },
  { value: 'warn', label: 'WARN' },
  { value: 'error', label: 'ERROR' },
]

/** 浏览器侧最多留多少条。缓冲区是服务端的事，这个上限是给页面自己的。 */
const MAX_KEPT = 1000
const SNAPSHOT_LIMIT = 200
/** 一批一渲染的窗口：机器人打日志可以很突然，一条一次渲染会让页面在最需要它的时候变慢。 */
const FLUSH_MS = 200
const RETRY_MS = 3000

/** 一条 SSE 的 data 字段 → 一条日志；形状不对就当没收到，不猜 */
function parseRecord(data: string): LogRecord | null {
  let raw: unknown
  try {
    raw = JSON.parse(data) as unknown
  } catch {
    return null
  }
  if (typeof raw !== 'object' || raw === null) {
    return null
  }
  const candidate = raw as Partial<LogRecord>
  if (typeof candidate.seq !== 'number' || typeof candidate.message !== 'string') {
    return null
  }
  const attrs: LogField[] = []
  if (Array.isArray(candidate.attrs)) {
    for (const field of candidate.attrs) {
      const one = field as LogField
      if (typeof one.key === 'string' && typeof one.value === 'string') {
        attrs.push({ key: one.key, value: one.value })
      }
    }
  }
  return {
    seq: candidate.seq,
    time: typeof candidate.time === 'number' ? candidate.time : 0,
    level: typeof candidate.level === 'string' ? candidate.level : 'INFO',
    message: candidate.message,
    attrs,
  }
}

/** 可中止的等待：暂停或换过滤条件时不该卡在这 3 秒里 */
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = window.setTimeout(resolve, ms)
    signal.addEventListener(
      'abort',
      () => {
        window.clearTimeout(timer)
        resolve()
      },
      { once: true },
    )
  })
}

/** 一条日志的纯文本形式（复制用） */
function lineOf(record: LogRecord): string {
  const attrs = record.attrs.map((field) => `${field.key}=${field.value}`).join(' ')
  return `${formatDateTime(record.time)} ${record.level} ${record.message}${attrs === '' ? '' : ` ${attrs}`}`
}

const CONNECTION_TEXT: Record<Connection, string> = {
  connecting: '正在连接…',
  live: '实时接收中',
  retrying: `连接断开，${String(RETRY_MS / 1000)} 秒后重连（按 seq 补回缺口）`,
  error: '连接失败',
  paused: '已暂停',
}

export function LogsView({ search }: LogsProps) {
  const now = useTicker(15_000)
  const [level, setLevel] = useState<LevelFilter>('info')
  const [paused, setPaused] = useState(false)
  const [autoScroll, setAutoScroll] = useState(true)
  const [connection, setConnection] = useState<Connection>('connecting')
  const [streamError, setStreamError] = useState<ApiError | null>(null)
  const [appliedLevel, setAppliedLevel] = useState('')
  const [buffer, setBuffer] = useState({ capacity: 0, kept: 0, dropped: 0 })
  const [records, setRecords] = useState<LogRecord[]>([])
  const [copied, setCopied] = useState(false)

  const config = useAsync(() => getConfig(), [])

  const bodyRef = useRef<HTMLDivElement | null>(null)
  const lastSeq = useRef(0)
  const queued = useRef<LogRecord[]>([])
  const flushTimer = useRef<number | null>(null)

  const appendBatch = useCallback((batch: readonly LogRecord[]) => {
    if (batch.length === 0) {
      return
    }
    setRecords((prev) => {
      const bySeq = new Map<number, LogRecord>()
      for (const record of prev) {
        bySeq.set(record.seq, record)
      }
      for (const record of batch) {
        // 快照与流会重叠：同一条 seq 后到的赢，先到的不会重复出现
        bySeq.set(record.seq, record)
      }
      const merged = [...bySeq.values()].sort((a, b) => a.seq - b.seq)
      return merged.length <= MAX_KEPT ? merged : merged.slice(merged.length - MAX_KEPT)
    })
  }, [])

  const pushRecord = useCallback(
    (record: LogRecord) => {
      lastSeq.current = Math.max(lastSeq.current, record.seq)
      queued.current.push(record)
      if (flushTimer.current === null) {
        flushTimer.current = window.setTimeout(() => {
          flushTimer.current = null
          const batch = queued.current
          queued.current = []
          appendBatch(batch)
        }, FLUSH_MS)
      }
    },
    [appendBatch],
  )

  // 卸载时把没来得及刷出去的那一批丢掉，也不留下定时器
  useEffect(() => {
    return () => {
      if (flushTimer.current !== null) {
        window.clearTimeout(flushTimer.current)
        flushTimer.current = null
      }
      queued.current = []
    }
  }, [])

  useEffect(() => {
    if (paused) {
      setConnection('paused')
      return
    }

    const controller = new AbortController()
    let alive = true

    async function run(): Promise<void> {
      setConnection('connecting')
      try {
        // 先要一份快照。它同时给出缓冲区的规模与被挤掉的条数 —— 这两件事只有这一个
        // 接口会说 —— 随后的流用 after=next_seq 接上，既不会重复也不会漏。
        const snapshot = await getLogs({ limit: SNAPSHOT_LIMIT, level })
        if (!alive) {
          return
        }
        setAppliedLevel(snapshot.level)
        setBuffer({ capacity: snapshot.capacity, kept: snapshot.kept, dropped: snapshot.dropped })
        lastSeq.current = Math.max(lastSeq.current, snapshot.next_seq)
        for (const record of snapshot.records) {
          pushRecord(record)
        }
      } catch (cause) {
        if (!alive) {
          return
        }
        setStreamError(toApiError(cause))
        setConnection('error')
        return
      }

      for (;;) {
        if (!alive) {
          return
        }
        try {
          await readEventStream(
            LOG_STREAM_PATH,
            { query: { level, after: lastSeq.current }, signal: controller.signal },
            (message) => {
              if (message.event === 'open') {
                setConnection('live')
                setStreamError(null)
                return
              }
              const record = parseRecord(message.data)
              if (record !== null) {
                pushRecord(record)
              }
            },
          )
          if (!alive) {
            return
          }
          // 流自己结束了（机器人重启、代理断开）：等一会儿，用 after 把缺口补回来
          setConnection('retrying')
        } catch (cause) {
          if (!alive) {
            return
          }
          setStreamError(toApiError(cause))
          setConnection('retrying')
        }
        await sleep(RETRY_MS, controller.signal)
      }
    }

    void run()
    return () => {
      alive = false
      controller.abort()
    }
  }, [level, paused, pushRecord])

  const rows = useMemo(
    () => records.filter((record) => matches([record.message, record.level], search)),
    [records, search],
  )

  useEffect(() => {
    if (!autoScroll) {
      return
    }
    const element = bodyRef.current
    if (element !== null) {
      element.scrollTop = element.scrollHeight
    }
  }, [rows, autoScroll])

  const counts = useMemo(() => {
    let warn = 0
    let err = 0
    for (const record of records) {
      if (record.level === 'ERROR') {
        err += 1
      } else if (record.level === 'WARN') {
        warn += 1
      }
    }
    return { warn, err }
  }, [records])

  const oldest = rows.length === 0 ? null : rows[0]
  const newest = rows.length === 0 ? null : rows[rows.length - 1]
  const botLevel = config.data?.log.level ?? ''

  function handleCopy(): void {
    setCopied(true)
    window.setTimeout(() => {
      setCopied(false)
    }, 1200)
    try {
      void navigator.clipboard?.writeText(rows.map(lineOf).join('\n'))
    } catch {
      // 忽略：环境不支持剪贴板
    }
  }

  function handleDownload(): void {
    downloadCsv(
      `bot-log-${String(unixNow())}.csv`,
      ['seq', '时间', '等级', '消息', '字段'],
      rows.map((record) => [
        String(record.seq),
        formatDateTime(record.time),
        record.level,
        record.message,
        record.attrs.map((field) => `${field.key}=${field.value}`).join(' '),
      ]),
    )
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">日志</h1>
          <div className="page-sub">
            机器人进程内的日志缓冲区{buffer.capacity === 0 ? '' : `（最近 ${String(buffer.capacity)} 条）`} + 实时流
          </div>
        </div>
        <div className="page-actions">
          <button type="button" className="btn btn-outline" onClick={handleCopy} disabled={rows.length === 0}>
            {copied ? '已复制' : '复制全部'}
          </button>
          <button type="button" className="btn btn-outline" onClick={handleDownload} disabled={rows.length === 0}>
            导出 CSV
          </button>
          <button
            type="button"
            className={`btn ${paused ? 'btn-secondary' : 'btn-outline'}`}
            onClick={() => {
              setPaused(!paused)
            }}
          >
            {paused ? '继续接收' : '暂停'}
          </button>
          <button
            type="button"
            className="btn btn-outline"
            onClick={() => {
              setRecords([])
              queued.current = []
              lastSeq.current = 0
            }}
            disabled={records.length === 0}
          >
            清空
          </button>
        </div>
      </div>

      {streamError ? (
        <Notice tone={connection === 'error' ? 'error' : 'warning'} title={`日志流异常（${streamError.code}）`}>
          {streamError.message}
        </Notice>
      ) : null}

      <div className="metrics">
        <StatCard
          label="正在显示"
          value={rows.length}
          dot={connection === 'live' ? 'on' : connection === 'paused' ? 'warn' : 'off'}
          decorated
          note={CONNECTION_TEXT[connection]}
        />
        <StatCard label="WARN" value={counts.warn} dot="warn" note="缓冲区里还在的部分" />
        <StatCard label="ERROR" value={counts.err} dot="danger" note="缓冲区里还在的部分" />
        <StatCard
          label="缓冲区"
          value={`${String(buffer.kept)} / ${buffer.capacity === 0 ? '—' : String(buffer.capacity)}`}
          dot={buffer.dropped > 0 ? 'warn' : 'off'}
          note={`已被挤掉 ${String(buffer.dropped)} 条`}
        />
      </div>

      <Notice tone="info" title="这是内存里的副本，不是 journal">
        进程把每条日志同时写到 <code>stderr</code>（systemd 收进 journal）和一块固定大小的内存缓冲区，这一页读的是后者。
        所以：缓冲区满了会丢最旧的（「已被挤掉」就是它）；这里的等级过滤只是<b>不再显示更低级别</b>，
        真正的下限是配置里的 <code>log.level</code>
        {botLevel === '' ? '' : `（当前 ${botLevel}）`}—— 比它低的日志从来没进过缓冲区，选「全部」也看不到。
        后端实际应用的过滤：<b>{appliedLevel === '' ? '—' : appliedLevel}</b>。
        <br />
        暂停不是停止记录：继续时会带上最后一条的 <code>seq</code> 重连，缓冲区里还在的部分会被补回来。
      </Notice>

      <div className="filterbar">
        <span className="filter">
          <span className="fk">等级</span>
          <SegmentedControl<LevelFilter> value={level} options={LEVEL_OPTIONS} onChange={setLevel} label="等级" />
        </span>
        <label className="filter">
          <span className="fk">自动滚动</span>
          <input
            type="checkbox"
            checked={autoScroll}
            onChange={(event) => {
              setAutoScroll(event.target.checked)
            }}
          />
        </label>
        <span className="spacer" />
        <span className="section-note">
          <span className={`dot ${connection === 'live' ? 'is-live' : 'is-off'}`} />
          {CONNECTION_TEXT[connection]} · 页面最多保留 {MAX_KEPT} 条
        </span>
      </div>

      <div className="logbox" ref={bodyRef}>
        {rows.length === 0 ? (
          <div className="log-empty">
            {connection === 'connecting'
              ? '正在读取日志…'
              : '这个过滤下没有日志。机器人很安静，或者等级选得太细了。'}
          </div>
        ) : (
          rows.map((record) => (
            <div className="log-line" key={record.seq}>
              <span className="log-time" title={formatDateTime(record.time)}>
                {formatClock(record.time, true)}
              </span>
              <span className={`log-level is-${record.level.toLowerCase()}`}>{record.level}</span>
              <span className="log-msg">
                {record.message}
                {record.attrs.length === 0 ? null : (
                  <span className="log-attrs">
                    {' '}
                    {record.attrs.map((field, index) => (
                      <span key={`${field.key}-${String(index)}`}>
                        <span className="log-attr-key">{field.key}</span>
                        <span>=</span>
                        <span>{field.value}</span>{' '}
                      </span>
                    ))}
                  </span>
                )}
              </span>
            </div>
          ))
        )}
      </div>

      <div className="log-foot">
        <span>
          缓冲区 {buffer.kept} 条 · 已被挤掉 {buffer.dropped} 条
        </span>
        <span className="spacer" />
        {oldest === null || newest === null ? (
          <span>没有可显示的记录</span>
        ) : (
          <span>
            最早一条 {formatAgo(oldest.time, now)} · 最新一条 {formatAgo(newest.time, now)}
          </span>
        )}
      </div>

      <div className="section-title">
        <h2>这一页读得到什么</h2>
        <span className="section-note">几个数字各是什么</span>
      </div>
      {config.data === null && config.loading ? (
        <div className="load-block">
          <Spinner label="正在读取配置…" />
        </div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>项</th>
                <th>值</th>
                <th>含义</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td>log.level</td>
                <td className="mono">{config.data === null ? '—' : config.data.log.level}</td>
                <td>低于这个级别的日志从来不进缓冲区，所以这一页看不到它们</td>
              </tr>
              <tr>
                <td>log.format</td>
                <td className="mono">{config.data === null ? '—' : config.data.log.format}</td>
                <td>stderr 上的格式；这一页按结构化字段展示，与它无关</td>
              </tr>
              <tr>
                <td>缓冲区容量</td>
                <td className="mono">{buffer.capacity === 0 ? '—' : String(buffer.capacity)}</td>
                <td>进程启动时固定，改大小需要重启机器人</td>
              </tr>
              <tr>
                <td>累计被挤掉</td>
                <td className="mono">{buffer.dropped}</td>
                <td>进程启动以来因缓冲区满而丢掉的条数；journal 里仍然有</td>
              </tr>
              <tr>
                <td>页面保留上限</td>
                <td className="mono">{MAX_KEPT}</td>
                <td>浏览器侧的封顶，避免长时间开着把页面自己拖慢</td>
              </tr>
            </tbody>
          </table>
        </div>
      )}

      {rows.length === 0 && connection !== 'connecting' ? (
        <EmptyState
          crumb="日志"
          message="没有日志可显示"
          note="要么这段时间机器人没说话，要么等级过滤把它们挡住了。日志同时写在 journal 里：journalctl --user -u qq-group-management-bot -n 200"
        />
      ) : null}
    </>
  )
}
