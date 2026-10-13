interface SwitchProps {
  checked: boolean
  onChange?: (next: boolean) => void
  disabled?: boolean
  label?: string
  title?: string
}

/**
 * 真开关（button[role=switch]）。
 * 写接口未开放时传 disabled —— 只展示真实状态，不假装可点。
 */
export function Switch({ checked, onChange, disabled = false, label, title }: SwitchProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      className={`switch${checked ? ' is-on' : ''}`}
      disabled={disabled}
      title={title}
      onClick={() => {
        onChange?.(!checked)
      }}
    >
      <span className="switch-track">
        <span className="switch-knob" />
      </span>
      {label !== undefined ? <span className={`switch-label${checked ? '' : ' is-off'}`}>{label}</span> : null}
    </button>
  )
}
