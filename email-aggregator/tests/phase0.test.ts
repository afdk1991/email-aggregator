/**
 * Phase 0 PoC 单元测试（零第三方依赖，Node 内置 node:test）。
 * 运行：node --test --experimental-strip-types tests/phase0.test.ts
 *
 * 覆盖：状态机迁移表 / KMS 信封加解密 / 内存邮件服务器游标与 IDLE /
 *       事件总线幂等与 DLQ / IngestService 端到端 / 检索索引 /
 *       连接器注册表 / buildApp+runScenario 全链路集成。
 */

import { test } from "node:test";
import assert from "node:assert/strict";

import { SyncStateMachine } from "../src/sync/stateMachine.ts";
import { CredentialVault, InMemoryKms } from "../src/security/vault.ts";
import { InMemoryMailServer, IMAPConnector } from "../src/connector/imap.ts";
import { InMemoryEventBus } from "../src/events/bus.ts";
import { IngestService } from "../src/ingest/ingest.ts";
import { InMemoryMetadataStore } from "../src/store/metadata.ts";
import { InMemoryContentStore } from "../src/store/content.ts";
import { InMemorySearchIndex } from "../src/search/index.ts";
import { ConnectorRegistry } from "../src/connector/registry.ts";
import { buildApp, runScenario } from "../src/app.ts";

// ---------------------------------------------------------------------------
// 1) 同步状态机：迁移表正确性
// ---------------------------------------------------------------------------
test("状态机：合法迁移沿 7 态推进", () => {
  const fsm = new SyncStateMachine();
  assert.equal(fsm.current, "UNCONNECTED");
  assert.equal(fsm.send("AUTH_OK"), true); // UNCONNECTED -> AUTHORIZING
  assert.equal(fsm.current, "AUTHORIZING");
  assert.equal(fsm.send("AUTH_OK"), true); // AUTHORIZING -> INITIAL_FULL
  assert.equal(fsm.send("FULL_DONE"), true); // -> INCREMENTAL
  assert.equal(fsm.current, "INCREMENTAL");
});

test("状态机：自环 CHANGE_RECEIVED 不产状态变化但通知监听者", () => {
  const fsm = new SyncStateMachine();
  fsm.send("AUTH_OK");
  fsm.send("AUTH_OK");
  fsm.send("FULL_DONE"); // INCREMENTAL
  let notified = 0;
  fsm.onTransition(() => notified++);
  const changed = fsm.send("CHANGE_RECEIVED");
  assert.equal(changed, false); // 仍是 INCREMENTAL
  assert.equal(fsm.current, "INCREMENTAL");
  assert.equal(notified, 1); // 监听者仍被回调
});

test("状态机：非法迁移抛错", () => {
  const fsm = new SyncStateMachine();
  fsm.send("AUTH_OK");
  fsm.send("AUTH_OK");
  fsm.send("FULL_DONE"); // INCREMENTAL
  assert.throws(() => fsm.send("AUTH_OK"), /illegal transition/);
});

test("状态机：THROTTLED -> RESUME -> INCREMENTAL", () => {
  const fsm = new SyncStateMachine();
  fsm.send("AUTH_OK");
  fsm.send("AUTH_OK");
  fsm.send("FULL_DONE"); // INCREMENTAL
  assert.equal(fsm.send("TRANSIENT_ERR"), true); // -> THROTTLED
  assert.equal(fsm.current, "THROTTLED");
  assert.equal(fsm.send("RESUME"), true); // -> INCREMENTAL
  assert.equal(fsm.current, "INCREMENTAL");
});

// ---------------------------------------------------------------------------
// 2) KMS 信封加密：加解密往返 + 密钥版本 + 轮换
// ---------------------------------------------------------------------------
test("KMS 信封：seal 后 unseal 还原明文", async () => {
  const vault = new CredentialVault(new InMemoryKms());
  const secret = "s3cr3t-access-token";
  const sealed = await vault.seal(secret);
  assert.equal(sealed.keyVersion, 1);
  assert.notEqual(sealed.ciphertext, "");
  const back = await vault.unseal(sealed);
  assert.equal(back, secret);
});

test("KMS 信封：错误 keyVersion 解包失败", async () => {
  const vault = new CredentialVault(new InMemoryKms(undefined, 3));
  const sealed = await vault.seal("x");
  // 伪造一个版本不匹配的密封体
  await assert.rejects(
    () => vault.unseal({ ...sealed, keyVersion: 9 }),
    /kek version mismatch/,
  );
});

test("KMS 信封：rotate 后仍可还原且 keyVersion 更新", async () => {
  const kms = new InMemoryKms(undefined, 1);
  const vault = new CredentialVault(kms);
  const sealed = await vault.seal("token-123");
  const rotated = await vault.rotate(sealed);
  assert.equal(rotated.keyVersion, 1); // 本 KMS 仅 1 版，轮换后仍是 1 版但重新封装
  assert.equal(await vault.unseal(rotated), "token-123");
});

// ---------------------------------------------------------------------------
// 3) 内存邮件服务器：游标语义 + IDLE 推送
// ---------------------------------------------------------------------------
test("邮件服务器：inject 分配自增 uid/modSeq", () => {
  const srv = new InMemoryMailServer();
  const m1 = srv.inject({ from: "a@b.c", to: ["x@y.z"], subject: "s1", bodyText: "b1", internalDate: Date.now(), flags: { seen: false, flagged: false, answered: false, deleted: false } });
  const m2 = srv.inject({ from: "a@b.c", to: ["x@y.z"], subject: "s2", bodyText: "b2", internalDate: Date.now(), flags: { seen: false, flagged: false, answered: false, deleted: false } });
  assert.equal(m1.uid, "1");
  assert.equal(m2.uid, "2");
  assert.equal(m2.modSeq, 2);
  assert.equal(srv.highestModSeq(), 2);
});

test("邮件服务器：fetchChangesSince 以 highestModSeq 为游标", () => {
  const srv = new InMemoryMailServer();
  srv.inject({ from: "a", to: ["x"], subject: "1", bodyText: "b", internalDate: 1, flags: { seen: false, flagged: false, answered: false, deleted: false } });
  const cs0 = srv.fetchChangesSince(0);
  assert.equal(cs0.added.length, 1);
  const cs1 = srv.fetchChangesSince(cs0.nextCursor.highestModSeq ? Number(cs0.nextCursor.highestModSeq) : 0);
  assert.equal(cs1.added.length, 0, "游标之后无新增应返回空");
});

test("邮件服务器：subscribe 后 inject 触发 onExists 回调", () => {
  const srv = new InMemoryMailServer();
  let called = 0;
  let lastCount = 0;
  const sub = srv.subscribe({ onExists: (_f, c) => { called++; lastCount = c; } });
  srv.inject({ from: "a", to: ["x"], subject: "1", bodyText: "b", internalDate: 1, flags: { seen: false, flagged: false, answered: false, deleted: false } });
  assert.equal(called, 1);
  assert.equal(lastCount, 1);
  return sub.unsubscribe().then(() => {
    srv.inject({ from: "a", to: ["x"], subject: "2", bodyText: "b", internalDate: 1, flags: { seen: false, flagged: false, answered: false, deleted: false } });
    assert.equal(called, 1, "退订后不应再回调");
  });
});

// ---------------------------------------------------------------------------
// 4) 事件总线：start 后才派发 / 幂等去重 / DLQ
// ---------------------------------------------------------------------------
test("事件总线：start 前 publish 被丢弃", async () => {
  const bus = new InMemoryEventBus();
  let got = 0;
  bus.subscribe("audit", () => { got++; });
  await bus.publish({ topic: "audit", partitionKey: "a", idempotencyKey: "k1", ts: 1 } as never);
  assert.equal(got, 0);
  await bus.start();
  await bus.publish({ topic: "audit", partitionKey: "a", idempotencyKey: "k2", ts: 2 } as never);
  assert.equal(got, 1);
});

test("事件总线：相同 idempotencyKey 仅消费一次", async () => {
  const bus = new InMemoryEventBus();
  let got = 0;
  bus.subscribe("mail-ingested", () => { got++; });
  await bus.start();
  const ev = { topic: "mail-ingested", partitionKey: "a", idempotencyKey: "dup", ts: 1 } as never;
  await bus.publish(ev);
  await bus.publish(ev); // 至少一次语义下可能重复
  assert.equal(got, 1, "幂等去重应使 handler 只触发一次");
});

test("事件总线：handler 抛错进入 DLQ", async () => {
  const bus = new InMemoryEventBus();
  bus.subscribe("audit", () => { throw new Error("boom"); });
  await bus.start();
  await bus.publish({ topic: "audit", partitionKey: "a", idempotencyKey: "e1", ts: 1 } as never);
  assert.equal(bus.getDlq().length, 1);
});

// ---------------------------------------------------------------------------
// 5) IngestService：sync-tasks -> 落库 + 发布 mail-ingested
// ---------------------------------------------------------------------------
test("IngestService：消费 sync-tasks 完成入库并发布 mail-ingested", async () => {
  const srv = new InMemoryMailServer();
  srv.inject({ from: "boss@c.c", to: ["me@a.d"], subject: "Ping", bodyText: "hello", internalDate: Date.now(), flags: { seen: false, flagged: false, answered: false, deleted: false } });
  const connector = new IMAPConnector(srv);
  const bus = new InMemoryEventBus();
  await bus.start();
  const store = new InMemoryMetadataStore();
  const content = new InMemoryContentStore();
  const ingest = new IngestService({ connector, bus, metadata: store, content });
  ingest.start();

  let ingested = 0;
  let ingestedTenant: string | undefined;
  bus.subscribe("mail-ingested", (e) => { ingested++; ingestedTenant = (e as { tenantId?: string }).tenantId; });

  // 模拟 orchestrator 发出一条 sync-task（uid=1）—— ADR-009：携带 tenantId
  await bus.publish({
    topic: "sync-tasks",
    partitionKey: "acc_x",
    idempotencyKey: "default|acc_x|task|1|0",
    ts: Date.now(),
    tenantId: "default",
    folder: "INBOX",
    cursor: { highestModSeq: "1" },
    priority: 5,
  } as never);

  await new Promise((r) => setTimeout(r, 50));
  assert.equal(await store.count("default", "acc_x"), 1);
  assert.equal(ingested, 1);
  assert.equal(ingestedTenant, "default", "ADR-009：mail-ingested 应透传 tenantId");
  const mail = (await store.listMails("default", "acc_x", "INBOX", 10))[0];
  assert.equal(mail.subject, "Ping");
  assert.equal(mail.tenantId, "default", "ADR-009：CanonicalMail 应携带 tenantId");
  assert.ok(mail.rawObjectKey, "应回填内容对象键");
});

// ---------------------------------------------------------------------------
// 6) 检索索引：命中 / 未命中 / 账户隔离
// ---------------------------------------------------------------------------
test("检索索引：关键词命中与账户隔离（ADR-009 跨租户）", async () => {
  const idx = new InMemorySearchIndex();
  const mk = (tenantId: string, accountId: string, subject: string, body: string) => ({
    idempotencyKey: `${tenantId}|${accountId}|INBOX|${subject}`,
    tenantId, accountId, folder: "INBOX" as const, messageId: `${subject}@x`,
    from: [{ address: "a@b.c" }], to: [], cc: [],
    subject, bodyText: body, attachments: [],
    flags: { seen: false, flagged: false, answered: false, deleted: false },
    internalDate: 1, sizeBytes: 1, cursor: {},
  });
  // 同一 accountId 在两个租户下各有邮件
  await idx.index(mk("tenantA", "acc1", "Welcome", "hi there"));
  await idx.index(mk("tenantB", "acc1", "Welcome", "secret tenantB"));
  const hA = await idx.search("tenantA", "acc1", "welcome", 10);
  assert.equal(hA.length, 1, "tenantA 只应命中自己的邮件");
  assert.equal(hA[0].accountId, "acc1");
  assert.equal(hA[0].tenantId, "tenantA");
  const hB = await idx.search("tenantB", "acc1", "welcome", 10);
  assert.equal(hB.length, 1, "tenantB 只应命中自己的邮件");
  assert.equal(hB[0].tenantId, "tenantB");
  const miss = await idx.search("tenantA", "acc1", "nonexistent", 10);
  assert.equal(miss.length, 0);
  // 跨租户隔离：tenantA 查不到 tenantB 的 "secret" 邮件
  const crossLeak = await idx.search("tenantA", "acc1", "secret", 10);
  assert.equal(crossLeak.length, 0, "ADR-009：跨租户检索不应泄漏");
});

// ---------------------------------------------------------------------------
// 7) 连接器注册表：注册/获取/缺失抛错
// ---------------------------------------------------------------------------
test("连接器注册表：按 providerType 路由", () => {
  const reg = new ConnectorRegistry();
  const srv = new InMemoryMailServer();
  reg.register("imap", () => new IMAPConnector(srv));
  assert.deepEqual(reg.list(), ["imap"]);
  const c = reg.get("imap");
  assert.equal(c.providerType, "imap");
  assert.throws(() => reg.get("pop3" as never), /no connector registered/);
});

// ---------------------------------------------------------------------------
// 8) 全链路集成：buildApp + runScenario
// ---------------------------------------------------------------------------
test("集成：buildApp + runScenario 端到端闭环（ADR-009 默认租户）", async () => {
  const app = buildApp();
  const r = await runScenario(app, "acc_demo");
  assert.equal(r.initialCount, 3, "初始全量种子 3 封");
  assert.equal(r.afterInjectCount, 4, "注入后 4 封");
  assert.ok(r.searchHits >= 1, "检索命中");
  assert.equal(r.tenantId, "default", "ADR-009：默认租户应为 default");
  const newMail = app.inMemory.getReceived().filter((n) => n.kind === "new-mail");
  assert.ok(newMail.length >= 1, "至少 1 次 new-mail 推送");
  assert.ok(newMail.every((n) => n.tenantId === "default"), "ADR-009：通知 payload 携带 tenantId");
});

// ---------------------------------------------------------------------------
// 9) Integration 适配层骨架（#ts6）：模块加载 + 接口形态 + Wire 工厂签名
//    不调用 Wire()（会触发外部依赖动态 import），仅验证：
//    - integration.ts 在零外部依赖下可加载
//    - 类型签名对齐 Go Wire()
//    - Phase 2 / ADR-009 物理隔离要素（OpenSearch mail-<tid> / 对象存储 tenant-<tid>/ 前缀 / KMS KEK）
// ---------------------------------------------------------------------------
test("Integration：模块加载 + Wire/Config/RealAdapters 形态对齐 Go（#ts6）", async () => {
  const mod = await import("../src/integration/integration.ts");
  assert.equal(typeof mod.Wire, "function", "Wire 应为 async 函数");
  assert.equal(typeof mod.fromAppConfig, "function", "fromAppConfig 工厂存在");
  assert.equal(typeof mod.NewPgMetadataStore, "function", "NewPgMetadataStore 工厂存在");
  assert.equal(typeof mod.NewObjectContentStore, "function", "NewObjectContentStore 工厂存在");
  assert.equal(typeof mod.NewOpenSearchIndex, "function", "NewOpenSearchIndex 工厂存在");
  assert.equal(typeof mod.NewKafkaAdapter, "function", "NewKafkaAdapter 工厂存在");
  assert.equal(typeof mod.NewWsHub, "function", "NewWsHub 工厂存在");
  assert.equal(typeof mod.NewTenantKms, "function", "NewTenantKms 工厂存在（Phase 1+ KMS KEK）");
  // KMS InMemory 实现：租户 KEK 派生 + 跨租户解密失败
  const kms = mod.NewTenantKms();
  const tid = "tenantA";
  const keyId = kms.activeKeyId(tid);
  assert.ok(keyId.startsWith("kek-tenantA-"), "activeKeyId 应携带租户 ID 前缀");
  const ctx = Buffer.from("acc1|INBOX|msg-001");
  const { plaintextDEK, encryptedDEK, keyId: kid } = await kms.deriveDEK(tid, ctx);
  assert.equal(kid, keyId, "派生 DEK 应引用当前 activeKeyId");
  assert.ok(plaintextDEK.length === 32, "plaintextDEK 应为 256bit");
  const restored = await kms.decryptDEK(tid, encryptedDEK, kid);
  assert.deepEqual(restored, plaintextDEK, "decryptDEK 应还原 plaintextDEK");
  // 跨租户解密应失败（Phase 1+ 物理隔离核心断言）
  await assert.rejects(
    () => kms.decryptDEK("tenantB", encryptedDEK, kid),
    /无法解密|cross-tenant|跨租户/i,
    "ADR-009 Phase 1+：跨租户 KEK 不匹配应解密失败",
  );
});

test("Integration：fromAppConfig + Phase 2 / ADR-009 物理隔离要素（OpenSearch mail-<tid> 索引 / 对象存储 tenant-<tid>/ 前缀）", async () => {
  const mod = await import("../src/integration/integration.ts");
  const { defaultConfig } = await import("../src/config.ts");
  const cfg = mod.fromAppConfig(defaultConfig, {
    objectBucket: "mail-bucket",
    objectAccessKey: "ak",
    objectSecretKey: "sk",
  });
  assert.equal(cfg.pgDsn, defaultConfig.pgDsn, "pgDsn 透传");
  assert.equal(cfg.objectBucket, "mail-bucket", "objectBucket 覆盖生效");
  assert.equal(cfg.objectAccessKey, "ak", "objectAccessKey 覆盖生效");
  assert.deepEqual(cfg.kafkaBrokers, defaultConfig.kafkaBrokers, "kafkaBrokers 透传");
  assert.equal(cfg.objectSecure, false, "default http → secure=false");

  // Phase 2 / ADR-009 物理隔离：对象存储前缀 tenant-<tid>/mail/<sha256>
  // 由于 ObjectContentStore.put 需要真实 minio 连接，无法在零依赖单测中验证；
  // 改为校验外部接口契约：put 接受 tenantId 参数（ContentStore 接口扩展）
  // —— 此处仅做静态形态校验，不实际调用
  assert.equal(typeof mod.ObjectContentStore, "function", "ObjectContentStore 类导出");

  // Phase 2 / ADR-009 物理隔离：OpenSearch 索引 mail-<tid>
  // OpenSearchIndex.search 也需要真实连接，无法单测；改为校验类导出
  assert.equal(typeof mod.OpenSearchIndex, "function", "OpenSearchIndex 类导出");
  assert.equal(typeof mod.PgMetadataStore, "function", "PgMetadataStore 类导出");
});
