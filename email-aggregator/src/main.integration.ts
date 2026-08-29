/**
 * 集成入口：使用 Wire() 装配真实基础设施（PG/MinIO/OpenSearch/Kafka）。
 * 对齐 Go `cmd/server_integration.go`，但 TS 版无需构建标签保护（动态 import 机制天然支持）。
 *
 * 运行（需先启动 deploy/docker-compose.yml 提供的 PG/MinIO/OpenSearch/Redpanda）：
 *   node --experimental-strip-types src/main.integration.ts
 *
 * 环境变量（默认值对齐 docker-compose.yml）：
 *   PG_DSN           postgres://agg:agg@localhost:5432/agg
 *   MINIO_ENDPOINT   localhost:9000
 *   MINIO_BUCKET     agg-mail
 *   MINIO_ACCESS_KEY agg
 *   MINIO_SECRET_KEY agg-secret
 *   MINIO_SECURE     false
 *   OPENSEARCH_ADDR  https://localhost:9200
 *   OPENSEARCH_USER  admin
 *   OPENSEARCH_PASS  Kp3mQ9@vL2*rT7xA8
 *   KAFKA_BROKERS    localhost:9092
 *   HTTP_PORT        8080
 *
 * 若 Wire() 失败（缺少依赖 / 连接失败），优雅回退到 InMemory 装配（buildApp），
 * 保证开发环境无外部依赖也能启动。
 */

import { loadConfig } from "./config.ts";
import { buildApp } from "./app.ts";
import {
  Wire,
  fromAppConfig,
  type IntegrationConfig,
  type RealAdapters,
} from "./integration/integration.ts";
import { Resolve, fromRequest, type TenantContext } from "./tenant/tenant.ts";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";

// ───────────────────────────────────────────────────────────────────────────
// 环境变量读取
// ───────────────────────────────────────────────────────────────────────────

function env(key: string, def: string): string {
  return process.env[key] || def;
}

function envBool(key: string, def: boolean): boolean {
  const v = process.env[key];
  if (!v) return def;
  return v === "true" || v === "1" || v === "yes";
}

function envPort(key: string, def: number): number {
  const v = process.env[key];
  if (!v) return def;
  const n = parseInt(v, 10);
  if (isNaN(n) || n <= 0 || n > 65535) {
    console.warn(`[warn] ${key}=${v} 非法，回退默认端口 ${def}`);
    return def;
  }
  return n;
}

function readIntegrationConfig(): IntegrationConfig {
  return fromAppConfig(loadConfig(), {
    pgDsn: env("PG_DSN", "postgres://agg:agg@localhost:5432/agg"),
    objectEndpoint: env("MINIO_ENDPOINT", "localhost:9000"),
    objectBucket: env("MINIO_BUCKET", "agg-mail"),
    objectAccessKey: env("MINIO_ACCESS_KEY", "agg"),
    objectSecretKey: env("MINIO_SECRET_KEY", "agg-secret"),
    objectSecure: envBool("MINIO_SECURE", false),
    openSearchAddr: env("OPENSEARCH_ADDR", "https://localhost:9200"),
    openSearchUser: env("OPENSEARCH_USER", "admin"),
    openSearchPass: env("OPENSEARCH_PASS", "Kp3mQ9@vL2*rT7xA8"),
    kafkaBrokers: [env("KAFKA_BROKERS", "localhost:9092")],
    kmsEndpoint: process.env.KMS_ENDPOINT, // 可选：Phase 1+ KMS
  });
}

// ───────────────────────────────────────────────────────────────────────────
// REST + WS 服务（复用 InMemory 版的 ApiServer 形态，但注入 RealAdapters）
// ───────────────────────────────────────────────────────────────────────────

async function startServer(adapters: RealAdapters, port: number): Promise<void> {
  const { metadata, index, notifier } = adapters;

  const server = createServer(async (req: IncomingMessage, res: ServerResponse) => {
    // ADR-009：从请求头解析租户上下文
    const tctx: TenantContext = (() => {
      const parsed = fromRequest(req);
      return { tenantId: Resolve(parsed.tenantId), tier: parsed.tier };
    })();
    const tid = tctx.tenantId;

    const url = new URL(req.url ?? "/", `http://localhost:${port}`);
    const q = url.searchParams;

    // 健康检查
    if (url.pathname === "/health") {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ status: "ok", tenantId: tid }));
      return;
    }

    // 列表邮件
    if (url.pathname === "/mails" && req.method === "GET") {
      const accountId = q.get("accountId") || "";
      const folder = q.get("folder") || "INBOX";
      const limit = Math.min(parseInt(q.get("limit") || "50", 10) || 50, 200);
      try {
        const mails = await metadata.listMails(tid, accountId, folder as any, limit);
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ tenantId: tid, accountId, folder, count: mails.length, mails }));
      } catch (e) {
        res.writeHead(500, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: (e as Error).message }));
      }
      return;
    }

    // 全文检索
    if (url.pathname === "/search" && req.method === "GET") {
      const accountId = q.get("accountId") || "";
      const query = q.get("q") || "";
      const limit = Math.min(parseInt(q.get("limit") || "50", 10) || 50, 200);
      try {
        const hits = await index.search(tid, accountId, query, limit);
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ tenantId: tid, accountId, query, count: hits.length, hits }));
      } catch (e) {
        res.writeHead(500, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: (e as Error).message }));
      }
      return;
    }

    res.writeHead(404);
    res.end("Not Found");
  });

  // 挂载 WebSocket（notifier 已实现 attach/listen）
  if ("attach" in notifier && typeof notifier.attach === "function") {
    notifier.attach(server);
  }

  server.listen(port, () => {
    console.log(`\n=== 邮箱聚合平台集成服务（真实基础设施）===`);
    console.log(`监听端口: http://localhost:${port}`);
    console.log(`租户模式: ADR-009 多租户隔离（从 X-Tenant-Id 请求头解析）`);
    console.log(`\n端点:`);
    console.log(`  GET /health?accountId=xxx`);
    console.log(`  GET /mails?accountId=xxx&folder=INBOX&limit=50`);
    console.log(`  GET /search?accountId=xxx&q=keyword`);
    console.log(`  WS  /notifications (subscribe {"accountId":"xxx"})`);
    console.log(`\n基础设施:`);
    console.log(`  PostgreSQL:  ${env("PG_DSN", "postgres://agg:agg@localhost:5432/agg")}`);
    console.log(`  MinIO:       ${env("MINIO_ENDPOINT", "localhost:9000")}/${env("MINIO_BUCKET", "agg-mail")}`);
    console.log(`  OpenSearch:  ${env("OPENSEARCH_ADDR", "https://localhost:9200")}`);
    console.log(`  Kafka:       ${env("KAFKA_BROKERS", "localhost:9092")}`);
    console.log(`\n服务常驻中（Ctrl+C 退出）...`);
  });
}

// ───────────────────────────────────────────────────────────────────────────
// 主入口
// ───────────────────────────────────────────────────────────────────────────

async function main(): Promise<void> {
  const port = envPort("HTTP_PORT", 8080);
  const cfg = readIntegrationConfig();

  console.log("尝试装配真实基础设施（PG/MinIO/OpenSearch/Kafka）...");

  let adapters: RealAdapters | null = null;
  try {
    adapters = await Wire(cfg);
    console.log("✓ 真实基础设施装配成功");
  } catch (e) {
    console.warn(`✗ 真实基础设施装配失败: ${(e as Error).message}`);
    console.warn("→ 优雅回退到 InMemory 装配（开发模式）\n");
  }

  if (adapters) {
    await startServer(adapters, port);

    // 优雅关闭
    process.on("SIGINT", async () => {
      console.log("\n收到关闭信号，释放资源...");
      await adapters!.close();
      process.exit(0);
    });
  } else {
    // 回退到 InMemory
    const app = buildApp(loadConfig(), { tenantId: "default", tier: "" });
    await app.api.listen();
    console.log("InMemory 装配已启动（无外部依赖）");
  }
}

main().catch((e) => {
  console.error("FATAL:", e);
  process.exit(1);
});
