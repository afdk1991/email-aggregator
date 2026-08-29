/**
 * 可交互服务入口：装配并运行端到端场景（预置种子数据），
 * 随后常驻，对外提供 REST + WebSocket（同端口，默认 8080）。
 *
 * 运行：node --experimental-strip-types src/main.ts
 * 自测：node --experimental-strip-types src/demo.ts
 */

import { buildApp, runScenario } from "./app.ts";
import { loadConfig } from "./config.ts";

async function main(): Promise<void> {
  const app = buildApp(loadConfig());
  const r = await runScenario(app, "acc_demo");

  console.log("=== 邮箱聚合平台 Phase 0 PoC（端到端场景）===");
  console.log(`初始全量同步邮件数 : ${r.initialCount}`);
  console.log(`检索 'Welcome' 命中 : ${r.searchHits}`);
  console.log(`注入新邮件后总数   : ${r.afterInjectCount}`);
  console.log(`new-mail 推送次数  : ${r.newMailNotifications}`);
  console.log("---------------------------------------------------");
  console.log(`REST  : http://localhost:${app.config.httpPort}/api/health`);
  console.log(`列表  : GET /api/mails?accountId=acc_demo&folder=INBOX&limit=20`);
  console.log(`检索  : GET /api/search?accountId=acc_demo&q=Welcome`);
  console.log(`WS    : ws://localhost:${app.config.httpPort}/  (subscribe {"type":"subscribe","accountId":"acc_demo"})`);
  console.log("服务常驻中（Ctrl+C 退出）...");

  await app.api.listen();
}

main().catch((e) => {
  console.error("FATAL", e);
  process.exit(1);
});
