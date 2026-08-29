// 前端契约类型：与 Go 主干版 model.CanonicalMail / aigateway.* / search.SearchHit
// 的 JSON 契约一一对齐（ADR-005 归一化模型）。字段命名保持 camelCase，与后端 json tag 一致。

export interface Address {
  name: string
  email: string
}

export interface AttachmentMeta {
  filename: string
  contentType: string
  sizeBytes: number
  contentHash: string
}

export interface SyncCursor {
  uidValidity?: number
  uidNext?: number
  lastUid?: number
  modseq?: number
  highWaterMark?: string
  providerSpecific?: Record<string, string>
}

export type Provider = 'imap' | 'pop3' | 'exchange' | 'gmail' | 'graph' | 'enterprise'

export interface CanonicalMail {
  id: string
  accountId: string
  provider: Provider
  folder: string
  from: Address
  to: Address[]
  cc: Address[]
  bcc: Address[]
  subject: string
  bodyText?: string
  bodyHTML?: string
  snippet?: string
  hasAttachment: boolean
  attachments?: AttachmentMeta[]
  internalDate: number
  sizeBytes: number
  cursor: SyncCursor
  read?: boolean
  rawObjectKey?: string
}

// 检索命中（Go search.SearchHit）
export interface SearchHit {
  idempotencyKey: string
  accountId: string
  subject: string
  from: string
  preview: string
  internalDate: number
}

// ---- AI 网关（ADR-010）契约 ----
export type Capability = 'chat' | 'embed' | 'classify' | 'summarize' | 'extract'
export type TenantTier = 'private' | 'enterprise' | 'public'
export type Role = 'system' | 'user' | 'model'

export interface ChatMessage {
  role: Role
  content: string
}

export interface ChatRequest {
  tenantId: string
  tenantTier: TenantTier
  accountId?: string
  capability: Capability
  messages: ChatMessage[]
  sensitivity: 0 | 1
  userConsented: boolean
}

export interface ChatResponse {
  content: string
  backend: string
  model?: string
  tokensIn: number
  tokensOut: number
  redacted: boolean
}

// ---- 实时推送（WebSocket /ws）契约 ----
// 与 Go notify.NotificationPayload 对齐（ADR-005 归一化模型）。
export type NotifyKind = 'new-mail' | 'sync-state' | 'error' | 'mail-updated' | 'mail-deleted'

export interface NotificationPayload {
  kind: NotifyKind
  accountId: string
  preview?: string
  ts: number
  mailId?: string
  read?: boolean
}

// 账户信息（连接/账户服务），来自 /api/accounts
export type AccountStatus = 'active' | 'paused' | 'error'

export interface AccountInfo {
  id: string
  unread: number
  provider?: Provider
  email?: string
  displayName?: string
  status?: AccountStatus
  syncFolder?: string
  lastSyncAt?: number
}
