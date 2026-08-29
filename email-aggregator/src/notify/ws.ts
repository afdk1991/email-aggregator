/**
 * 通知层：把 notifications 事件推送给订阅了某账户的客户端。
 *  - InMemoryNotifier：演示 / 单测用，push 到注册的回调。
 *  - WsNotifier：真实 WebSocket（仅用 Node 内置 http+crypto，零依赖）实现，
 *    浏览器/前端经 WebSocket 订阅账户即可实时收到新邮件推送。
 *
 * 租户隔离（ADR-009）：sink 与 publish 均以 (tenantId, accountId) 复合键路由，
 * 防止跨租户通知泄漏。
 *
 * 与 Go 实现对齐：email-aggregator-go/src/notify/ws.go（tenantId 首参 + Resolve 收敛）。
 */

import { type IncomingMessage, type Server } from "node:http";
import { createHash } from "node:crypto";
import { Resolve } from "../tenant/tenant.ts";

export type NotifyKind = "new-mail" | "sync-state" | "error";

export interface NotificationPayload {
  kind: NotifyKind;
  tenantId?: string;
  accountId: string;
  preview?: string;
  ts: number;
}

export interface PushSink {
  send(payload: NotificationPayload): void;
  close(): void;
}

export interface Notifier {
  addSink(tenantId: string, accountId: string, sink: PushSink): void;
  removeSink(tenantId: string, accountId: string, sink: PushSink): void;
  publish(tenantId: string, accountId: string, payload: NotificationPayload): void;
}

/** 复合路由键：`${tenantId}|${accountId}` */
function routeKey(tenantId: string, accountId: string): string {
  return `${Resolve(tenantId)}|${accountId}`;
}

export class InMemoryNotifier implements Notifier {
  private sinks = new Map<string, Set<PushSink>>();
  private received: NotificationPayload[] = [];

  addSink(tenantId: string, accountId: string, sink: PushSink): void {
    const k = routeKey(tenantId, accountId);
    const set = this.sinks.get(k) ?? new Set();
    set.add(sink);
    this.sinks.set(k, set);
  }
  removeSink(tenantId: string, accountId: string, sink: PushSink): void {
    this.sinks.get(routeKey(tenantId, accountId))?.delete(sink);
  }
  publish(tenantId: string, accountId: string, payload: NotificationPayload): void {
    const tid = Resolve(tenantId);
    const enriched: NotificationPayload = { ...payload, tenantId: tid };
    this.received.push(enriched);
    this.sinks.get(routeKey(tid, accountId))?.forEach((s) => s.send(enriched));
  }
  /** 演示断言用：已收到的通知列表 */
  getReceived(): NotificationPayload[] {
    return this.received;
  }
}

const WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

/** 极简 WebSocket 服务端（RFC6455，仅依赖内置模块） */
export class WsNotifier implements Notifier {
  private server: Server | null = null;
  private sinks = new Map<string, Set<PushSink>>();
  private sockets = new Set<PushSink>();

  /** 挂载到已有 HTTP 服务，使 REST 与 WS 同端口 */
  attach(server: Server): void {
    this.server = server;
    server.on("upgrade", (req, socket) => this.onUpgrade(req, socket as never));
  }

  private onUpgrade(req: IncomingMessage, socket: { write: (b: Buffer) => void; on: (e: string, cb: (d: Buffer) => void) => void; destroy: () => void }): void {
    const key = req.headers["sec-websocket-key"];
    if (!key) return socket.destroy();
    const accept = createHash("sha1").update(key + WS_GUID).digest("base64");
    socket.write(
      Buffer.from(
        "HTTP/1.1 101 Switching Protocols\r\n" +
          "Upgrade: websocket\r\n" +
          "Connection: Upgrade\r\n" +
          `Sec-WebSocket-Accept: ${accept}\r\n\r\n`,
      ),
    );
    const sink: PushSink = {
      send: (p) => this.sendFrame(socket, JSON.stringify(p)),
      close: () => socket.destroy(),
    };
    this.sockets.add(sink);
    socket.on("data", (buf) => this.onData(buf, sink));
  }

  private onData(buf: Buffer, sink: PushSink): void {
    // 解析单帧文本消息（演示：客户端发送 {type:"subscribe",tenantId,accountId}）
    if (buf.length < 2) return;
    const opcode = buf[0] & 0x0f;
    const masked = (buf[1] & 0x80) !== 0;
    let len = buf[1] & 0x7f;
    let offset = 2;
    if (len === 126) {
      len = buf.readUInt16BE(2);
      offset = 4;
    } else if (len === 127) {
      len = Number(buf.readBigUInt64BE(2));
      offset = 10;
    }
    let maskKey: Buffer | null = null;
    if (masked) {
      maskKey = buf.subarray(offset, offset + 4);
      offset += 4;
    }
    const payload = buf.subarray(offset, offset + len);
    if (maskKey) {
      for (let i = 0; i < payload.length; i++) payload[i] ^= maskKey[i & 3];
    }
    if (opcode === 0x1) {
      try {
        const msg = JSON.parse(payload.toString("utf8"));
        if (msg.type === "subscribe" && msg.accountId) {
          // 兼容旧客户端（无 tenantId）与多租户客户端
          this.addSink(msg.tenantId ?? "", msg.accountId as string, sink);
        }
      } catch {
        /* ignore malformed */
      }
    } else if (opcode === 0x8) {
      this.sockets.delete(sink);
    }
  }

  private sendFrame(socket: { write: (b: Buffer) => void }, text: string): void {
    const data = Buffer.from(text, "utf8");
    const len = data.length;
    let header: Buffer;
    if (len < 126) {
      header = Buffer.from([0x81, len]);
    } else if (len < 65536) {
      header = Buffer.alloc(4);
      header[0] = 0x81;
      header[1] = 126;
      header.writeUInt16BE(len, 2);
    } else {
      header = Buffer.alloc(10);
      header[0] = 0x81;
      header[1] = 127;
      header.writeBigUInt64BE(BigInt(len), 2);
    }
    socket.write(Buffer.concat([header, data]));
  }

  addSink(tenantId: string, accountId: string, sink: PushSink): void {
    const k = routeKey(tenantId, accountId);
    const set = this.sinks.get(k) ?? new Set();
    set.add(sink);
    this.sinks.set(k, set);
  }
  removeSink(tenantId: string, accountId: string, sink: PushSink): void {
    this.sinks.get(routeKey(tenantId, accountId))?.delete(sink);
  }
  publish(tenantId: string, accountId: string, payload: NotificationPayload): void {
    const tid = Resolve(tenantId);
    this.sinks.get(routeKey(tid, accountId))?.forEach((s) => s.send({ ...payload, tenantId: tid }));
  }

  listen(port: number): Promise<void> {
    return new Promise((resolve) => this.server!.listen(port, () => resolve()));
  }
  close(): void {
    this.server?.close();
  }
}
