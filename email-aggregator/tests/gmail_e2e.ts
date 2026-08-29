/**
 * TS Gmail 连接器 · 端到端验证（进程内 mock Gmail API，node:http 零依赖）
 *
 * 运行：node --experimental-strip-types tests/gmail_e2e.ts
 * 覆盖：Bearer 鉴权、profile 探测、分页全量、historyId 增量（无重复）、删除 OnDelete 语义、
 *       format=raw 拉取与 RFC 822 解析、多文件夹/推送/发送的能力声明（NotImplemented）。
 */

import http from "node:http";
import { GmailConnector } from "../src/connector/gmail.ts";
import type { AccountCredential, Session } from "../src/model/connector.ts";

// ── 进程内 mock Gmail API 服务端 ──────────────────────────────────────────

function rfc822(subject: string, from: string, to: string, body: string, date: string): string {
  return [
    `From: ${from}`,
    `To: ${to}`,
    `Subject: ${subject}`,
    `Date: ${date}`,
    "Message-ID: <1@demo>",
    "Content-Type: text/plain",
    "",
    body,
  ].join("\r\n");
}

const b64url = (s: string) =>
  Buffer.from(s, "utf8").toString("base64url");

interface MockMail {
  id: string;
  raw: string;
  internalDate: number;
}
interface MockHist {
  id: number;
  added: string[];
  deleted: string[];
}

class MockGmailServer {
  private srv: http.Server;
  private mails: Map<string, MockMail> = new Map();
  private order: string[] = [];
  private hist: MockHist[] = [];
  private nextMsg = 0;
  private curHist = 0;
  token = "";
  pageSize = 2;
  base = "";

  constructor() {
    this.srv = http.createServer((req, res) => {
      const url = new URL(req.url ?? "/", "http://127.0.0.1");
      const base = "/gmail/v1/users/";
      if (!url.pathname.startsWith(base)) {
        res.writeHead(404); res.end(); return;
      }
      if (this.token && req.headers.authorization !== "Bearer " + this.token) {
        res.writeHead(401, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: { code: 401, message: "Invalid Credentials" } }));
        return;
      }
      const rest = url.pathname.slice(base.length).split("/").filter(Boolean); // [user, sub, ...]
      const sub = rest[1];
      const json = (v: unknown) => { res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify(v)); };

      if (sub === "profile") {
        json({ emailAddress: rest[0], historyId: String(this.curHist) });
      } else if (sub === "messages" && rest.length === 2) {
        const start = Number(url.searchParams.get("pageToken") ?? "0");
        const max = Number(url.searchParams.get("maxResults") ?? this.pageSize);
        const items = this.order.slice(start, start + max).map((id) => ({ id, threadId: id }));
        const out: Record<string, unknown> = { messages: items, resultSizeEstimate: this.order.length };
        if (start + max < this.order.length) out.nextPageToken = String(start + max);
        json(out);
      } else if (sub === "messages" && rest.length === 3) {
        const m = this.mails.get(rest[2]);
        if (!m) { res.writeHead(404); res.end(JSON.stringify({ error: "Message not found" })); return; }
        json({ id: m.id, threadId: m.id, internalDate: m.internalDate, sizeEstimate: m.raw.length, raw: b64url(m.raw) });
      } else if (sub === "history") {
        const start = Number(url.searchParams.get("startHistoryId") ?? "0");
        const recs = this.hist.filter((h) => h.id > start).map((h) => {
          const rec: Record<string, unknown> = { id: h.id };
          if (h.added.length) rec.messagesAdded = h.added.map((id) => ({ message: { id, threadId: id } }));
          if (h.deleted.length) rec.messagesDeleted = h.deleted.map((id) => ({ message: { id } }));
          return rec;
        });
        json({ history: recs, historyId: String(this.curHist) });
      } else {
        res.writeHead(404); res.end();
      }
    });
  }

  async start(): Promise<string> {
    await new Promise<void>((resolve) => this.srv.listen(0, "127.0.0.1", resolve));
    const addr = this.srv.address() as { port: number };
    this.base = `http://127.0.0.1:${addr.port}/gmail/v1`;
    return this.base;
  }

  stop(): void { this.srv.close(); }

  inject(raw: string, internalDate: number): string {
    this.nextMsg++;
    const id = "gm-" + this.nextMsg;
    this.curHist++;
    this.mails.set(id, { id, raw, internalDate });
    this.order.push(id);
    this.hist.push({ id: this.curHist, added: [id], deleted: [] });
    return id;
  }

  delete(id: string): void {
    if (!this.mails.has(id)) return;
    this.mails.delete(id);
    this.curHist++;
    this.hist.push({ id: this.curHist, added: [], deleted: [id] });
  }

  count(): number { return this.mails.size; }
  latestHistory(): number { return this.curHist; }
}

// ── 断言辅助 ──────────────────────────────────────────────────────────────

let pass = 0;
let fail = 0;
function check(name: string, ok: boolean, detail = ""): void {
  if (ok) { pass++; console.log(`PASS  ${name}${detail ? " — " + detail : ""}`); }
  else { fail++; console.log(`FAIL  ${name}${detail ? " — " + detail : ""}`); }
}

// ── 主流程 ────────────────────────────────────────────────────────────────

async function main(): Promise<void> {
  console.log("=== TS Gmail 连接器 · 端到端验证 ===");
  const srv = new MockGmailServer();
  await srv.start();
  srv.pageSize = 2; // 注入 3 封 → 跨 2 页
  srv.token = "tok-1"; // 开启 Bearer 校验（错误令牌 → 401）

  const d1 = Date.parse("Fri, 17 Jul 2026 10:00:00 +0000");
  const d2 = Date.parse("Sat, 18 Jul 2026 09:00:00 +0000");
  const d3 = Date.parse("Sun, 19 Jul 2026 08:00:00 +0000");
  const id1 = srv.inject(rfc822("TS Gmail 第一封", "alice@example.com", "bob@example.com", "TS Gmail 正文一", new Date(d1).toUTCString()), d1);
  const id2 = srv.inject(rfc822("ts gmail second", "bob@example.com", "carol@example.com", "second body", new Date(d2).toUTCString()), d2);
  const id3 = srv.inject(rfc822("ts gmail third", "carol@example.com", "alice@example.com", "third body", new Date(d3).toUTCString()), d3);

  const account: AccountCredential = {
    accountId: "acc_gmail",
    providerType: "gmail",
    authMethod: "oauth2",
    username: "me@example.com",
    accessToken: "tok-1",
    options: { endpoint: srv.base },
  };
  const conn = new GmailConnector();

  check("能力声明：增量=true 推送/文件夹/旗帜=false", conn.capabilities.incremental && !conn.capabilities.push && !conn.capabilities.folders && !conn.capabilities.flags);

  let session: Session;
  try {
    session = await conn.connect(account);
    check("connect + Bearer profile 探测", true);
  } catch (e) {
    check("connect + Bearer profile 探测", false, (e as Error).message);
    process.exit(1);
  }

  // 鉴权失败：错 token
  const badAcc: AccountCredential = { ...account, accessToken: "wrong" };
  let authFailed = false;
  try { await conn.connect(badAcc); } catch { authFailed = true; }
  check("错误 Bearer 令牌 → connect 401 失败", authFailed);

  check("healthCheck ok", (await conn.healthCheck(session)).ok);

  // listFolders：resultSizeEstimate
  const folders = await conn.listFolders(session);
  check("listFolders → INBOX count=3", folders.length === 1 && folders[0].name === "INBOX" && folders[0].messageCount === 3);

  // fetchChanges 全量（historyId=0 → 全量 added=3，跨 2 页拉取）
  const cs1 = await conn.fetchChanges(session, "INBOX", {});
  check("fetchChanges 全量 added=3（分页）", cs1.added.length === 3 && cs1.added.includes(id1) && cs1.added.includes(id2) && cs1.added.includes(id3));
  check("fetchChanges nextCursor.historyId", cs1.nextCursor.historyId === String(srv.latestHistory()), cs1.nextCursor.historyId);

  // fetchMessage：format=raw → RFC 822 解析
  const raw1 = await conn.fetchMessage(session, "INBOX", id1);
  const text1 = new TextDecoder().decode(raw1.mime);
  check("fetchMessage 拉取全文", text1.includes("TS Gmail 正文一"));
  check("fetchMessage internalDate 解析", raw1.internalDate === d1, String(raw1.internalDate));

  // 增量：新邮件 → history 增量 1 封；同游标再增量 → 无重复
  const id4 = srv.inject(rfc822("ts gmail four", "meet@example.com", "me@example.com", "fourth body", new Date(Date.parse("Mon, 20 Jul 2026 07:00:00 +0000")).toUTCString()), Date.parse("Mon, 20 Jul 2026 07:00:00 +0000"));
  const cs2 = await conn.fetchChanges(session, "INBOX", { historyId: cs1.nextCursor.historyId });
  check("fetchChanges 增量（historyId 断点）added=1", cs2.added.length === 1 && cs2.added[0] === id4);
  const cs3 = await conn.fetchChanges(session, "INBOX", { historyId: cs2.nextCursor.historyId });
  check("fetchChanges 同游标再增量无重复", cs3.added.length === 0 && cs3.deleted.length === 0);

  // 删除：OnDelete 语义（deleted 列表返回被删 id）
  srv.delete(id1);
  const cs4 = await conn.fetchChanges(session, "INBOX", { historyId: cs3.nextCursor.historyId });
  check("fetchChanges 删除 → deleted=[id1]", cs4.deleted.length === 1 && cs4.deleted[0] === id1);

  // 不支持项
  let notImpl = 0;
  try { await conn.subscribe(session, "INBOX", { onExists: () => {} }); } catch { notImpl++; }
  try { await conn.send(account, { from: "a@b.c", to: ["x@y.z"], subject: "s", bodyText: "b" }); } catch { notImpl++; }
  check("subscribe/send 抛 NotImplemented", notImpl === 2);

  srv.stop();
  console.log(`\n=== 结果：PASS ${pass} / FAIL ${fail} ===`);
  process.exit(fail > 0 ? 1 : 0);
}

main().catch((e) => {
  console.error("FATAL:", e);
  process.exit(1);
});
