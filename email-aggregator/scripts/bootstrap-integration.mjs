#!/usr/bin/env node
/**
 * ============================================================================
 * bootstrap-integration.mjs — 真实集成栈「环境预置」（CI 与本地通用）
 * ----------------------------------------------------------------------------
 * 职责（对应 deploy/bootstrap.sh 的第 2、3 步，但**不需要 Docker**）：
 *   1) 应用 PG 迁移（deploy/migrations/[0-9]*.sql，字典序）
 *   2) 预建 MinIO 桶（MINIO_BUCKET，幂等）
 *
 * 为什么需要它：
 *   CI 的 integration job 用 `services:` 拉起 PG / MinIO / OpenSearch / Kafka，
 *   但**容器起来 ≠ schema 就绪**。此前缺这一步，导致：
 *     - TS 集成测试：relation "mail_metadata" does not exist
 *     - TS 集成测试：The specified bucket does not exist
 *   （Go 集成测试只覆盖 Kafka + OpenSearch，故长期掩盖了这个缺口。）
 *
 * 迁移文件契约（新增迁移必须满足，否则 CI 会红）：
 *   - 幂等：重复执行无副作用（IF NOT EXISTS / ON CONFLICT / DO $$ 守卫）
 *   - 纯 PG 安全：不得无条件依赖 Citus 等扩展；需扩展时用
 *     `DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_available_extensions ...) ... END $$;` 守卫
 *   （002 / 003_citus_rebalance / 006_citus_workers 均已按此约定加守卫，可在 postgres:16 上静默跳过）
 *
 * 用法：
 *   node scripts/bootstrap-integration.mjs            # 执行迁移 + 建桶
 *   node scripts/bootstrap-integration.mjs --dry-run  # 只打印计划（不连任何服务）
 *
 * 环境变量（与 tests/integration_real_e2e.ts 完全一致）：
 *   PG_DSN / MINIO_ENDPOINT / MINIO_BUCKET / MINIO_ACCESS_KEY / MINIO_SECRET_KEY / MINIO_SECURE
 *   BOOTSTRAP_WAIT_SECONDS（默认 60）—— PG 连接重试预算
 * ============================================================================
 */

import { readFileSync, readdirSync } from "node:fs";
import { createHash } from "node:crypto";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const PKG_DIR = resolve(SCRIPT_DIR, ".."); // email-aggregator/
const REPO_ROOT = resolve(PKG_DIR, ".."); // 仓库根
const MIGRATIONS_DIR = join(REPO_ROOT, "email-aggregator-go", "deploy", "migrations");

const DRY_RUN = process.argv.includes("--dry-run");

const PG_DSN = process.env.PG_DSN || "postgres://agg:agg-secret@127.0.0.1:15432/mailagg";
const MINIO_ENDPOINT = process.env.MINIO_ENDPOINT || "127.0.0.1:9000";
const MINIO_BUCKET = process.env.MINIO_BUCKET || "agg-mail";
const MINIO_ACCESS_KEY = process.env.MINIO_ACCESS_KEY || "agg";
const MINIO_SECRET_KEY = process.env.MINIO_SECRET_KEY || "agg-secret";
const MINIO_SECURE = (process.env.MINIO_SECURE || "false") === "true";
const WAIT_SECONDS = Number(process.env.BOOTSTRAP_WAIT_SECONDS || 60);

/** DSN 脱敏：仅保留用户名，密码以 *** 代替（避免日志泄漏） */
function redactDsn(dsn) {
  return dsn.replace(/\/\/([^:/@]+):[^@]*@/, "//$1:***@");
}

const sha256 = (buf) => createHash("sha256").update(buf).digest("hex");

/** 枚举迁移文件：仅 [0-9]*.sql，按字典序（与 deploy/migrations/run.sh 的 glob 一致） */
function listMigrations() {
  const names = readdirSync(MIGRATIONS_DIR)
    .filter((f) => /^\d.*\.sql$/.test(f))
    .sort();
  if (names.length === 0) {
    throw new Error(`未在 ${MIGRATIONS_DIR} 找到任何迁移文件（期望 [0-9]*.sql）`);
  }
  return names.map((name) => {
    const abs = join(MIGRATIONS_DIR, name);
    const sql = readFileSync(abs, "utf8");
    return { name, abs, sql, bytes: Buffer.byteLength(sql), sha256: sha256(sql) };
  });
}

/** PG 连接重试（CI 的 service healthcheck 通过后仍可能有极短的认证窗口） */
async function connectPg() {
  const pg = await import("pg").catch((e) => {
    throw new Error(`缺少 pg 依赖：请在 email-aggregator 下执行 npm ci；原始错误：${e.message}`);
  });
  const Client = pg.default?.Client ?? pg.Client;
  if (typeof Client !== "function") throw new Error("pg 模块未导出 Client，请检查 pg 版本");

  const deadline = Date.now() + WAIT_SECONDS * 1000;
  let lastErr = null;
  for (let i = 1; ; i++) {
    const client = new Client({ connectionString: PG_DSN });
    try {
      await client.connect();
      if (i > 1) console.log(`  PG 已连接（第 ${i} 次尝试）`);
      return client;
    } catch (e) {
      lastErr = e;
      await client.end().catch(() => {});
      if (Date.now() >= deadline) {
        throw new Error(
          `PG 在 ${WAIT_SECONDS}s 内不可达：${lastErr?.message}\n` +
            `  请确认 PG 已启动且端口可达（当前 DSN：${redactDsn(PG_DSN)}）`,
        );
      }
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
}

// ── 步骤 1：PG 迁移 ──────────────────────────────────────────────────────────
async function applyMigrations(files) {
  console.log(`\n[1/2] PG 迁移 —— ${redactDsn(PG_DSN)}`);
  const client = await connectPg();
  try {
    const { rows } = await client.query("SELECT version() AS v");
    console.log(`  server: ${String(rows[0]?.v ?? "?").split(" ").slice(0, 2).join(" ")}`);
    for (const f of files) {
      // 无参数 → simple query 协议，可一次下发多语句（含 DO $$ ... $$; 块）
      await client.query(f.sql);
      console.log(`  ok  ${f.name}  (${f.bytes} B, sha256:${f.sha256.slice(0, 12)})`);
    }
    // 关键落点自检：迁移后必须真实存在，否则后续集成测试会以晦涩错误失败
    const need = ["mail_metadata", "account_sync_cursor"];
    const { rows: got } = await client.query(
      `SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename = ANY($1::text[])`,
      [need],
    );
    const have = new Set(got.map((r) => r.tablename));
    const missing = need.filter((t) => !have.has(t));
    if (missing.length > 0) {
      throw new Error(`迁移已执行但表仍缺失：${missing.join(", ")}（请检查迁移文件是否被正确应用）`);
    }
    const { rows: cols } = await client.query(
      `SELECT column_name FROM information_schema.columns
       WHERE table_name='mail_metadata' AND column_name IN ('tenant_id','read')`,
    );
    const colNames = cols.map((c) => c.column_name).sort();
    console.log(`  自检通过：${need.join(" / ")} 存在；mail_metadata 关键列 [${colNames.join(", ")}]`);
  } finally {
    await client.end().catch(() => {});
  }
}

// ── 步骤 2：MinIO 建桶 ──────────────────────────────────────────────────────
async function ensureBucket() {
  console.log(`\n[2/2] MinIO 建桶 —— ${MINIO_ENDPOINT}/${MINIO_BUCKET}`);
  const mod = await import("minio").catch((e) => {
    throw new Error(`缺少 minio 依赖：请在 email-aggregator 下执行 npm ci；原始错误：${e.message}`);
  });
  const Client = mod.Client ?? mod.default?.Client;
  if (typeof Client !== "function") throw new Error("minio 模块未导出 Client");

  const [host, port] = MINIO_ENDPOINT.split(":");
  const client = new Client({
    endPoint: host,
    port: Number(port ?? (MINIO_SECURE ? 443 : 80)),
    useSSL: MINIO_SECURE,
    accessKey: MINIO_ACCESS_KEY,
    secretKey: MINIO_SECRET_KEY,
  });

  const exists = await client.bucketExists(MINIO_BUCKET);
  if (exists) {
    console.log(`  已存在，跳过创建（幂等）`);
  } else {
    await client.makeBucket(MINIO_BUCKET);
    console.log(`  ok  已创建桶 ${MINIO_BUCKET}`);
  }
  // 复验：确保后续 put 不会因"桶不存在"而失败
  if (!(await client.bucketExists(MINIO_BUCKET))) {
    throw new Error(`桶 ${MINIO_BUCKET} 创建后仍不可见，请检查 MinIO 权限与服务状态`);
  }
  console.log(`  自检通过：桶 ${MINIO_BUCKET} 可访问`);
}

// ── 主流程 ──────────────────────────────────────────────────────────────────
async function main() {
  const files = listMigrations();
  console.log("=== 集成环境预置（PG 迁移 + MinIO 建桶）===");
  console.log(`迁移目录：${MIGRATIONS_DIR}`);
  console.log(`待应用 ${files.length} 个迁移：${files.map((f) => f.name).join(", ")}`);

  if (DRY_RUN) {
    console.log("\n--dry-run：仅打印计划，不连接任何服务。");
    for (const f of files) console.log(`  ${f.name}  (${f.bytes} B, sha256:${f.sha256})`);
    console.log(`  MinIO 桶：${MINIO_BUCKET} @ ${MINIO_ENDPOINT}`);
    return;
  }

  await applyMigrations(files);
  await ensureBucket();

  console.log("\n=== 预置完成：PG schema 与对象存储桶均已就绪 ===");
}

main().catch((e) => {
  console.error(`\n[error] 集成环境预置失败：${e.message}`);
  if (!DRY_RUN) {
    console.error("  排查建议：");
    console.error("    1) 服务未起：确认 PG/MinIO 容器健康（CI 里看 Initialize containers 步骤）");
    console.error("    2) 端口不符：核对 PG_DSN / MINIO_ENDPOINT 与 ports 映射是否一致");
    console.error("    3) 迁移报错：单独重跑 node scripts/bootstrap-integration.mjs 看首个失败文件");
  }
  process.exit(1);
});
