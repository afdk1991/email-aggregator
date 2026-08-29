// 网络层：所有 REST 调用集中在此，页面/组件不直接 fetch。
// 端点契约来自 email-aggregator-go/src/api/server.go（同源 /api 经 vite 代理到 :8080）。
import type {
  CanonicalMail,
  ChatRequest,
  ChatResponse,
  NotificationPayload,
  SearchHit,
} from '../types'

const BASE = '/api'

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
export interface AccountInfo {
  id: string
  unread: number
}
export interface AccountsResp {
  accounts: AccountInfo[]
}
export function listAccounts(): Promise<AccountsResp> {
  return getJSON<AccountsResp>(`${BASE}/accounts`)
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
