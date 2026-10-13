/**
 * 三态/多态分段控件（真数据里用于「本群覆盖：继承全局 / 开 / 关」的展示，
 * 以及违规判定的本地过滤器）。纯受控组件，不自己维护状态。
 */
export interface SegmentedOption<T extends string> {
  value: T
  label: string
}

interface SegmentedControlProps<T extends string> {
  value: T
  options: readonly SegmentedOption<T>[]
  onChange?: (next: T) => void
  disabled?: boolean
  label?: string
  title?: string
}

export function SegmentedControl<T extends string>({
  value,
  options,
  onChange,
  disabled = false,
  label,
  title,
}: SegmentedControlProps<T>) {
  return (
    <div className="seg" role="group" aria-label={label} title={title}>
      {options.map((option) => {
        const on = option.value === value
        return (
          <button
            key={option.value}
            type="button"
            data-val={option.value}
            className={`seg-btn${on ? ' is-on' : ''}`}
            aria-pressed={on}
            disabled={disabled}
            onClick={() => {
              onChange?.(option.value)
            }}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}
