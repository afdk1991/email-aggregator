/**
 * 统一邮件领域模型（Canonical Mail Model）。
 * 这是平台内部唯一表示，屏蔽 IMAP / POP3 / Exchange(Graph) / Gmail 协议差异。
 * 适配器负责：协议 ↔ CanonicalMail 的双向映射。
 */

export type FolderName =
  | "INBOX"
  | "SENT"
  | "DRAFTS"
  | "TRASH"
  | "JUNK"
  | "CUSTOM";

export interface Contact {
  name?: string;
  address: string; // RFC5322 addr-spec
}

export interface Attachment {
  filename: string;
  mimeType: string;
  sizeBytes: number;
  /** 内容寻址哈希（sha256），用于去重：相同附件只存一份 */
  contentHash: string;
  /** 内容存储中的对象键（由 content store 回填） */
  objectKey?: string;
}

export interface MailFlags {
  seen: boolean;
  flagged: boolean;
  answered: boolean;
  deleted: boolean;
}

/**
 * 同步游标：各协议用自己的字段，统一封装。
 * 仅适配层关心，不进入检索模型。
 */
export interface SyncCursor {
  uidValidity?: string; // IMAP
  uidNext?: string; // IMAP
  highestModSeq?: string; // IMAP CONDSTORE / QRESYNC
  historyId?: string; // Gmail
  deltaLink?: string; // Graph (Microsoft)
  uidlWatermark?: string; // POP3
}

export interface CanonicalMail {
  /** 幂等去重键 = (tenantId, accountId, folder, messageId) */
  idempotencyKey: string;
  /** 租户 ID（ADR-009）：逻辑多租户隔离主键，空值由下游 Resolve 收敛到 DefaultTenantID */
  tenantId?: string;
  accountId: string;
  folder: FolderName;
  /** RFC822 Message-ID */
  messageId: string;
  from: Contact[];
  to: Contact[];
  cc: Contact[];
  subject: string;
  bodyText?: string;
  bodyHtml?: string;
  attachments: Attachment[];
  flags: MailFlags;
  /** 收件时间戳（ms） */
  internalDate: number;
  sizeBytes: number;
  /** 协议侧游标（断点续传） */
  cursor: SyncCursor;
  /** 内容存储中的原始 MIME 对象键（用于从对象存储还原正文/附件） */
  rawObjectKey?: string;
}

export interface Folder {
  name: FolderName;
  /** 统一文件夹路径（多层级以 '/' 分隔） */
  path: string;
  messageCount: number;
  unreadCount: number;
}

/** 上游返回的"变化集"：新增 / 变更 / 删除的 UID 列表 */
export interface ChangeSet {
  added: string[];
  changed: string[];
  deleted: string[];
  /** 本批次后的最新游标，供持久化 */
  nextCursor: SyncCursor;
}

/**
 * 幂等去重键构造。
 * - 单租户（无 tenantId）：`${accountId}|${folder}|${messageId}` —— 与改造前完全等价（最小可逆）。
 * - 多租户（带 tenantId）：`${tenantId}|${accountId}|${folder}|${messageId}` —— 防止跨租户邮件碰撞。
 * 与 Go 实现对齐：email-aggregator-go 幂等键 `tid:accountId:mailId` 同义（分隔符为 TS 历史约定）。
 */
export function idempotencyKey(
  accountId: string,
  folder: FolderName,
  messageId: string,
  tenantId?: string,
): string {
  return tenantId
    ? `${tenantId}|${accountId}|${folder}|${messageId}`
    : `${accountId}|${folder}|${messageId}`;
}
