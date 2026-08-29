/**
 * 连接器（第三方邮箱适配器）契约。
 * 所有协议以适配器插件实现同一接口，框架按 providerType 路由（ADR-005 适配器注册表）。
 */

import type { CanonicalMail, ChangeSet, Folder, SyncCursor } from "./canonical.ts";

// 供 connector 实现层（如 imap.ts）按名导入统一模型类型
export type { ChangeSet, Folder } from "./canonical.ts";

export type ProviderType = "imap" | "pop3" | "exchange" | "gmail" | "enterprise";

export type AuthMethod = "oauth2" | "password" | "app-password";

export interface ConnectorCapabilities {
  incremental: boolean; // 增量（IMAP QRESYNC / Graph delta / Gmail historyId）
  push: boolean; // 服务端推送（IMAP IDLE / Webhook）
  folders: boolean; // 多文件夹（POP3 = false）
  flags: boolean; // 旗帜 / 已读（POP3 = false）
  maxPageSize: number;
}

/** 经 KMS 解密后的短期内存凭据，绝不下盘 */
export interface AccountCredential {
  accountId: string;
  providerType: ProviderType;
  authMethod: AuthMethod;
  /** OAuth2: access/refresh token；password: user/pass 或 app-password */
  accessToken?: string;
  refreshToken?: string;
  username?: string;
  secret?: string;
  host?: string;
  port?: number;
  /** 连接参数（IMAP 是否用 TLS、超时等） */
  options?: Record<string, string | number | boolean>;
}

export interface Session {
  accountId: string;
  providerType: ProviderType;
  /** 连接建立时间（ms） */
  establishedAt: number;
  /** 连接级上下文（如 IMAP 标签、socket 句柄）——仅运行期有效 */
  ctx: Map<string, unknown>;
}

export interface RawMessage {
  uid: string;
  /** 完整 MIME 字节（适配器拉取后透传） */
  mime: Uint8Array;
  sizeBytes: number;
  flags?: { seen?: boolean; flagged?: boolean; answered?: boolean; deleted?: boolean };
  internalDate?: number;
}

export interface PushHook {
  /** 上游推送新邮件到达时回调（IMAP IDLE EXISTS / Webhook） */
  onExists(folder: string, newCount: number): void;
}

export interface Subscription {
  folder: string;
  active: boolean;
  unsubscribe(): Promise<void>;
}

export interface SendResult {
  ok: boolean;
  providerMessageId?: string;
  error?: string;
}

export interface OutboundMessage {
  from: string;
  to: string[];
  subject: string;
  bodyText?: string;
  bodyHtml?: string;
}

export interface Health {
  ok: boolean;
  latencyMs: number;
  detail?: string;
}

/**
 * 统一连接器接口。
 * 框架只依赖此接口；具体协议差异被隔离在实现内。
 */
export interface Connector {
  readonly providerType: ProviderType;
  readonly capabilities: ConnectorCapabilities;

  connect(account: AccountCredential): Promise<Session>;

  listFolders(session: Session): Promise<Folder[]>;

  /** 增量拉取：以 cursor 为游标，返回新/变更邮件 UID 列表 */
  fetchChanges(session: Session, folder: string, cursor: SyncCursor): Promise<ChangeSet>;

  /** 按 UID 拉取完整邮件（含 MIME） */
  fetchMessage(session: Session, folder: string, uid: string): Promise<RawMessage>;

  /** 订阅推送；不支持则抛 NotImplemented */
  subscribe(session: Session, folder: string, hook: PushHook): Promise<Subscription>;

  send(account: AccountCredential, message: OutboundMessage): Promise<SendResult>;

  healthCheck(session: Session): Promise<Health>;
}

export class NotImplemented extends Error {
  constructor(method: string) {
    super(`method not implemented: ${method}`);
    this.name = "NotImplemented";
  }
}
