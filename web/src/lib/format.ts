/* =========================================================================
   展示层小工具：时间 / 时长 / 体积格式化，以及接口枚举值 → 中文标签。
   只做“翻译”，不产生任何接口没给的数字。
   ========================================================================= */

/** 当前 Unix 秒 */
export function unixNow(): number {
  return Math.floor(Date.now() / 1000)
}

/** 今天 00:00（本地时区）的 Unix 秒，用于 `/overview?since=` */
export function startOfTodayUnix(): number {
  const d = new Date()
  d.setHours(0, 0, 0, 0)
  return Math.floor(d.getTime() / 1000)
}

function pad2(n: number): string {
  return n < 10 ? `0${n}` : String(n)
}

/** HH:MM（seconds=true 时 HH:MM:SS）；时间戳为 0 或非法时返回 '—' */
export function formatClock(unixSeconds: number, withSeconds = false): string {
  if (!Number.isFinite(unixSeconds) || unixSeconds <= 0) {
    return '—'
  }
  const d = new Date(unixSeconds * 1000)
  const hhmm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  return withSeconds ? `${hhmm}:${pad2(d.getSeconds())}` : hhmm
}

/** YYYY-MM-DD HH:MM */
export function formatDateTime(unixSeconds: number): string {
  if (!Number.isFinite(unixSeconds) || unixSeconds <= 0) {
    return '—'
  }
  const d = new Date(unixSeconds * 1000)
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${formatClock(unixSeconds)}`
}

/** 相对时间：刚刚 / N 分钟前 / N 小时前 / N 天前 */
export function formatAgo(unixSeconds: number, now: number): string {
  if (!Number.isFinite(unixSeconds) || unixSeconds <= 0) {
    return '—'
  }
  const delta = now - unixSeconds
  if (delta < 0) {
    return `未来 ${formatSpan(-delta)}`
  }
  if (delta < 60) {
    return '刚刚'
  }
  return `${formatSpan(delta)}前`
}

/** 秒 → 「1 分钟」「4 小时 12 分」「2 天」 */
export function formatSpan(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds))
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  if (days > 0) {
    return hours > 0 ? `${days} 天 ${hours} 小时` : `${days} 天`
  }
  if (hours > 0) {
    return minutes > 0 ? `${hours} 小时 ${minutes} 分` : `${hours} 小时`
  }
  if (minutes > 0) {
    return `${minutes} 分钟`
  }
  return `${total} 秒`
}

/** 倒计时：已到期 / N 分钟 */
export function formatCountdown(targetUnix: number, now: number): string {
  if (!Number.isFinite(targetUnix) || targetUnix <= 0) {
    return '—'
  }
  const remain = targetUnix - now
  if (remain <= 0) {
    return '已到期'
  }
  return formatSpan(remain)
}

/** 时长（秒）→ 「30 分钟」；0 或负值 → '—' */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds) || seconds <= 0) {
    return '—'
  }
  return formatSpan(seconds)
}

/** 字节 → 「3.2 MB」 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return '—'
  }
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  const digits = unit === 0 ? 0 : 1
  return `${value.toFixed(digits)} ${units[unit] ?? 'B'}`
}

/** 长 openid 截断显示：前 8 … 后 4 */
export function shortId(value: string, head = 8, tail = 4): string {
  if (value === '') {
    return '—'
  }
  if (value.length <= head + tail + 1) {
    return value
  }
  return `${value.slice(0, head)}…${value.slice(-tail)}`
}

/** 文本为空时的占位 */
export function orDash(value: string | null | undefined): string {
  if (value === null || value === undefined || value.trim() === '') {
    return '—'
  }
  return value
}

/**
 * 群号列的占位。
 *
 * 0 不是一个群号，是「配置里没写 qq_group_id」的零值：官方平台只给 openid，
 * 群号要靠 OneBot 的群列表对出来，没对出来时配置里就空着。打印成 0 会让人
 * 以为真有个 0 号群，所以这里显示破折号。
 */
export function qqGroupIdLabel(id: number): string {
  return id > 0 ? String(id) : '—'
}

/* --------------------------------------------------------------- 标签映射 */

/** 功能 key → 中文名（接口返回的 name 与 config.yaml 的 feature key 同名） */
const FEATURE_LABELS: Record<string, string> = {
  member_log: '成员事件记录',
  mute_guard: '禁言守护',
  join_verification: '入群验证',
  join_request: '入群申请',
  moderation: '违规审核',
  admin_commands: '管理命令',
  broadcast: '群广播',
  high_risk: '高风险监控',
}

export function featureLabel(name: string): string {
  return FEATURE_LABELS[name] ?? name
}

/** 违规类别 → 中文 */
const CATEGORY_LABELS: Record<string, string> = {
  ad: '广告',
  nsfw: '色情',
  porn: '色情',
  abuse: '辱骂',
  insult: '辱骂',
  other: '其他',
}

export function categoryLabel(category: string): string {
  return CATEGORY_LABELS[category] ?? orDash(category)
}

/**
 * 类别显示名：后端给的 `category_label` 优先。
 * 类别名来自配置（features.moderation.categories），前端那张表只是兜底，
 * 配置里加了新类别时后端仍然给得出中文。
 */
export function categoryDisplay(category: string, serverLabel: string): string {
  return serverLabel !== '' ? serverLabel : categoryLabel(category)
}

export type Tone = 'neutral' | 'coral' | 'warn' | 'danger' | 'success' | 'primary'

export function categoryTone(category: string): Tone {
  switch (category) {
    case 'ad':
      return 'danger'
    case 'nsfw':
    case 'porn':
      return 'coral'
    case 'abuse':
    case 'insult':
      return 'warn'
    default:
      return 'neutral'
  }
}

/** 判定结论 → 中文 + 色 */
const VERDICT_LABELS: Record<string, string> = {
  violation: '违规',
  ok: '放行',
  pass: '放行',
  error: '判定错误',
}

export function verdictLabel(verdict: string): string {
  return VERDICT_LABELS[verdict] ?? orDash(verdict)
}

export function verdictTone(verdict: string): Tone {
  switch (verdict) {
    case 'violation':
      return 'danger'
    case 'ok':
    case 'pass':
      return 'success'
    case 'error':
      return 'warn'
    default:
      return 'neutral'
  }
}

/**
 * 处置动作 → 中文。
 *
 * 这一列**不是**一个封闭的枚举：举报流程会把做成了什么拼成一句话写进去
 * （`internal/features/admincmd/report.go` 里 `strings.Join(notes, " ")`，
 * 例如「已禁言 10 分钟 撤回失败（…）」），所以兜底必须是"原样显示"而不是
 * "回退到未知"。下面这些是真正由代码写的短标记。
 */
const ACTION_LABELS: Record<string, string> = {
  none: '无处置',
  pass: '放行',
  recall: '撤回',
  mute: '禁言',
  kick: '移出',
  'recall+mute': '撤回 + 禁言',
  dry_run: '试运行（未执行）',
  // 举报没查出违规时，反过来罚举报者本人
  report_penalty: '禁言举报者',
  report_penalty_failed: '禁言举报者失败',
}

export function actionLabel(action: string): string {
  if (action === '') {
    return '—'
  }
  const known = ACTION_LABELS[action]
  if (known !== undefined) {
    return known
  }
  return action
    .split('+')
    .map((part) => ACTION_LABELS[part] ?? part)
    .join(' + ')
}

export function actionTone(action: string): Tone {
  if (action.includes('mute') || action.includes('kick')) {
    return 'warn'
  }
  if (action.includes('penalty')) {
    // 没有违规却罚了举报者（或罚不动）：两种情况都该被一眼看到
    return 'warn'
  }
  if (action.includes('recall')) {
    return 'coral'
  }
  if (action === '' || action === 'none' || action === 'pass') {
    return 'neutral'
  }
  return 'neutral'
}

/** 成员事件 kind → 中文 */
export function memberEventLabel(kind: string): string {
  switch (kind) {
    case 'join':
      return '入群'
    case 'leave':
      return '退群'
    default:
      return orDash(kind)
  }
}

const SCENE_LABELS: Record<string, string> = {
  group: '群聊',
  private: '私聊',
  all: '全部',
}

export function sceneLabel(scene: string): string {
  return SCENE_LABELS[scene] ?? scene
}

/**
 * 命令的执行权限（后端 command.Audience）→ 中文。
 * everyone 任意群成员；admins 只认配置里的本群管理员（QQ 群主/管理员不算）；
 * whois 是「管理员、还没配置管理员的群、或关掉了限制的部署」。
 */
export function audienceLabel(audience: string): string {
  switch (audience) {
    case 'admins':
      return '仅管理员'
    case 'whois':
      return '管理员 / 未配置'
    case 'everyone':
      return '所有人'
    default:
      return orDash(audience)
  }
}

export function audienceTone(audience: string): Tone {
  switch (audience) {
    case 'admins':
      return 'coral'
    case 'whois':
      return 'warn'
    default:
      return 'neutral'
  }
}

/**
 * 后端解好的 mute_mode → 中文。
 *
 * 取值来自 joinverify 的常量：MuteModeMute = "mute"（平台真禁言）、
 * MuteModePseudo = "pseudo"（不真禁言，靠撤回消息）。
 *
 * 这里刻意**不去解析 row.settings**：那一列是后端自己的结构，用 Go 字段名序列化
 * （"MuteMode"），照着它写解析等于把后端的字段名抄进前端，改一次名就静默失效。
 * 后端已经把这两个值解出来了，用它们。
 */
export function muteModeLabel(muteMode: string): string | null {
  switch (muteMode) {
    case 'pseudo':
      return '伪禁言'
    case 'mute':
      return '真禁言'
    case '':
      return null
    default:
      return muteMode
  }
}

/** 不区分大小写的包含匹配（搜索框过滤用） */
export function matches(haystack: readonly string[], needle: string): boolean {
  const q = needle.trim().toLowerCase()
  if (q === '') {
    return true
  }
  return haystack.some((item) => item.toLowerCase().includes(q))
}

/* ------------------------------------------------------------- 到期与名单 */

export type ExpiryState = 'permanent' | 'active' | 'expired'

/**
 * 一条带到期时间的名单条目的状态。
 *
 * `expiresAt === 0` 是「永久」，这不是猜的：`store.Barred.ExpiresAt` 的注释就写着
 * 「0 表示永久」。而高风险名单反过来 —— 那里的 0 会被 store 拒绝，所以这个函数
 * 在高风险页面上永远给不出 permanent，这是数据本身的性质，不是判断失误。
 */
export function expiryState(expiresAt: number, now: number): ExpiryState {
  if (expiresAt === 0) {
    return 'permanent'
  }
  return expiresAt <= now ? 'expired' : 'active'
}

export function expiryStateLabel(state: ExpiryState): string {
  switch (state) {
    case 'permanent':
      return '永久'
    case 'expired':
      return '已过期'
    default:
      return '生效中'
  }
}

export function expiryStateTone(state: ExpiryState): Tone {
  switch (state) {
    case 'permanent':
      return 'warn'
    case 'expired':
      return 'neutral'
    default:
      return 'success'
  }
}

/** 到期时间 → 一句能直接看的话：永久 / 已过期 / 还剩 3 小时 */
export function expiryNote(expiresAt: number, now: number): string {
  const state = expiryState(expiresAt, now)
  if (state === 'permanent') {
    return '永不过期'
  }
  if (state === 'expired') {
    return `${formatAgo(expiresAt, now)}过期`
  }
  return `还剩 ${formatSpan(expiresAt - now)}`
}

/* ----------------------------------------------------------------- 日志等级 */

/** DEBUG / INFO / WARN / ERROR（后端 slog.Level.String()）→ 色。等级名本身不翻译：
 *  这四个词是所有日志工具的共同语言，译成中文反而不好搜。 */
export function levelTone(level: string): Tone {
  switch (level) {
    case 'ERROR':
      return 'danger'
    case 'WARN':
      return 'warn'
    case 'DEBUG':
      return 'neutral'
    default:
      return 'success'
  }
}

/* --------------------------------------------------------------- 文本处理 */

/** 长文本预览：超过 max 就截断并加省略号（title 里放全文） */
export function preview(text: string, max: number): string {
  const flat = text.replace(/\s+/g, ' ').trim()
  if (flat === '') {
    return '—'
  }
  return flat.length <= max ? flat : `${flat.slice(0, max)}…`
}
