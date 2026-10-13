import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import './styles/tokens.css'
import './styles/global.css'

/**
 * 挂载入口。
 * 产物被改成经典脚本（见 vite.config.ts 的 classicScriptPlugin），标签上带 defer；
 * 这里再兜一层 DOM-ready 判断，保证无论脚本被放在 <head>、还是被换成同步脚本，
 * #root 都已经在 DOM 里。
 */
function mount(): void {
  const container = document.getElementById('root')
  if (container === null) {
    throw new Error('缺少 #root 挂载点：index.html 被改动过')
  }
  // 清掉 index.html 里的加载兜底文案（无 JS 时它才是唯一可见内容）
  container.textContent = ''
  createRoot(container).render(
    <StrictMode>
      <App />
    </StrictMode>,
  )
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', mount, { once: true })
} else {
  mount()
}
