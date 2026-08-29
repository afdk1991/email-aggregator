/**
 * 无头自检：运行端到端场景并断言关键不变量，退出码反映成败。
 * 用于 CI / 本地验证，无需外部基础设施（内存桩）。
 *
 * ADR-009：演示多租户隔离 —— 以 tenantA 运行，并断言 tenantId 全链路透传。
 */

import { buildApp, runScenario } from "./app.ts";
import { loadConfig } from "./config.ts";
import { type TenantContext, DefaultTenantID } from "./tenant/tenant.ts";

async function main(): Promise<void> {
  // ADR-009：注入租户上下文 tenantA（模拟前置 L7 网关 / 鉴权层注入 X-Tenant-Id）
  const tenantCtx: TenantContext = { tenantId: "tenantA", tier: "enterprise" };
  const app = buildApp(loadConfig(), tenantCtx);
  const accountId = "acc_demo";
  const r = await runScenario(app, accountId);
  const newMail = app.inMemory.getReceived().filter((n) => n.kind === "new-mail");

  const checks: Array<[string, boolean]> = [
    ["初始全量：种子 3 封入库", r.initialCount === 3],
    ["增量：注入 1 封后总数为 4", r.afterInjectCount === 4],
    ["检索：'Welcome' 至少命中 1", r.searchHits >= 1],
    ["推送：至少收到 1 次 new-mail", newMail.length >= 1],
    ["幂等：mail-ingested 无重复入库", r.afterInjectCount === 4],
    // ADR-009 断言：tenantId 全链路透传
    ["ADR-009：ScenarioResult.tenantId === tenantA", r.tenantId === "tenantA"],
    ["ADR-009：通知 payload 携带 tenantA", newMail.every((n) => n.tenantId === "tenantA")],
    ["ADR-009：租户隔离——default 租户看不到 tenantA 的邮件", await crossTenantIsolationCheck(app, accountId)],
  ];

  console.log("=== Phase 0 PoC 自检（ADR-009 多租户）===");
  let allPass = true;
  for (const [name, ok] of checks) {
    console.log(`${ok ? "PASS" : "FAIL"}  ${name}`);
    if (!ok) allPass = false;
  }
  console.log(allPass ? "\nRESULT: ALL PASS ✅" : "\nRESULT: FAILED ❌");
  process.exit(allPass ? 0 : 1);
}

/**
 * 跨租户隔离断言：default 租户不应能查到 tenantA 的邮件。
 * 模拟：从 default 租户视角查询同一 accountId，应返回空。
 */
async function crossTenantIsolationCheck(app: ReturnType<typeof buildApp>, accountId: string): Promise<boolean> {
  // tenantA 视角：应能查到邮件
  const tenantAView = await app.store.count("tenantA", accountId);
  // default 视角：同一 accountId 应看不到 tenantA 的邮件（隔离）
  const defaultView = await app.store.count(DefaultTenantID, accountId);
  // 同样验证检索层
  const tenantASearch = (await app.search.search("tenantA", accountId, "Welcome", 10)).length;
  const defaultSearch = (await app.search.search(DefaultTenantID, accountId, "Welcome", 10)).length;
  return tenantAView > 0 && defaultView === 0 && tenantASearch > 0 && defaultSearch === 0;
}

main().catch((e) => {
  console.error("SELFTEST ERROR", e);
  process.exit(1);
});
