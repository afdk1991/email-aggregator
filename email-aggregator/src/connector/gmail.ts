/**
 * Gmail 连接器（Gmail API REST + OAuth2 Bearer，Node fetch 零依赖）。
 *
 * 与 Go RealGmailConnector 对齐（架构 §10/§12：Gmail 走官方 API，非 IMAP 复用）：
 *  - connect：Bearer 令牌 + profile 探测（401 快速失败）
 *  - fetchChanges：history.list(startHistoryId) → added/deleted，游标 = SyncCursor.historyId
 *  - fetchMessage：messages.get(format=raw) 拉取 base64url RFC822 并解析
 *  - listFolders：POP3/Gmail 语义为单一 INBOX，count 取 messages.list 的 resultSizeEstimate
 *
 * 能力声明：增量 = true（historyId）、推送 = false（Cloud Pub/Sub 真实环境才有，PoC 轮询不实现）、
 * 多文件夹 = false、旗帜 = false。
 */

import {
  type AccountCredential,
  type ChangeSet,
  type Connector,
  type ConnectorCapabilities,
  type Folder,
  type Health,
  type OutboundMessage,
  type PushHook,
  type RawMessage,
  type SendResult,
  type Session,
  type Subscription,
  NotImplemented as NotImplementedErr,
} from "../model/connector.ts";
import type { FolderName, SyncCursor } from "../model/canonical.ts";
import { parseRawMessage } from "./imap.ts";

const CAPS: ConnectorCapabilities = {
  incremental: true,
  push: false,
  folders: false,
  flags: false,
  maxPageSize: 50,
};

const DEFAULT_BASE = "https://gmail.googleapis.com/gmail/v1";

interface GmailHistoryRecord {
  id: number;
  added: string[];
  deleted: string[];
}

/** 轻量 Gmail API 客户端（Bearer + JSON 解析 + 非 2xx 报错） */
export class GmailApiClient {
  readonly baseURL: string;
  readonly token: string;
  readonly user: string;

  constructor(baseURL: string, token: string, user: string) {
    this.baseURL = baseURL.replace(/\/+$/, "");
    this.token = token;
    this.user = user;
  }

  async get<T>(path: string, query = ""): Promise<T> {
    const url = this.baseURL + path + (query ? "?" + query : "");
    const resp = await fetch(url, {
      headers: { Authorization: "Bearer " + this.token },
    });
    const body = await resp.text();
    if (!resp.ok) {
      throw new Error(`gmail GET ${path}: status=${resp.status} body=${body.slice(0, 160)}`);
    }
    return JSON.parse(body) as T;
  }

  async profile(): Promise<{ emailAddress: string; historyId: string }> {
    return this.get(`/users/${this.user}/profile`);
  }

  async listAll(): Promise<string[]> {
    const out: string[] = [];
    let token = "";
    for (;;) {
      const q = `maxResults=50${token ? "&pageToken=" + token : ""}`;
      const l = await this.get<{ messages?: { id: string; threadId: string }[]; nextPageToken?: string }>(
        `/users/${this.user}/messages`,
        q,
      );
      for (const m of l.messages ?? []) out.push(m.id);
      if (!l.nextPageToken) return out;
      token = l.nextPageToken;
    }
  }

  /** resultSizeEstimate（列表总数估计，供 listFolders 计数） */
  async countEstimate(): Promise<number> {
    const l = await this.get<{ resultSizeEstimate?: number }>(`/users/${this.user}/messages`, "maxResults=1");
    return l.resultSizeEstimate ?? 0;
  }

  async getRaw(id: string): Promise<{ raw: Uint8Array; size: number; internalDate?: number }> {
    const m = await this.get<{ id: string; internalDate?: number; sizeEstimate?: number; raw?: string }>(
      `/users/${this.user}/messages/${id}`,
      "format=raw",
    );
    if (!m.raw) throw new Error(`gmail: message ${id} has no raw payload`);
    const b64 = m.raw.replace(/-/g, "+").replace(/_/g, "/");
    const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
    const bin = atob(padded);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    return { raw: bytes, size: m.sizeEstimate ?? bytes.length, internalDate: m.internalDate };
  }

  async history(start: number): Promise<{ records: GmailHistoryRecord[]; latest: string }> {
    const h = await this.get<{
      history?: {
        id: number;
        messagesAdded?: { message: { id: string } }[];
        messagesDeleted?: { message: { id: string } }[];
      }[];
      historyId?: string;
    }>(`/users/${this.user}/history`, `startHistoryId=${start}`);
    const records: GmailHistoryRecord[] = (h.history ?? []).map((r) => ({
      id: r.id,
      added: (r.messagesAdded ?? []).map((a) => a.message.id),
      deleted: (r.messagesDeleted ?? []).map((d) => d.message.id),
    }));
    return { records, latest: h.historyId ?? String(start) };
  }
}

/** Gmail 连接器（Gmail API REST） */
export class GmailConnector implements Connector {
  readonly providerType = "gmail" as const;
  readonly capabilities: ConnectorCapabilities = CAPS;

  private client(session: Session): GmailApiClient {
    const c = session.ctx.get("client") as GmailApiClient | undefined;
    if (!c) throw new Error("GmailConnector: session not connected");
    return c;
  }

  async connect(account: AccountCredential): Promise<Session> {
    const token = account.accessToken;
    if (!token) throw new Error("GmailConnector: oauth2 access token required");
    const baseURL = (account.options?.["endpoint"] as string | undefined) ?? DEFAULT_BASE;
    const user = account.username ?? "me";

    const client = new GmailApiClient(baseURL, token, user);
    await client.profile(); // 401 快速失败
    return {
      accountId: account.accountId,
      providerType: "gmail",
      establishedAt: Date.now(),
      ctx: new Map([["client", client]]),
    };
  }

  async listFolders(session: Session): Promise<Folder[]> {
    const count = await this.client(session).countEstimate();
    return [{ name: "INBOX" as FolderName, path: "INBOX", messageCount: count, unreadCount: 0 }];
  }

  async fetchChanges(session: Session, _folder: string, cursor: SyncCursor): Promise<ChangeSet> {
    const client = this.client(session);
    const start = cursor.historyId ? Number(cursor.historyId) : 0;
    const { records, latest } = await client.history(start);
    const added: string[] = [];
    const deleted: string[] = [];
    for (const r of records) {
      added.push(...r.added);
      deleted.push(...r.deleted);
    }
    return { added, changed: [], deleted, nextCursor: { historyId: latest } };
  }

  async fetchMessage(session: Session, _folder: string, uid: string): Promise<RawMessage> {
    const client = this.client(session);
    const { raw, size, internalDate } = await client.getRaw(uid);
    const parsed = parseRawMessage({ uid, mime: raw, sizeBytes: size });
    return {
      uid,
      mime: raw,
      sizeBytes: size,
      flags: {},
      internalDate: internalDate ?? parsed.internalDate,
    };
  }

  async subscribe(_session: Session, _folder: string, _hook: PushHook): Promise<Subscription> {
    // Gmail 官方推送走 Cloud Pub/Sub（真实环境）；PoC 不实现 → 明确抛错而非假装支持
    throw new NotImplementedErr("subscribe (Gmail uses Cloud Pub/Sub; poll fetchChanges in PoC)");
  }

  async send(_account: AccountCredential, _message: OutboundMessage): Promise<SendResult> {
    // Gmail API 发送为 POST messages/send（真实环境经 OAuth）；PoC 不实现
    throw new NotImplementedErr("send (use Gmail API messages.send in production)");
  }

  async healthCheck(session: Session): Promise<Health> {
    const t0 = Date.now();
    const ok = !!session.ctx.get("client");
    return { ok, latencyMs: Date.now() - t0, detail: ok ? "gmail connected" : "gmail not connected" };
  }
}
