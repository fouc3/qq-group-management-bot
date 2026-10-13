import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { FocusEvent as ReactFocusEvent, KeyboardEvent as ReactKeyboardEvent } from 'react'
import type { Group, MemberEvent } from '../api/types'
import { INDEX_LIMIT, useSearchIndex } from '../hooks/useSearchIndex'
import { preview } from '../lib/format'
import { filterHit, matchHits, navHits } from '../lib/search'
import type { SearchHit } from '../lib/search'
import type { ViewKey } from './Sidebar'

/** 下拉框里最多画几条；超出的在底部说一声「还有 N 条」 */
const MAX_ROWS = 12

interface SearchPaletteProps {
  groups: readonly Group[]
  events: readonly MemberEvent[]
  groupLabel: (openid: string) => string
  currentView: ViewKey
  /** 当前页生效的文本筛选（由搜索里的「筛选」项设置，不在输入框里） */
  filter: string
  onFilter: (value: string) => void
  onPick: (hit: SearchHit) => void
}

/**
 * 顶栏搜索：输入之后给的是「待选择」的候选，不是把当前页筛成一半。
 *
 * 每条候选自带动作 —— 群是切换当前群、记录是跳过去定位、命令是打开配置页并筛选、
 * 页面是直接过去；候选的最后还总是给一条「在当前页筛选」，把原来的文本过滤能力
 * 留在明面上（它不再自动发生）。
 */
export function SearchPalette({
  groups,
  events,
  groupLabel,
  currentView,
  filter,
  onFilter,
  onPick,
}: SearchPaletteProps) {
  const [query, setQuery] = useState('')
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(0)
  const rootRef = useRef<HTMLDivElement | null>(null)
  const inputRef = useRef<HTMLInputElement | null>(null)

  const index = useSearchIndex(open, { groups, events, groupLabel })
  const trimmed = query.trim()

  const matched = useMemo(
    () => (trimmed === '' ? { hits: navHits(), more: 0, folded: 0 } : matchHits(index.hits, trimmed, MAX_ROWS)),
    [trimmed, index.hits],
  )

  // 文本筛选项：当前页 + 日志页（不在日志页时才有意义）
  const extras = useMemo(() => {
    if (trimmed === '') {
      return []
    }
    const list = [filterHit(currentView, trimmed)]
    if (currentView !== 'logs') {
      list.push(filterHit('logs', trimmed, '在「日志」里筛选'))
    }
    return list
  }, [trimmed, currentView])

  const rows = useMemo(() => [...matched.hits, ...extras], [matched.hits, extras])

  useEffect(() => {
    setActive(0)
  }, [trimmed])

  // ⌘K / Ctrl+K：把光标送进搜索框（控制台里翻页用键盘比用鼠标快）
  useEffect(() => {
    function onKey(event: KeyboardEvent): void {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault()
        inputRef.current?.focus()
        inputRef.current?.select()
        setOpen(true)
      }
    }
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
    }
  }, [])

  // 点外面关掉下拉框
  useEffect(() => {
    function onDown(event: PointerEvent): void {
      const root = rootRef.current
      if (root === null || !(event.target instanceof Node)) {
        return
      }
      if (!root.contains(event.target)) {
        setOpen(false)
      }
    }
    document.addEventListener('pointerdown', onDown)
    return () => {
      document.removeEventListener('pointerdown', onDown)
    }
  }, [])

  const pick = useCallback(
    (hit: SearchHit): void => {
      onPick(hit)
      setQuery('')
      setActive(0)
      setOpen(false)
    },
    [onPick],
  )

  function onKeyDown(event: ReactKeyboardEvent<HTMLInputElement>): void {
    if (event.key === 'Escape') {
      setOpen(false)
      return
    }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault()
      if (!open) {
        setOpen(true)
        return
      }
      const count = rows.length
      if (count === 0) {
        return
      }
      setActive((current) => {
        if (event.key === 'ArrowDown') {
          return (current + 1) % count
        }
        return (current - 1 + count) % count
      })
      return
    }
    if (event.key === 'Enter') {
      const hit = rows[active]
      if (hit === undefined) {
        return
      }
      event.preventDefault()
      pick(hit)
    }
  }

  function onBlur(event: ReactFocusEvent<HTMLInputElement>): void {
    const root = rootRef.current
    if (root === null || !(event.relatedTarget instanceof Node) || !root.contains(event.relatedTarget)) {
      setOpen(false)
    }
  }

  const countsText = index.counts.map((item) => `${item.label} ${String(item.value)}`).join(' · ')

  return (
    <div className="searchbox" ref={rootRef}>
      {filter === '' ? null : (
        <span className="filter-chip" title="当前页的文本筛选，来自搜索里的「筛选」项">
          <span className="fc-k">筛选</span>
          <span className="fc-v">{preview(filter, 16)}</span>
          <button
            type="button"
            className="fc-x"
            aria-label="清除本页筛选"
            onClick={() => {
              onFilter('')
            }}
          >
            ✕
          </button>
        </span>
      )}

      <div className="searchbox-field">
        <svg
          width="15"
          height="15"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <circle cx="11" cy="11" r="7" />
          <path d="m20 20-3.5-3.5" />
        </svg>
        <input
          ref={inputRef}
          type="text"
          value={query}
          placeholder="搜索群 / openid / 判定 id / 命令…"
          aria-label="搜索"
          title="⌘K / Ctrl+K 聚焦 · ↑↓ 选择 · Enter 跳转 · Esc 关闭"
          onChange={(event) => {
            setQuery(event.target.value)
            setOpen(true)
          }}
          onFocus={() => {
            setOpen(true)
          }}
          onKeyDown={onKeyDown}
          onBlur={onBlur}
        />
        {open ? (
          <div className="palette">
            {trimmed === '' ? <div className="palette-note">输入群号 / openid / 判定 id，或直接选一页</div> : null}
            {index.loading && index.hits.length === 0 ? <div className="palette-note">正在读取搜索索引…</div> : null}
            {index.error === '' ? null : <div className="palette-note is-error">索引没取到：{index.error}</div>}
            {rows.length === 0 && !index.loading ? (
              <div className="palette-note">没有匹配的候选（索引范围见底部）</div>
            ) : null}

            {rows.map((hit, position) => (
              <button
                type="button"
                key={hit.id}
                className={`prow${position === active ? ' is-active' : ''}`}
                title={hit.hint}
                onMouseEnter={() => {
                  setActive(position)
                }}
                onClick={() => {
                  pick(hit)
                }}
              >
                <span className={`pk pk-${hit.action.type}`}>{hit.kind}</span>
                <span className="pcol">
                  <span className="pt">{hit.title}</span>
                  <span className="ps">{hit.subtitle}</span>
                </span>
                <span className="pm">{hit.meta}</span>
              </button>
            ))}

            {trimmed === '' ? null : (
              <div className="palette-foot">
                {matched.more > 0 ? (
                  <span>还有 {matched.more} 条没显示，再输几个字缩小范围</span>
                ) : null}
                {matched.folded > 0 ? (
                  <span>
                    {matched.folded} 条只是属于命中的那个群，已经并进「切换到群」那一条
                  </span>
                ) : null}
                {countsText === '' ? null : (
                  <span className="muted">
                    索引：{countsText}（每张表最多读最新 {INDEX_LIMIT} 条）
                  </span>
                )}
              </div>
            )}
          </div>
        ) : null}
      </div>
    </div>
  )
}
