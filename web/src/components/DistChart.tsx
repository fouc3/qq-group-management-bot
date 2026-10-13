import type { ReactNode } from 'react'
import type { BarTone } from './BarChart'
import { Card } from './Card'
import { EmptyState } from './EmptyState'

export interface DistRow {
  label: string
  value: number
  tone: BarTone
  title?: string
}

interface DistChartProps {
  title: ReactNode
  desc?: ReactNode
  rows: readonly DistRow[]
  emptyText?: string
}

/** 横向分布条（纯 CSS）：适用于「按类别/按群」的计数分布 */
export function DistChart({ title, desc, rows, emptyText }: DistChartProps) {
  const max = rows.reduce((acc, row) => Math.max(acc, row.value), 0)
  return (
    <Card title={title} desc={desc}>
      {rows.length === 0 ? (
        <EmptyState inline message={emptyText ?? '暂无数据'} note="接口返回空集合时不画占位条" />
      ) : (
        <div className="dist">
          {rows.map((row) => (
            <div className="dist-row" key={row.label} title={row.title ?? `${row.label} · ${row.value}`}>
              <span className="dist-name">{row.label}</span>
              <span className="dist-track">
                <span
                  className={`dist-fill tone-${row.tone}`}
                  style={{ width: `${max <= 0 ? 0 : (row.value / max) * 100}%` }}
                />
              </span>
              <span className="dist-n">{row.value}</span>
            </div>
          ))}
        </div>
      )}
    </Card>
  )
}
