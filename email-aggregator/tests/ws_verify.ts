/**
 * WS 实时推送验证（独立端口，避免占用 main 服务的 8080）。
 * 真实 HTTP upgrade + WsNotifier 帧编解码 + 内置 WebSocket 客户端。
 * 运行：node --experimental-strip-types tests/ws_verify.ts
 */

import { buildApp, runScenario } from "../src/app.ts";
import { loadConfig } from "../src/config.ts";

const PORT = 8082;
const ACC = "acc_ws";

async function main(): Promise<void> {
  const app = buildApp(loadConfig({ httpPort: PORT }));
  await runScenario(app, ACC); // 初始全量 + 1 封注入
  await app.api.listen();
  console.log(`WS 验证服务已监听 :${PORT}`);

  const ws = new WebSocket(`ws://localhost:${PORT}/`);
  let received: unknown = null;

  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("WS 超时：3s 内未收到推送")), 3000);
    ws.addEventListener("open", () => {
      ws.send(JSON.stringify({ type: "subscribe", accountId: ACC }));
      // 订阅后模拟上游新邮件到达（触发 IMAP IDLE 推送链）
      setTimeout(() => {
        app.mailServer.inject({
          from: "alert@corp.com",
          to: ["me@aggregator.dev"],
          subject: "WS push test",
          bodyText: "realtime notification",
          internalDate: Date.now(),
          flags: { seen: false, flagged: false, answered: false, deleted: false },
        });
      }, 120);
    });
    ws.addEventListener("message", (ev) => {
      try {
        const p = JSON.parse((ev as MessageEvent).data as string);
        if (p.kind === "new-mail") {
          received = p;
          clearTimeout(timer);
          ws.close();
          resolve();
        }
      } catch {
        /* ignore */
      }
    });
    ws.addEventListener("error", () => reject(new Error("WS 连接错误")));
  });

  const ok = !!received && (received as { kind: string }).kind === "new-mail";
  console.log("WS_PUSH_RECEIVED:", JSON.stringify(received));
  console.log(ok ? "WS_VERIFY: PASS ✅" : "WS_VERIFY: FAIL ❌");
  app.api.close();
  process.exit(ok ? 0 : 1);
}

main().catch((e) => {
  console.error("WS_VERIFY_ERROR", e);
  process.exit(1);
});
