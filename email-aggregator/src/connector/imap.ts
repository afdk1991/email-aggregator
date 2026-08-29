/**
 * IMAP 连接器（PoC）。
 *
 * 真实环境：connect() 建立 IMAP（TLS）连接，capability 探测后用
 *   IDLE（推送）/ QRESYNC+CONDSTORE（增量，基于 UIDVALIDITY/UIDNEXT/HIGHESTMODSEQ）
 *   / UIDL（POP3 全量去重）。本文件用 InMemoryMailServer 模拟上游，
 *   IMAP 特定的「游标语义 / IDLE 推送」逻辑保持真实可平移。
 *
 * 关键设计：
 *  - fetchChanges 以 highestModSeq 为游标 → 对应 IMAP QRESYNC 的 HIGHESTMODSEQ。
 *  - subscribe 模拟 IMAP IDLE：上游新邮件到达即回调 onExists。
 *  - MIME 解析走 parseRawMessage（最小可用 RFC822 头解析，附件解析留 TODO）。
 */

import {
  type AccountCredential,
  type ChangeSet,
  type Connector,
  type ConnectorCapabilities,
  type Folder,
  type Health,
  type NotImplemented,
  type OutboundMessage,
  type PushHook,
  type RawMessage,
  type SendResult,
  type Session,
  type Subscription,
  NotImplemented as NotImplementedErr,
} from "../model/connector.ts";
import type { FolderName, SyncCursor } from "../model/canonical.ts";

export interface StoredMessage {
  uid: string;
  modSeq: number;
  from: string;
  to: string[];
  subject: string;
  bodyText: string;
  internalDate: number;
  flags: { seen: boolean; flagged: boolean; answered: boolean; deleted: boolean };
}

/** 演示用内存邮件服务器，模拟 IMAP 上游（含 HIGHESTMODSEQ 与 IDLE 推送） */
export class InMemoryMailServer {
  private messages: StoredMessage[] = [];
  private nextUid = 1;
  private nextModSeq = 1;
  private idleHooks: PushHook[] = [];

  /** 注入一封新邮件（演示 INCREMENTAL 推送） */
  inject(env: Omit<StoredMessage, "uid" | "modSeq">): StoredMessage {
    const msg: StoredMessage = {
      ...env,
      uid: String(this.nextUid++),
      modSeq: this.nextModSeq++,
    };
    this.messages.push(msg);
    const count = this.messages.length;
    for (const h of this.idleHooks) h.onExists("INBOX", count);
    return msg;
  }

  highestModSeq(): number {
    return this.nextModSeq - 1;
  }

  fetchChangesSince(sinceModSeq: number): ChangeSet {
    const added = this.messages
      .filter((m) => m.modSeq > sinceModSeq)
      .map((m) => m.uid);
    return {
      added,
      changed: [],
      deleted: [],
      nextCursor: { highestModSeq: String(this.highestModSeq()) },
    };
  }

  get(uid: string): StoredMessage | undefined {
    return this.messages.find((m) => m.uid === uid);
  }

  all(): StoredMessage[] {
    return [...this.messages];
  }

  subscribe(hook: PushHook): Subscription {
    this.idleHooks.push(hook);
    return {
      folder: "INBOX",
      active: true,
      unsubscribe: async () => {
        this.idleHooks = this.idleHooks.filter((h) => h !== hook);
      },
    };
  }
}

const CAPS: ConnectorCapabilities = {
  incremental: true,
  push: true,
  folders: true,
  flags: true,
  maxPageSize: 50,
};

/** 构造最小 RFC822 报文（演示用；真实 IMAP 拉的是完整 MIME） */
export function buildMime(m: StoredMessage, messageId: string): Uint8Array {
  const headers = [
    `From: ${m.from}`,
    `To: ${m.to.join(", ")}`,
    `Subject: ${m.subject}`,
    `Message-ID: <${messageId}>`,
    `Date: ${new Date(m.internalDate).toUTCString()}`,
    `X-ModSeq: ${m.modSeq}`,
    "",
    m.bodyText,
  ];
  return new TextEncoder().encode(headers.join("\r\n"));
}

export interface ParsedEnvelope {
  messageId: string;
  from: string;
  to: string[];
  subject: string;
  bodyText: string;
  internalDate: number;
  modSeq: number;
}

/** 最小 RFC822 头解析（演示规范化；附件/多段 MIME 解析留 TODO） */
export function parseRawMessage(raw: RawMessage): ParsedEnvelope {
  const text = new TextDecoder().decode(raw.mime);
  const sep = text.indexOf("\r\n\r\n");
  const head = text.slice(0, sep);
  const body = text.slice(sep + 4);
  const get = (name: string): string => {
    const line = head.split("\r\n").find((l) => l.startsWith(name + ":"));
    return line ? line.slice(name.length + 1).trim() : "";
  };
  const modSeq = Number(get("X-ModSeq") || "0");
  const dateRaw = get("Date");
  const internalDate = dateRaw
    ? (new Date(dateRaw).getTime() || (raw.internalDate ?? Date.now()))
    : (raw.internalDate ?? Date.now());
  return {
    messageId: get("Message-ID").replace(/[<>]/g, ""),
    from: get("From"),
    to: get("To")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean),
    subject: get("Subject"),
    bodyText: body,
    internalDate,
    modSeq,
  };
}

export class IMAPConnector implements Connector {
  readonly providerType = "imap" as const;
  readonly capabilities = CAPS;

  /** 演示用：直接持有内存服务器；真实环境 connect() 时按 host:port 建连 */
  private server?: InMemoryMailServer;
  constructor(server?: InMemoryMailServer) {
    this.server = server;
  }

  async connect(account: AccountCredential): Promise<Session> {
    if (!this.server) {
      // 真实路径：this.server = await dialImap(account.host, account.port, account.username, account.secret)
      throw new Error("IMAPConnector: no upstream server (demo requires injected InMemoryMailServer)");
    }
    return {
      accountId: account.accountId,
      providerType: "imap",
      establishedAt: Date.now(),
      ctx: new Map([["server", this.server]]),
    };
  }

  private s(session: Session): InMemoryMailServer {
    const srv = session.ctx.get("server") as InMemoryMailServer | undefined;
    if (!srv) throw new Error("IMAPConnector: session has no upstream");
    return srv;
  }

  async listFolders(_session: Session): Promise<Folder[]> {
    return [{ name: "INBOX" as FolderName, path: "INBOX", messageCount: this.s(_session).all().length, unreadCount: 0 }];
  }

  async fetchChanges(session: Session, _folder: string, cursor: SyncCursor): Promise<ChangeSet> {
    const since = cursor.highestModSeq ? Number(cursor.highestModSeq) : 0;
    return this.s(session).fetchChangesSince(since);
  }

  async fetchMessage(session: Session, _folder: string, uid: string): Promise<RawMessage> {
    const m = this.s(session).get(uid);
    if (!m) throw new Error(`IMAPConnector: uid not found: ${uid}`);
    const messageId = `${m.uid}@demo.imap`;
    return {
      uid: m.uid,
      mime: buildMime(m, messageId),
      sizeBytes: m.bodyText.length,
      flags: m.flags,
      internalDate: m.internalDate,
    };
  }

  async subscribe(session: Session, folder: string, hook: PushHook): Promise<Subscription> {
    if (!this.capabilities.push) throw new NotImplementedErr("subscribe");
    return this.s(session).subscribe(hook);
  }

  async send(_account: AccountCredential, _message: OutboundMessage): Promise<SendResult> {
    // 真实环境：IMAP 本身不发送，需经 SMTP / Graph sendMail / Gmail send
    throw new NotImplementedErr("send (use SMTP/Graph in production)");
  }

  async healthCheck(session: Session): Promise<Health> {
    const t0 = Date.now();
    const ok = !!this.s(session);
    return { ok, latencyMs: Date.now() - t0, detail: "in-memory upstream" };
  }
}
