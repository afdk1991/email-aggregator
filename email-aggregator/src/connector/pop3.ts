/**
 * POP3 连接器（RFC 1939，纯 node:net 实现，零第三方依赖）。
 *
 * 与 Go 侧 `RealPOP3Connector` 对齐：USER/PASS 鉴权、UIDL 高水位增量、RETR 拉取全文并解析 RFC 822。
 * 契约差异点（TS Connector 接口）：
 *  - fetchChanges 返回 UIDL 变更集，游标用 SyncCursor.uidlWatermark（契约已为 POP3 预留）。
 *  - fetchMessage 的 uid 实为 UIDL；内部以「seq ↔ UIDL」映射定位 RETR 序号。
 *
 * 能力声明：
 *  - 增量 = true（UIDL 近似游标；正确性由摄取层 UIDL 幂等去重兜底）
 *  - 推送 = false（POP3 无 IDLE/推送，需轮询 fetchChanges）
 *  - 多文件夹 / 旗帜 = false（POP3 仅单一 INBOX，无 FLAGS）
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
import net from "node:net";
import { parseRawMessage } from "./imap.ts";

const CAPS: ConnectorCapabilities = {
  incremental: true,
  push: false,
  folders: false,
  flags: false,
  maxPageSize: 50,
};

/** RFC 1939 最小 POP3 客户端（行协议，超时保护，单写队列）。
 *  注意：服务器可能在一个 TCP 段里突发多行（如 UIDL 全量），因此解析出的行先入缓冲队列，
 *  readLine 时按需派发，避免「顺序 await 只有一个 waiter 排队」导致突发行被丢弃。 */
class Pop3Client {
  private sock: net.Socket;
  private buf = Buffer.alloc(0);
  private lines: string[] = [];
  private waiters: Array<(line: string | null) => void> = [];

  constructor(sock: net.Socket) {
    this.sock = sock;
    sock.on("data", (d: Buffer) => {
      this.buf = Buffer.concat([this.buf, d]);
      this.drain();
    });
    sock.on("close", () => this.flush(null));
    sock.on("error", () => this.flush(null));
  }

  private drain(): void {
    for (;;) {
      const idx = this.buf.indexOf("\r\n");
      if (idx < 0) break;
      const line = this.buf.subarray(0, idx).toString("utf8");
      this.buf = this.buf.subarray(idx + 2);
      this.lines.push(line);
      this.pump();
    }
  }

  private pump(): void {
    while (this.lines.length > 0 && this.waiters.length > 0) {
      const w = this.waiters.shift()!;
      w(this.lines.shift()!);
    }
  }

  private flush(v: string | null): void {
    while (this.waiters.length > 0) {
      const w = this.waiters.shift()!;
      w(v);
    }
  }

  readLine(): Promise<string> {
    return new Promise((resolve, reject) => {
      if (this.lines.length > 0) {
        resolve(this.lines.shift()!);
        return;
      }
      const timer = setTimeout(() => {
        this.waiters = this.waiters.filter((w) => w !== wrapped);
        reject(new Error("pop3 read timeout"));
      }, 30000);
      const wrapped = (line: string | null) => {
        clearTimeout(timer);
        if (line === null) reject(new Error("pop3 connection closed"));
        else resolve(line);
      };
      this.waiters.push(wrapped);
    });
  }

  write(s: string): Promise<void> {
    return new Promise((resolve, reject) => {
      this.sock.write(s + "\r\n", (err) => (err ? reject(err) : resolve()));
    });
  }

  /** 多行响应：首行已读取 + 逐行直到 "."（含点填充还原） */
  async readMulti(): Promise<{ first: string; lines: string[] }> {
    const first = await this.readLine();
    if (!first.startsWith("+OK")) throw new Error(`pop3 command failed: ${first}`);
    const lines: string[] = [];
    for (;;) {
      const line = await this.readLine();
      if (line === ".") break;
      lines.push(line.startsWith("..") ? line.slice(1) : line);
    }
    return { first, lines };
  }

  async uidl(): Promise<Map<number, string>> {
    await this.write("UIDL");
    const { lines } = await this.readMulti();
    const out = new Map<number, string>();
    for (const l of lines) {
      const parts = l.trim().split(/\s+/);
      if (parts.length >= 2 && /^\d+$/.test(parts[0])) out.set(Number(parts[0]), parts[1]);
    }
    return out;
  }

  async retr(seq: number): Promise<Uint8Array> {
    await this.write(`RETR ${seq}`);
    const first = await this.readLine();
    if (!first.startsWith("+OK")) throw new Error(`pop3 RETR ${seq}: ${first}`);
    const chunks: string[] = [];
    for (;;) {
      const line = await this.readLine();
      if (line === ".") break;
      chunks.push(line.startsWith("..") ? line.slice(1) : line);
    }
    return new TextEncoder().encode(chunks.join("\r\n") + "\r\n");
  }

  async quit(): Promise<void> {
    try {
      await this.write("QUIT");
    } catch {
      /* 忽略 */
    }
    this.sock.destroy();
  }

  get connected(): boolean {
    return !this.sock.destroyed;
  }
}

/** POP3 连接器（RFC 1939） */
export class POP3Connector implements Connector {
  readonly providerType = "pop3" as const;
  readonly capabilities: ConnectorCapabilities = CAPS;

  async connect(account: AccountCredential): Promise<Session> {
    if (!account.host || !account.port) {
      throw new Error("POP3Connector: account requires host/port");
    }
    const sock = net.connect({ host: account.host, port: account.port });
    await new Promise<void>((resolve, reject) => {
      sock.once("connect", resolve);
      sock.once("error", reject);
    });
    const client = new Pop3Client(sock);
    const greeting = await client.readLine();
    if (!greeting.startsWith("+OK")) {
      sock.destroy();
      throw new Error(`pop3 server greeting: ${greeting}`);
    }
    await client.write(`USER ${account.username ?? ""}`);
    const u = await client.readLine();
    if (!u.startsWith("+OK")) throw new Error(`pop3 USER rejected: ${u}`);
    await client.write(`PASS ${account.secret ?? ""}`);
    const p = await client.readLine();
    if (!p.startsWith("+OK")) throw new Error(`pop3 PASS rejected: ${p}`);

    return {
      accountId: account.accountId,
      providerType: "pop3",
      establishedAt: Date.now(),
      ctx: new Map([["client", client]]),
    };
  }

  private c(session: Session): Pop3Client {
    const c = session.ctx.get("client") as Pop3Client | undefined;
    if (!c || !c.connected) throw new Error("POP3Connector: session not connected");
    return c;
  }

  async listFolders(session: Session): Promise<Folder[]> {
    // POP3 无多文件夹概念；以 UIDL 统计 INBOX 消息数
    const uidls = await this.c(session).uidl();
    return [{ name: "INBOX" as FolderName, path: "INBOX", messageCount: uidls.size, unreadCount: 0 }];
  }

  async fetchChanges(session: Session, _folder: string, cursor: SyncCursor): Promise<ChangeSet> {
    const client = this.c(session);
    const uidls = await client.uidl();
    const wm = cursor.uidlWatermark ?? "";
    const added: string[] = [];
    let maxWm = wm;
    // 保存 seq↔uidl 映射，供 fetchMessage 定位 RETR 序号
    const seqByUidl = new Map<string, number>();
    const uidlBySeq = new Map<number, string>();
    for (const [seq, uidl] of uidls) {
      seqByUidl.set(uidl, seq);
      uidlBySeq.set(seq, uidl);
      if (wm !== "" && uidl <= wm) continue;
      added.push(uidl);
      if (uidl > maxWm) maxWm = uidl;
    }
    session.ctx.set("seqByUidl", seqByUidl);
    session.ctx.set("uidlBySeq", uidlBySeq);
    return {
      added,
      changed: [],
      deleted: [], // POP3 无可靠的删除检测；完整比对留 TODO
      nextCursor: { uidlWatermark: maxWm },
    };
  }

  async fetchMessage(session: Session, _folder: string, uid: string): Promise<RawMessage> {
    const client = this.c(session);
    const seqByUidl = session.ctx.get("seqByUidl") as Map<string, number> | undefined;
    const seq = seqByUidl?.get(uid);
    if (!seq) throw new Error(`POP3Connector: unknown uid (call fetchChanges first): ${uid}`);
    const mime = await client.retr(seq);
    const parsed = parseRawMessage({ uid, mime, sizeBytes: mime.length });
    return {
      uid,
      mime,
      sizeBytes: mime.length,
      flags: {},
      internalDate: parsed.internalDate,
    };
  }

  async subscribe(_session: Session, _folder: string, _hook: PushHook): Promise<Subscription> {
    // POP3 无推送（capabilities.push = false）
    throw new NotImplementedErr("subscribe (POP3 has no push; poll fetchChanges)");
  }

  async send(_account: AccountCredential, _message: OutboundMessage): Promise<SendResult> {
    // POP3 不能发送；真实环境经 SMTP / Graph / Gmail send
    throw new NotImplementedErr("send (use SMTP/Graph in production)");
  }

  async healthCheck(session: Session): Promise<Health> {
    const t0 = Date.now();
    const client = session.ctx.get("client") as Pop3Client | undefined;
    const ok = !!client && client.connected;
    return { ok, latencyMs: Date.now() - t0, detail: ok ? "pop3 connected" : "pop3 not connected" };
  }
}
