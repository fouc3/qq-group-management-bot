interface SpinnerProps {
  label?: string
  /** 整块区域居中时配 .load-block 使用 */
  large?: boolean
}

export function Spinner({ label = '加载中…', large = false }: SpinnerProps) {
  return (
    <span className={`spinner${large ? ' is-lg' : ''}`} role="status" aria-live="polite">
      <span className="ring" />
      <span>{label}</span>
    </span>
  )
}
