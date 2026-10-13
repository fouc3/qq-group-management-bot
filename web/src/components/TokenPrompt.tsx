import { useState } from 'react'
import type { FormEvent } from 'react'
import { Notice } from './Notice'

interface TokenPromptProps {
  onSubmit: (token: string) => void
  /** 401 之后回到登录页时给出原因 */
  notice?: string
}

/**
 * 登录页：只有输入框和「保存」。
 * token 存本地 localStorage（qgb.admin.token），只用于向 /api/v1 发 Bearer 凭证。
 */
export function TokenPrompt({ onSubmit, notice }: TokenPromptProps) {
  const [value, setValue] = useState('')
  const trimmed = value.trim()

  function handleSubmit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    if (trimmed === '') {
      return
    }
    onSubmit(trimmed)
  }

  return (
    <div className="token-page">
      <div className="token-card">
        <div className="token-head">
          <div className="brand-mark">
            <svg
              width="18"
              height="18"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <rect x="4" y="8" width="16" height="12" rx="3" />
              <path d="M12 4v4" />
              <path d="M9 14h.01" />
              <path d="M15 14h.01" />
            </svg>
          </div>
          <div>
            <h1 className="token-title">群管控制台</h1>
            <div className="token-sub">QQ Group Management Bot · 运维控制台</div>
          </div>
        </div>

        {notice !== undefined && notice !== '' ? (
          <Notice tone="warning" title="需要重新登录">
            {notice}
          </Notice>
        ) : null}

        <form className="token-form" onSubmit={handleSubmit}>
          <div className="field">
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
              <rect x="4" y="11" width="16" height="9" rx="2" />
              <path d="M8 11V8a4 4 0 0 1 8 0v3" />
            </svg>
            <input
              type="password"
              value={value}
              placeholder="粘贴 config.yaml 里的 admin.token"
              aria-label="后台 token"
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => {
                setValue(event.target.value)
              }}
            />
          </div>
          <button type="submit" className="btn btn-primary" disabled={trimmed === ''}>
            保存
          </button>
        </form>

        <div className="token-hint">
          token 只保存在本机 <code>localStorage</code>（key <code>qgb.admin.token</code>），用于
          <code>Authorization: Bearer</code>；收到 401 会立即清除并要求重新输入。
        </div>
      </div>
    </div>
  )
}
