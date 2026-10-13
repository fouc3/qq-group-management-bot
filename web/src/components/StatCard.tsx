import type { ReactNode } from 'react'

interface StatCardProps {
  label: ReactNode
  value: ReactNode
  note?: ReactNode
  dot?: 'coral' | 'warn' | 'on' | 'off' | 'danger'
  decorated?: boolean
}

export function StatCard({ label, value, note, dot = 'off', decorated = false }: StatCardProps) {
  return (
    <div className={`card metric${decorated ? ' decorated' : ''}`}>
      <div className="metric-top">
        <span className={`dot is-${dot}`} />
        <span className="metric-k">{label}</span>
      </div>
      <div className="metric-v">{value}</div>
      <div className="metric-note">{note ?? ''}</div>
    </div>
  )
}
