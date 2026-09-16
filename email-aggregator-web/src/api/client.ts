// 网络层：所有 REST 调用集中在此，页面/组件不直接 fetch。
// 端点契约来自 email-aggregator-go/src/api/server.go（同源 /api 经 vite 代理到 :8080）。
import type {
  AccountInfo,
  AccountStatus,
  CanonicalMail,
  ChatRequest,
  ChatResponse,
  NotificationPayload,
  Provider,
  SearchHit,
} from '../types'

// ── API 基址解析 ───────────────────────────────────────────────────────────
//
// 六端部署下，页面的 origin 各不相同，不能一律用相对路径 '/api'：
//   · Web 直访 / Go 单二进制（webui 标签）→ 与 API 同源，用相对路径即可
//   · Electron 桌面端                      → 加载 127.0.0.1:<随机端口>，仍同源
//   · Capacitor（Android / iOS）           → origin 是 https://localhost，必须绝对地址
//   · HarmonyOS Web 容器                   → origin 是 resource://rawfile，必须绝对地址
//
// 解析优先级：localStorage['apiBase']（运行期）> VITE_API_BASE_URL（构建期）> 同源。
// 保留运行期覆盖是为了让自部署用户不必重新打包即可指向自己的服务器。
export function resolveApiOrigin(): string {
  let override = ''
  try {
    override = localStorage.getItem('apiBase') ?? ''
  } catch {
    /* 隐私模式或非浏览器环境无 storage */
  }
  const raw = (override || import.meta.env.VITE_API_BASE_URL || '').trim()
  return raw.replace(/\/+$/, '')
}

const BASE = `${resolveApiOrigin()}/api`

/**
 * 与 REST 同源的 WebSocket 端点（ws/wss 跟随基址协议）。
 * 抽到网络层统一解析，避免 App 里再写一遍 origin 推断逻辑而两处漂移。
 */
export function wsEndpoint(accountId: string): string {
  const origin = resolveApiOrigin()
  const wsBase = origin
    ? origin.replace(/^https:/i, 'wss:').replace(/^http:/i, 'ws:')
    : `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}`
  return `${wsBase}/ws?accountId=${encodeURIComponent(accountId)}`
}

async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url)
  if (!r.ok) throw new Error(`HTTP ${r.status}`)
  return (await r.json()) as T
}

export interface HealthResp {
  ok: boolean
}
export function health(): Promise<HealthResp> {
  return getJSON<HealthResp>(`${BASE}/health`)
}

export interface MailsResp {
  accountId: string
  count: number
  mails: CanonicalMail[]
}
export function listMails(accountId: string, folder = 'INBOX', limit = 50): Promise<MailsResp> {
  const qs = new URLSearchParams({ accountId, folder, limit: String(limit) })
  return getJSON<MailsResp>(`${BASE}/mails?${qs.toString()}`)
}

// getMail 按账户+ID 取单封邮件的权威资源（服务端真实数据），供详情模态使用。
export interface MailResp {
  mail: CanonicalMail
}
export function getMail(accountId: string, id: string): Promise<CanonicalMail> {
  const qs = new URLSearchParams({ accountId })
  return getJSON<CanonicalMail>(`${BASE}/mails/${encodeURIComponent(id)}?${qs.toString()}`)
}

export interface SearchResp {
  accountId: string
  query: string
  hits: SearchHit[]
}
export function searchMails(accountId: string, q: string): Promise<SearchResp> {
  const qs = new URLSearchParams({ accountId, q })
  return getJSON<SearchResp>(`${BASE}/search?${qs.toString()}`)
}

// 账户注册表：返回后端已种子账户及各自未读数，驱动前端切换 chips 与未读徽标。
export interface AccountsResp {
  accounts: AccountInfo[]
}
export function listAccounts(): Promise<AccountsResp> {
  return getJSON<AccountsResp>(`${BASE}/accounts`)
}

// 新增账户（POST /api/accounts）。
export interface NewAccountInput {
  id: string
  provider: Provider
  email: string
  displayName?: string
  status?: AccountStatus
  syncFolder?: string
  serverHost?: string
}
export async function createAccount(input: NewAccountInput): Promise<AccountInfo> {
  const r = await fetch(`${BASE}/accounts`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(input),
  })
  if (!r.ok) {
    const err = (await r.json().catch(() => ({}))) as { error?: string }
    throw new Error(err.error || `HTTP ${r.status}`)
  }
  return (await r.json()) as AccountInfo
}

// saveAccountCredentials 录入/更新账户凭据（授权码/密码），服务端 KMS 信封加密后存 credentialsRef。
export async function saveAccountCredentials(
  accountId: string,
  username: string,
  password: string,
): Promise<void> {
  const r = await fetch(`${BASE}/accounts/${encodeURIComponent(accountId)}/credentials`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  if (!r.ok) {
    const err = (await r.json().catch(() => ({}))) as { error?: string }
    throw new Error(err.error || `HTTP ${r.status}`)
  }
}

// syncAccount 后台触发一次账户真实同步（POST /api/accounts/{id}/sync，202）。
export async function syncAccount(accountId: string): Promise<void> {
  const r = await fetch(`${BASE}/accounts/${encodeURIComponent(accountId)}/sync`, {
    method: 'POST',
  })
  if (!r.ok) {
    const err = (await r.json().catch(() => ({}))) as { error?: string }
    throw new Error(err.error || `HTTP ${r.status}`)
  }
}

// 更新账户状态（PUT /api/accounts/{id}，部分更新）。
export async function updateAccountStatus(accountId: string, status: AccountStatus): Promise<AccountInfo> {
  const r = await fetch(`${BASE}/accounts/${encodeURIComponent(accountId)}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ id: accountId, status }),
  })
  if (!r.ok) {
    const err = (await r.json().catch(() => ({}))) as { error?: string }
    throw new Error(err.error || `HTTP ${r.status}`)
  }
  return (await r.json()) as AccountInfo
}

// 删除账户（DELETE /api/accounts/{id}）。
export async function deleteAccount(accountId: string): Promise<void> {
  const r = await fetch(`${BASE}/accounts/${encodeURIComponent(accountId)}`, { method: 'DELETE' })
  if (!r.ok) throw new Error(`HTTP ${r.status}`)
}

// setRead 设置邮件已读/未读（POST /api/mails/{id}/read，body {read}）。
export async function setRead(accountId: string, id: string, read: boolean): Promise<void> {
  const qs = new URLSearchParams({ accountId })
  const r = await fetch(`${BASE}/mails/${encodeURIComponent(id)}/read?${qs.toString()}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ read }),
  })
  if (!r.ok) throw new Error(`HTTP ${r.status}`)
}

// deleteMail 删除邮件（DELETE /api/mails/{id}）。
export async function deleteMail(accountId: string, id: string): Promise<void> {
  const qs = new URLSearchParams({ accountId })
  const r = await fetch(`${BASE}/mails/${encodeURIComponent(id)}?${qs.toString()}`, {
    method: 'DELETE',
  })
  if (!r.ok) throw new Error(`HTTP ${r.status}`)
}

// AI 网关入口（ADR-010）：基础 demo 未挂载时返回 422/405，前端做降级提示。
export async function aiChat(req: ChatRequest): Promise<ChatResponse> {
  const r = await fetch(`${BASE}/ai/chat`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(req),
  })
  if (!r.ok) {
    const err = (await r.json().catch(() => ({}))) as { error?: string }
    throw new Error(err.error || `HTTP ${r.status}`)
  }
  return (await r.json()) as ChatResponse
}

// demoPush 触发一封演示新邮件（后端 /api/demo/push），用于验证 WebSocket 实时推送。
// 真实环境由同步流水线自动触发，这里仅作联调入口。
export interface DemoPushResp {
  ok: boolean
  id: string
  subject: string
}
export async function demoPush(accountId: string): Promise<DemoPushResp> {
  const qs = new URLSearchParams({ accountId })
  const r = await fetch(`${BASE}/demo/push?${qs.toString()}`, { method: 'POST' })
  if (!r.ok) throw new Error(`HTTP ${r.status}`)
  return (await r.json()) as DemoPushResp
}

// 实时通知载荷（与 Go notify.NotificationPayload 对齐）
export type { NotificationPayload }
