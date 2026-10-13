/* =========================================================================
   与 docs/frontend/api-contract.md / 冻结接口形状一一对应的手写类型。
   约定：时间为 Unix 秒（number）；错误体为 { error: { code, message } }。
   禁止 any —— 未知形状一律用 unknown + 运行时判窄。
   ========================================================================= */

/** 统一错误体 */
export interface ApiErrorBody {
  error: {
    code: string
    message: string
    detail?: unknown
  }
}

/* ------------------------------------------------------------- /status */
export interface FeatureStatus {
  name: string
  enabled: boolean
  running: boolean
}

export interface DatabaseInfo {
  driver: string
  size_bytes: number
}

export interface RedisInfo {
  addr: string
  db: number
  prefix: string
}

/**
 * 在线人机验证服务的运行状态。
 *
 * configured 读的是文件（human_verification 段写没写），connected 读的是进程里
 * 那条长连接现在通不通 —— 两者是两件事：写了段但连不上时，链接一条也签发不出去，
 * 开了在线验证的群会退回群内按钮。
 */
export interface HumanVerificationStatus {
  configured: boolean
  connected: boolean
}

export interface StatusResponse {
  version: string
  started_at: number
  uptime_seconds: number
  config_path: string
  features: FeatureStatus[]
  groups_managed: number
  database: DatabaseInfo
  redis: RedisInfo
  command_prefix: string
  judged_model: string
  /** 没有 human_verification 段的部署：configured=false, connected=false */
  human_verification: HumanVerificationStatus
}

/* ------------------------------------------------------------ /overview */
export interface BlacklistCountByGroup {
  group_openid: string
  count: number
}

export interface BlacklistOverview {
  total: number
  by_group: BlacklistCountByGroup[]
}

export interface MemberEventCounts {
  total: number
  joined: number
  left: number
  scanned: number
}

export interface JudgementCounts {
  total: number
  violations: number
  ok: number
  errors: number
  /** true = 每群只读最近 N 条，这里是「最新的那部分」而不是全部 */
  truncated: boolean
}

export interface OverviewResponse {
  since: number
  groups: number
  pending: number
  high_risk: number
  blacklist: BlacklistOverview
  member_events: MemberEventCounts
  judgements: JudgementCounts
  /**
   * 后端说明哪个数字是下界、哪个问题因为配置问不出来。
   * 有界计数必须原样展示，否则等于把「只统计了最近 500 条」当成精确值给运维看。
   */
  notes?: string[]
}

/* -------------------------------------------------------------- /groups */
export interface GroupFeature {
  name: string
  global_enabled: boolean
  /** null = 继承全局（三态） */
  group_override: boolean | null
  effective: boolean
}

export interface Group {
  openid: string
  qq_group_id: number
  features: GroupFeature[]
}

export interface GroupsResponse {
  groups: Group[]
  /** true = bot.groups 为空，机器人对所在的所有群生效（枚举不出来，所以列表为空） */
  unrestricted: boolean
}

/* ------------------------------------------------------------- /pending */
export interface PendingVerification {
  token: string
  group_openid: string
  member_openid: string
  joined_at: number
  deadline: number
  held_until: number
  escalated_at: number
  reported: boolean
  /** 后端序列化后的 JSON 字符串（Go 字段名，如 {"MuteMode":"pseudo"}）。不透明，
   *  要判断扣留方式请用下面的 mute_mode —— 那是后端解好的，不必自己解析。 */
  settings: string
  /** mute / pseudo，后端从 settings 里解出来的；解不开时为空串 */
  mute_mode: string
  /** 配置的验证期限（小时），解不开时为 0 */
  deadline_hours: number
  /** 伪禁言下还能发几条就转真禁言，未启用时为 0 */
  left_to_talk: number
  /**
   * 这条记录要本人怎么证明自己是人：true = 浏览器里的在线验证（卡片上是一条
   * 只有本人能点开的链接），false = 群内按钮，null = 这条记录的 settings 里没写
   * （更早写入的条目）。
   *
   * 是成员进群时冻结下来的设置，不是本群当前的设置：先被按住的人继续用他当时
   * 那张卡片。
   */
  online_verify: boolean | null
}

export interface PendingResponse {
  pending: PendingVerification[]
}

/* -------------------------------------------- 验证队列的三个动作（写接口） */
export interface VerificationActionResponse {
  token: string
  group_openid: string
  member_openid: string
  /** resend / approve / reject */
  action: string
  /** 给运维看的一句话 */
  message: string
  /** 仅 reject 有意义：是否写入了黑名单 */
  blacklisted?: boolean
  /** 动作之后队列还剩多少条（后端重新数的） */
  pending: number
}

/* -------------------------------------------------------- /member-events */
export interface MemberEvent {
  group_openid: string
  group_name: string
  group_qq_id: number
  member_openid: string
  /** join / leave（保留 string，后端可能扩展） */
  kind: string
  event_at: number
}

export interface MemberEventsResponse {
  events: MemberEvent[]
  scanned: number
  truncated: boolean
}

/* --------------------------------------------------------- /judgements */
export interface JudgementRecall {
  id: string
  text: string
  recalled: boolean
  reason: string
}

export interface Judgement {
  id: string
  group_openid: string
  subject_openid: string
  reporter_openid: string
  /** ad / nsfw / abuse / other … */
  category: string
  /** 后端给的中文类别名（category 的可读形式），空串表示后端没给 */
  category_label: string
  /** violation / ok / error … */
  verdict: string
  model: string
  message_ids: string[]
  reason: string
  reasoning: string
  /** recall+mute / recall / none … */
  action: string
  mute_seconds: number
  recall_reason: string
  recalls: JudgementRecall[]
  created_at: number
}

export interface JudgementsResponse {
  judgements: Judgement[]
  /** true = 某个群返回了它一次能给的全部行，列表是「最新的那部分」 */
  truncated: boolean
  /** true = 没指定群且配置里也没有群，这个问题问不出来（不是「没有违规」） */
  group_required?: boolean
}

/* ---------------------------------------------------------- /blacklist */
export interface BlacklistEntry {
  id: string
  group_openid: string
  member_openid: string
  union_openid: string
  reason: string
  added_at: number
  added_by: string
  expires_at: number
}

export interface BlacklistResponse {
  entries: BlacklistEntry[]
}

/* ---------------------------------------------------------- /high-risk */
export interface HighRiskWatch {
  id: string
  group_openid: string
  member_openid: string
  reason: string
  added_at: number
  added_by: string
  expires_at: number
}

export interface HighRiskResponse {
  watches: HighRiskWatch[]
}

/* --------------------------------------------------------- /broadcasts */
export interface BroadcastTarget {
  group_openid: string
  message_id: string
}

export interface Broadcast {
  token: string
  sender_openid: string
  anonymous: boolean
  markdown: boolean
  content: string
  sent_at: number
  targets: BroadcastTarget[]
}

export interface BroadcastsResponse {
  broadcasts: Broadcast[]
  /** true = bot.groups 为空，广播记录是按目标群读的，没有群名单就一条都读不出来 */
  unrestricted?: boolean
}

/* ----------------------------------------------------------- /commands */
export interface CommandItem {
  name: string
  aliases: string[]
  usage: string
  desc: string
  scenes: string[]
  /** 谁可以执行："everyone" / "admins" / "whois" */
  audience: string
}

export interface CommandsResponse {
  prefix: string
  commands: CommandItem[]
}

/* ------------------------------------------------------------- /config */
export interface ConfigBotGroup {
  openid: string
  qq_group_id: number
}

export interface ConfigBot {
  qq: number
  groups: ConfigBotGroup[]
  /** 是否配了凭证（只说有没有，不说是什么） */
  credentials_configured: boolean
}

export interface ConfigLog {
  level: string
  format: string
}

export interface ConfigDatabase {
  driver: string
}

export interface ConfigRedis {
  addr: string
  db: number
  prefix: string
}

export interface ConfigFeature {
  name: string
  enabled: boolean
  groups: string[]
}

export interface ConfigAdmin {
  listen: string
  token_configured: boolean
  /** 非空 = 前端从磁盘目录而不是内嵌产物托管（开发时用） */
  web_dir?: string
}

/**
 * 配置里的 human_verification 段（脱密）。
 *
 * 两把令牌只说有没有、绝不说是什么：谁拿到 issue_token 就能给任意对象签发验证
 * 链接。
 */
export interface HumanVerificationConfig {
  configured: boolean
  /** 服务地址（结果长连接由它推导出来） */
  base_url: string
  /** 每条链接签上的业务标识；配置里留空时是后端自带的那个默认值 */
  cdata: string
  /** 一条链接的有效期（秒），配置里留空时是服务方默认值 */
  ttl_seconds: number
  /** 一次调用的超时（秒），配置里留空时是默认值 */
  timeout_seconds: number
  issue_token_configured: boolean
  ws_token_configured: boolean
}

export interface ConfigResponse {
  path: string
  bot: ConfigBot
  log: ConfigLog
  database: ConfigDatabase
  redis: ConfigRedis
  features: ConfigFeature[]
  admin: ConfigAdmin
  /** 本部署的在线人机验证；没写那一段时 configured=false */
  human_verification: HumanVerificationConfig
}

/** 删除类接口的回答：removed=false 表示本来就没有这条 */
export interface RemovalResponse {
  removed: boolean
}

/* ------------------------------------------------------ /config/reload */
export interface ReloadResponse {
  reloaded: boolean
}

/* --------------------------------------------------------------- /logs */
/** 一条日志里的一对结构化字段（后端已渲染成文本） */
export interface LogField {
  key: string
  value: string
}

export interface LogRecord {
  /** 从 1 开始、不重复；重连时带 after=它 就能只补缺口 */
  seq: number
  time: number
  /** DEBUG / INFO / WARN / ERROR（后端 slog.Level.String()） */
  level: string
  message: string
  attrs: LogField[]
}

export interface LogsResponse {
  records: LogRecord[]
  /** 后端实际应用的等级过滤（大写）；请求里写了它不认识的值时这里会体现 */
  level: string
  /** 缓冲区一次能装多少条 */
  capacity: number
  /** 现在装着多少条 */
  kept: number
  /** 进程启动以来被挤出去多少条（缓冲区满了） */
  dropped: number
  /** 最新一条的 seq，0 表示缓冲区是空的；重连时用它当 after */
  next_seq: number
}
