/* =========================================================================
   fetch 封装
   - BASE 取自 VITE_API_BASE，默认同源（''），拼 `${BASE}/api/v1/...`
   - token 存 localStorage（key: qgb.admin.token），有 token 就带 Bearer
   - 任何请求收到 401 → 清 token 并通知 UI 回登录页
   - 错误体统一 { error: { code, message, detail } }
   ========================================================================= */

declare global {
  interface ImportMetaEnv {
    readonly VITE_API_BASE?: string
  }
  interface ImportMeta {
    readonly env: ImportMetaEnv
  }
}

/** API 基础地址；交给 Go 侧 embed 时留空即同源 */
const BASE: string = import.meta.env.VITE_API_BASE ?? ''

export const TOKEN_STORAGE_KEY = 'qgb.admin.token'

const REQUEST_TIMEOUT_MS = 15000

/** 统一错误类型：UI 只需要 status / code / message */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly detail: unknown

  constructor(status: number, code: string, message: string, detail?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.detail = detail
  }
}

/** 任意 thrown 值 → ApiError */
export function toApiError(cause: unknown): ApiError {
  if (cause instanceof ApiError) {
    return cause
  }
  if (cause instanceof Error && cause.message !== '') {
    return new ApiError(0, 'unknown_error', cause.message)
  }
  return new ApiError(0, 'unknown_error', '未知错误')
}

/* ------------------------------------------------------------------ token */

function readStoredToken(): string {
  try {
    return window.localStorage.getItem(TOKEN_STORAGE_KEY) ?? ''
  } catch {
    // file:// 下部分浏览器禁用 localStorage，退化为内存态
    return ''
  }
}

let currentToken: string = readStoredToken()
const tokenListeners = new Set<() => void>()

function notifyTokenChange(): void {
  tokenListeners.forEach((listener) => {
    listener()
  })
}

export function getToken(): string {
  return currentToken
}

export function hasToken(): boolean {
  return currentToken !== ''
}

export function saveToken(next: string): void {
  currentToken = next.trim()
  try {
    if (currentToken === '') {
      window.localStorage.removeItem(TOKEN_STORAGE_KEY)
    } else {
      window.localStorage.setItem(TOKEN_STORAGE_KEY, currentToken)
    }
  } catch {
    // 忽略：内存里仍然有 token，只是刷新后需要重新输入
  }
  notifyTokenChange()
}

export function clearToken(): void {
  if (currentToken === '') {
    return
  }
  saveToken('')
}

/** 订阅 token 变化（401 清 token 后 App 需要切回登录页） */
export function subscribeToken(listener: () => void): () => void {
  tokenListeners.add(listener)
  return () => {
    tokenListeners.delete(listener)
  }
}

/* ---------------------------------------------------------------- request */

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'DELETE'
  query?: Record<string, string | number | boolean | null | undefined>
  body?: unknown
}

function buildUrl(path: string, query?: RequestOptions['query']): string {
  const base = BASE.endsWith('/') ? BASE.slice(0, -1) : BASE
  const search = new URLSearchParams()
  if (query) {
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === null || value === '') {
        continue
      }
      search.set(key, String(value))
    }
  }
  const qs = search.toString()
  return `${base}${path}${qs === '' ? '' : `?${qs}`}`
}

function errorEnvelope(payload: unknown): ApiError | null {
  if (typeof payload !== 'object' || payload === null) {
    return null
  }
  const raw = (payload as { error?: unknown }).error
  if (typeof raw !== 'object' || raw === null) {
    return null
  }
  const body = raw as { code?: unknown; message?: unknown; detail?: unknown }
  const code = typeof body.code === 'string' ? body.code : 'http_error'
  const message = typeof body.message === 'string' ? body.message : '请求失败'
  return new ApiError(0, code, message, body.detail)
}

/** 读取 JSON；空体 / 非 JSON 体都返回 null，不抛错 */
async function readJson(response: Response): Promise<unknown> {
  try {
    return (await response.json()) as unknown
  } catch {
    return null
  }
}

/** 带超时的 fetch：后端未启动时连接会立刻被拒，卡死只在服务假死时发生 */
async function fetchWithTimeout(url: string, init: RequestInit): Promise<Response> {
  const controller = new AbortController()
  const timer = window.setTimeout(() => {
    controller.abort()
  }, REQUEST_TIMEOUT_MS)
  try {
    return await fetch(url, { ...init, signal: controller.signal })
  } catch (cause) {
    if (cause instanceof Error && cause.name === 'AbortError') {
      throw new ApiError(0, 'timeout', `请求超时（${REQUEST_TIMEOUT_MS / 1000}s）：后端无响应`)
    }
    throw new ApiError(0, 'network_error', '无法连接到后端服务：请确认机器人进程已启动、admin 已开启且监听地址可达')
  } finally {
    window.clearTimeout(timer)
  }
}

/**
 * 唯一出口：`${BASE}/api/v1/...`
 * 返回体在边界处断言为 T —— 契约由后端保证，形状与 types.ts 一一对应。
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const url = buildUrl(path, options.query)
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (currentToken !== '') {
    headers.Authorization = `Bearer ${currentToken}`
  }
  let body: string | undefined
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(options.body)
  }

  const response = await fetchWithTimeout(url, {
    method: options.method ?? 'GET',
    headers,
    body,
  })

  if (response.status === 401) {
    clearToken()
    throw new ApiError(401, 'unauthorized', '登录凭证已失效（HTTP 401），请重新输入后台 token')
  }

  if (!response.ok) {
    const payload = await readJson(response)
    const envelope = errorEnvelope(payload)
    if (envelope) {
      throw new ApiError(response.status, envelope.code, envelope.message, envelope.detail)
    }
    throw new ApiError(response.status, 'http_error', `请求失败（HTTP ${response.status}）`)
  }

  const payload = await readJson(response)
  return payload as T
}

/* ------------------------------------------------------------ event stream */

/** 一条服务端事件（`event:` 省略时 event 为 "message"） */
export interface StreamMessage {
  event: string
  data: string
  id: string
}

/** 把一个事件块（空行之前的那些行）解析成一条消息；只有注释时返回 null */
function parseEvent(chunk: string): StreamMessage | null {
  const message: StreamMessage = { event: 'message', data: '', id: '' }
  let hasData = false
  for (const line of chunk.split('\n')) {
    if (line === '' || line.startsWith(':')) {
      continue
    }
    const colon = line.indexOf(':')
    const field = colon < 0 ? line : line.slice(0, colon)
    const value = colon < 0 ? '' : line.slice(colon + 1).replace(/^ /, '')
    if (field === 'event') {
      message.event = value
    } else if (field === 'data') {
      message.data += (hasData ? '\n' : '') + value
      hasData = true
    } else if (field === 'id') {
      message.id = value
    }
  }
  return hasData ? message : null
}

/**
 * 读一个 `text/event-stream`。
 *
 * 用 fetch + reader 而不是浏览器的 EventSource，因为 EventSource 不能带请求头：
 * token 就只剩查询串一条路，而查询串会进日志、进历史。这里复用与 request 相同的
 * 鉴权与错误形状，所以 401 的行为（清 token、回登录页）与其它请求一致。
 *
 * abort 是正常退出方式：调用方中止时本函数静默返回，不当作错误。
 */
export async function readEventStream(
  path: string,
  options: { query?: RequestOptions['query']; signal: AbortSignal },
  onMessage: (message: StreamMessage) => void,
): Promise<void> {
  const url = buildUrl(path, options.query)
  const headers: Record<string, string> = { Accept: 'text/event-stream' }
  if (currentToken !== '') {
    headers.Authorization = `Bearer ${currentToken}`
  }

  let response: Response
  try {
    response = await fetch(url, { headers, signal: options.signal })
  } catch {
    if (options.signal.aborted) {
      return
    }
    throw new ApiError(0, 'network_error', '无法连接到后端服务：请确认机器人进程已启动、admin 已开启且监听地址可达')
  }

  if (response.status === 401) {
    clearToken()
    throw new ApiError(401, 'unauthorized', '登录凭证已失效（HTTP 401），请重新输入后台 token')
  }
  if (!response.ok) {
    const envelope = errorEnvelope(await readJson(response))
    if (envelope) {
      throw new ApiError(response.status, envelope.code, envelope.message, envelope.detail)
    }
    throw new ApiError(response.status, 'http_error', `日志流建立失败（HTTP ${response.status}）`)
  }
  if (response.body === null) {
    throw new ApiError(0, 'no_stream', '服务端没有返回可读的流')
  }

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) {
        return
      }
      buffer += decoder.decode(value, { stream: true })
      // 事件之间以空行分隔；CRLF 与 LF 都要认（有些代理会改写）
      buffer = buffer.replace(/\r\n/g, '\n')
      let end = buffer.indexOf('\n\n')
      while (end >= 0) {
        const message = parseEvent(buffer.slice(0, end))
        buffer = buffer.slice(end + 2)
        if (message !== null) {
          onMessage(message)
        }
        end = buffer.indexOf('\n\n')
      }
    }
  } catch (cause) {
    if (options.signal.aborted) {
      return
    }
    throw toApiError(cause)
  } finally {
    reader.releaseLock()
  }
}
