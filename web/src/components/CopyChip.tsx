import { useState } from 'react'

interface CopyChipProps {
  value: string
  /** 显示文本，默认显示完整等宽值（超长自动省略） */
  display?: string
  label?: string
}

/** openid 芯片：点击复制（file:// 等无剪贴板权限的环境静默忽略） */
export function CopyChip({ value, display, label = '复制 openid' }: CopyChipProps) {
  const [copied, setCopied] = useState(false)

  function handleCopy(): void {
    setCopied(true)
    window.setTimeout(() => {
      setCopied(false)
    }, 900)
    try {
      void navigator.clipboard?.writeText(value)
    } catch {
      // 忽略：环境不支持剪贴板
    }
  }

  return (
    <button
      type="button"
      className={`openid-chip${copied ? ' is-copied' : ''}`}
      title={`${label}：${value}`}
      onClick={handleCopy}
    >
      <span className="val">{display ?? value}</span>
      {copied ? (
        <span className="copy mono-sm nowrap">已复制</span>
      ) : (
        <span className="copy" aria-hidden="true">
          <svg
            width="14"
            height="14"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <rect x="9" y="9" width="12" height="12" rx="2" />
            <path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1" />
          </svg>
        </span>
      )}
    </button>
  )
}
