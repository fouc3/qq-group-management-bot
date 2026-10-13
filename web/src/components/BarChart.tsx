import type { ReactNode } from 'react'
import { Card } from './Card'
import { EmptyState } from './EmptyState'

export type BarTone = 'success' | 'primary' | 'warning' | 'danger' | 'muted'

export interface BarValue {
  tone: BarTone
  value: number
}

export interface BarColumn {
  label: string
  values: readonly BarValue[]
  /** 悬停提示，通常写「类别 · 数值」 */
  title?: string
}

export interface BarLegendItem {
  tone: BarTone
  label: string
  value?: number
}

interface BarChartProps {
  title: ReactNode
  desc?: ReactNode
  legend?: readonly BarLegendItem[]
  columns: readonly BarColumn[]
  /** true = 每根柱子内部堆叠（按列合计归一）；false = 所有分段共用一个刻度（按最大值归一） */
  stacked?: boolean
  emptyText?: string
}

function percent(value: number, denominator: number): number {
  if (denominator <= 0 || value <= 0) {
    return 0
  }
  return (value / denominator) * 100
}

/**
 * 纯 CSS 柱状图（无图表库）：flex column-reverse 堆叠 + 百分比高度。
 * 接口没有给逐小时序列时不要伪造时间轴，用真实存在的分类维度做柱子。
 */
export function BarChart({ title, desc, legend, columns, stacked = false, emptyText }: BarChartProps) {
  const columnTotals = columns.map((column) => column.values.reduce((sum, item) => sum + item.value, 0))
  const maxSegment = columns.reduce(
    (max, column) => column.values.reduce((inner, item) => Math.max(inner, item.value), max),
    0,
  )
  const denominator = stacked ? columnTotals.reduce((max, total) => Math.max(max, total), 0) : maxSegment

  return (
    <Card
      title={title}
      desc={desc}
      actions={
        legend !== undefined && legend.length > 0 ? (
          <div className="legend">
            {legend.map((item) => (
              <span key={`${item.tone}-${item.label}`}>
                <i className={`tone-${item.tone}`} />
                {item.label}
                {item.value === undefined ? null : ` ${item.value}`}
              </span>
            ))}
          </div>
        ) : undefined
      }
    >
      {columns.length === 0 ? (
        <EmptyState
          inline
          message={emptyText ?? '暂无数据'}
          note="接口返回空集合时不画占位柱"
        />
      ) : (
        <div className="chart-bars">
          {columns.map((column) => (
            <div className="col" key={column.label} title={column.title ?? column.label}>
              <div className="col-stack">
                {column.values.map((item, index) => {
                  const height = percent(item.value, denominator)
                  return (
                    <span
                      key={`${column.label}-${item.tone}-${index}`}
                      className={`col-seg tone-${item.tone}`}
                      style={{ height: `${height}%`, minHeight: item.value > 0 ? '3px' : undefined }}
                    />
                  )
                })}
              </div>
              <span className="col-x">{column.label}</span>
            </div>
          ))}
        </div>
      )}
    </Card>
  )
}
