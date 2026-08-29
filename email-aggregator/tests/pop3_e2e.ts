/**
 * TS POP3 连接器 · 端到端验证（进程内 TCP POP3 服务端，纯 node:net）
 *
 * 运行：node --experimental-strip-types tests/pop3_e2e.ts
 * 覆盖：USER/PASS 鉴权、UIDL 高水位增量（uidlWatermark）、RETR 全文拉取与 RFC 822 解析、
 *       多文件夹/推送/发送的能力声明（POP3 不支持 → NotImplemented）。
 */

import net from "node:net";
import { POP3Connector } from "../src/connector/pop3.ts";
import type { AccountCredential, Session } from "../src/model/connector.ts";

// ── 进程内 mock POP3 服务端 ──────────────────────────────────────────────

function rfc822(from: string, to: string, subject: string, body: string, date: string): string {
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

const MAILS = [
  rfc822("alice@example.com", "bob@example.com", "TS POP3 第一封", "TS POP3 正文一（真实链路）", "Fri, 17 Jul 2026 10:00:00 +0000"),
  rfc822("bob@example.com", "carol@example.com", "ts pop3 second", "second body line", "Sat, 18 Jul 2026 09:00:00 +0000"),
];
const UIDLS = ["<ts-pop3-1@demo>", "<ts-pop3-2@demo>"];

function startPop3Server(): Promise<{ addr: { host: string; port: number }; stop: () => void }> {
  return new Promise((resolve, reject) => {
    const server = net.createServer((sock) => {
      let authed = false;
      sock.setEncoding("utf8");
      let lineBuf = "";
      const write = (s: string) => sock.write(s + "\r\n");
      write("+OK mock TS POP3 ready");
      sock.on("data", (d: string) => {
        lineBuf += d;
        for (;;) {
          const nl = lineBuf.indexOf("\r\n");
          if (nl < 0) break;
          const line = lineBuf.slice(0, nl);
          lineBuf = lineBuf.slice(nl + 2);
          const parts = line.split(" ");
          const cmd = (parts[0] ?? "").toUpperCase();
          switch (cmd) {
            case "USER":
              write("+OK user accepted");
              break;
            case "PASS":
              authed = true;
              write("+OK logged in");
              break;
            case "UIDL":
              write("+OK 2 messages");
              write("1 " + UIDLS[0]);
              write("2 " + UIDLS[1]);
              write(".");
              break;
            case "RETR": {
              const idx = Number(parts[1]) - 1;
              const msg = MAILS[idx] ?? MAILS[0];
              write("+OK " + Buffer.byteLength(msg) + " octets");
              for (const l of msg.split("\r\n")) write(l.startsWith(".") ? "." + l : l);
              write(".");
              break;
            }
            case "QUIT":
              write("+OK bye");
              sock.end();
              return;
            default:
              write(authed ? "-ERR unknown command " + cmd : "-ERR not authenticated");
          }
        }
      });
    });
    server.on("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address() as net.AddressInfo;
      resolve({
        addr: { host: "127.0.0.1", port: addr.port },
        stop: () => server.close(),
      });
    });
  });
}

// ── 断言辅助 ──────────────────────────────────────────────────────────────

let pass = 0;
let fail = 0;
function check(name: string, ok: boolean, detail = ""): void {
  if (ok) {
    pass++;
    console.log(`PASS  ${name}${detail ? " — " + detail : ""}`);
  } else {
    fail++;
    console.log(`FAIL  ${name}${detail ? " — " + detail : ""}`);
  }
}

// ── 主流程 ────────────────────────────────────────────────────────────────

async function main(): Promise<void> {
  console.log("=== TS POP3 连接器 · 端到端验证 ===");
  const srv = await startPop3Server();

  const account: AccountCredential = {
    accountId: "acc_pop3",
    providerType: "pop3",
    authMethod: "password",
    username: "alice@example.com",
    secret: "pw",
    host: srv.addr.host,
    port: srv.addr.port,
  };
  const conn = new POP3Connector();

  // 能力声明
  const cap = conn.capabilities;
  check("能力声明：增量=true 推送=false 文件夹=false", cap.incremental && !cap.push && !cap.folders && !cap.flags);

  // connect
  let session: Session;
  try {
    session = await conn.connect(account);
    check("connect + USER/PASS 鉴权", true);
  } catch (e) {
    check("connect + USER/PASS 鉴权", false, (e as Error).message);
    process.exit(1);
  }

  // health
  const h = await conn.healthCheck(session);
  check("healthCheck ok", h.ok);

  // listFolders（POP3 单 INBOX，按 UIDL 计数）
  const folders = await conn.listFolders(session);
  check("listFolders → INBOX count=2", folders.length === 1 && folders[0].name === "INBOX" && folders[0].messageCount === 2);

  // fetchChanges 全量（空游标）
  const cs1 = await conn.fetchChanges(session, "INBOX", {});
  check("fetchChanges 全量 added=2", cs1.added.length === 2 && cs1.added[0] === UIDLS[0] && cs1.added[1] === UIDLS[1]);
  check("fetchChanges nextCursor.uidlWatermark", cs1.nextCursor.uidlWatermark === UIDLS[1], cs1.nextCursor.uidlWatermark);

  // fetchMessage + RFC 822 解析
  const raw1 = await conn.fetchMessage(session, "INBOX", UIDLS[0]);
  const text1 = new TextDecoder().decode(raw1.mime);
  check("fetchMessage 拉取全文", text1.includes("TS POP3 正文一"));
  check("fetchMessage internalDate 解析", raw1.internalDate && raw1.internalDate > 0, String(raw1.internalDate));

  // 增量：高水位=第一封 → 仅新增第二封
  const cs2 = await conn.fetchChanges(session, "INBOX", { uidlWatermark: UIDLS[0] });
  check("fetchChanges 增量（水位后）added=1", cs2.added.length === 1 && cs2.added[0] === UIDLS[1]);

  // 不支持项
  let notImpl = 0;
  try {
    await conn.subscribe(session, "INBOX", { onExists: () => {} });
  } catch {
    notImpl++;
  }
  try {
    await conn.send(account, { from: "a@b.c", to: ["x@y.z"], subject: "s", bodyText: "b" });
  } catch {
    notImpl++;
  }
  check("subscribe/send 抛 NotImplemented（POP3 不支持）", notImpl === 2);

  srv.stop();
  console.log(`\n=== 结果：PASS ${pass} / FAIL ${fail} ===`);
  process.exit(fail > 0 ? 1 : 0);
}

main().catch((e) => {
  console.error("FATAL:", e);
  process.exit(1);
});
