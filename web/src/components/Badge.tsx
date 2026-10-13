import type { ReactNode } from 'react'
import type { Tone } from '../lib/format'

interface BadgeProps {
  tone?: Tone
  children: ReactNode
  title?: string
}

const TONE_CLASS: Record<Tone, string> = {
  neutral: 'badge-tint-neutral',
  coral: 'badge-tint-coral',
  warn: 'badge-tint-warn',
  danger: 'badge-tint-danger',
  success: 'badge-tint-success',
  primary: 'badge-primary',
}

export function Badge({ tone = 'neutral', children, title }: BadgeProps) {
  return (
    <span className={`badge ${TONE_CLASS[tone]}`} title={title}>
      {children}
    </span>
  )
}
