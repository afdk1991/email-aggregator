/**
 * TS 集成层 · 真实中间件端到端联调（需先启动 deploy/docker-compose.yml 中间件栈）
 *
 * 运行（对齐 main.integration.ts 的环境变量）：
 *   node --experimental-strip-types tests/integration_real_e2e.ts
 *
 * 验证覆盖（对真实 PG / MinIO / OpenSearch / Kafka）：
 *   1. Wire() 真实装配（含 OpenSearch TLS 自签跳过 + 鉴权 ping）
 *   2. PG   upsertMail + getMail + listMails（真实落库/回读）
 *   3. MinIO put（Phase 1+ 前缀 tenant-<tid>/mail/<sha256>，租户内去重）
 *   4. OpenSearch index + search（mail-<tid> 单租户索引）
 *   5. Kafka publish mail-ingested（生产者真实发送）
 *   6. 资源释放 close()
 */

import { Wire, type RealAdapters } from "../src/integration/integration.ts";
import type { CanonicalMail } from "../src/model/canonical.ts";

const env = (k: string, d: string): string => process.env[k] || d;
const cfg = {
  pgDsn: env("PG_DSN", "postgres://agg:agg-secret@127.0.0.1:15432/mailagg"),
  objectEndpoint: env("MINIO_ENDPOINT", "127.0.0.1:9000"),
  objectBucket: env("MINIO_BUCKET", "agg-mail"),
  objectAccessKey: env("MINIO_ACCESS_KEY", "agg"),
  objectSecretKey: env("MINIO_SECRET_KEY", "agg-secret"),
  objectSecure: env("MINIO_SECURE", "false") === "true",
  openSearchAddr: env("OPENSEARCH_ADDR", "https://127.0.0.1:9200"),
  openSearchUser: env("OPENSEARCH_USER", "admin"),
  openSearchPass: env("OPENSEARCH_PASS", ""),
  kafkaBrokers: [env("KAFKA_BROKERS", "127.0.0.1:9092")],
};

const TID = "default";
const ACCOUNT = "acc_ts";
const KEY = "ts-real-e2e-20260829-001";

const mail: CanonicalMail = {
  idempotencyKey: KEY,
  tenantId: TID,
  accountId: ACCOUNT,
  folder: "INBOX",
  messageId: KEY,
  from: [{ name: "TS 集成", address: "ts@system.com" }],
  to: [],
  cc: [],
  subject: "TS集成真实联调邮件",
  bodyText: "这是 TS 集成层对接真实中间件的端到端验证，包含检索关键词：真实链路验收。",
  attachments: [],
  flags: { seen: false, flagged: false, answered: false, deleted: false },
  internalDate: Date.now(),
  sizeBytes: 200,
  cursor: {},
};

let pass = 0;
let fail = 0;
function check(name: string, ok: boolean, detail = ""): void {
  if (ok) { pass++; console.log(`PASS  ${name}${detail ? " — " + detail : ""}`); }
  else { fail++; console.log(`FAIL  ${name}${detail ? " — " + detail : ""}`); }
}

async function main(): Promise<void> {
  console.log("=== TS 集成层 · 真实中间件端到端联调 ===");
  console.log(`PG=${cfg.pgDsn}  MinIO=${cfg.objectEndpoint}/${cfg.objectBucket}  OS=${cfg.openSearchAddr}  Kafka=${cfg.kafkaBrokers[0]}\n`);

  let a: RealAdapters;
  try {
    a = await Wire(cfg);
    check("Wire() 真实装配成功（含 OpenSearch TLS 自签跳过 + 鉴权 ping）", true);
  } catch (e) {
    check("Wire() 真实装配成功", false, (e as Error).message);
    process.exit(1);
  }

  // 1) MinIO：内容寻址写入（Phase 1+ 前缀 tenant-<tid>/mail/<sha256>）
  try {
    const st = await a.content.put(Buffer.from(mail.bodyText ?? ""), TID);
    const prefixOk = st.objectKey.startsWith(`tenant-${TID}/mail/`);
    check("MinIO 内容寻址 put", prefixOk && st.contentHash.length === 64, st.objectKey);
    // 幂等：重复 put 返回同一 key
    const st2 = await a.content.put(Buffer.from(mail.bodyText ?? ""), TID);
    check("MinIO 租户内去重（重复 put 同 key）", st2.objectKey === st.objectKey, st2.objectKey);
  } catch (e) {
    check("MinIO 内容寻址 put", false, (e as Error).message);
  }

  // 2) PG：落库 + 回读 + 列表
  try {
    await a.metadata.upsertMail(mail);
    const got = await a.metadata.getMail(TID, ACCOUNT, KEY);
    check("PG upsertMail + getMail 回读", !!got && got.subject === mail.subject);
    const list = await a.metadata.listMails(TID, ACCOUNT, "INBOX", 10);
    check("PG listMails 含新邮件", list.some((m) => m.idempotencyKey === KEY), `count=${list.length}`);
  } catch (e) {
    check("PG 落库/回读", false, (e as Error).message);
  }

  // 3) OpenSearch：索引 + 检索（mail-default 单租户索引）
  try {
    await a.index.index(mail);
    // 索引后稍候，OpenSearch 近实时刷新
    await new Promise((r) => setTimeout(r, 1500));
    const hits = await a.index.search(TID, ACCOUNT, "真实链路", 5);
    check("OpenSearch index + search 命中", hits.some((h) => h.idempotencyKey === KEY), `hits=${hits.length}`);
  } catch (e) {
    check("OpenSearch index + search", false, (e as Error).message);
  }

  // 4) Kafka：发布 mail-ingested 事件
  try {
    await a.bus.publish({
      topic: "mail-ingested",
      partitionKey: ACCOUNT,
      idempotencyKey: KEY,
      ts: Date.now(),
      tenantId: TID,
      accountId: ACCOUNT,
      canonicalMail: mail,
      source: "imap",
    });
    check("Kafka publish mail-ingested", true, `topic=mail-ingested key=${ACCOUNT}`);
  } catch (e) {
    check("Kafka publish mail-ingested", false, (e as Error).message);
  }

  // 5) 清理
  await a.close();
  console.log(`\n=== 结果：PASS ${pass} / FAIL ${fail} ===`);
  process.exit(fail > 0 ? 1 : 0);
}

main().catch((e) => {
  console.error("FATAL:", e);
  process.exit(1);
});
