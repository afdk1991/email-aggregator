/**
 * 应用装配：把各组件按事件总线接起来，形成端到端闭环。
 * demo / main / 集成测试共用此装配，避免重复接线。
 */

import { loadConfig, type AppConfig } from "./config.ts";
import { InMemoryMailServer, IMAPConnector, type StoredMessage } from "./connector/imap.ts";
import { ConnectorRegistry } from "./connector/registry.ts";
import { InMemoryEventBus, type EventBus } from "./events/bus.ts";
import type { MailIngestedEvent, AppEvent, AuditEvent } from "./events/contracts.ts";
import { CredentialVault, InMemoryKms } from "./security/vault.ts";
import { SyncOrchestrator } from "./sync/orchestrator.ts";
import { InMemoryMetadataStore } from "./store/metadata.ts";
import { InMemoryContentStore } from "./store/content.ts";
import { InMemorySearchIndex } from "./search/index.ts";
import { InMemoryNotifier, WsNotifier, type Notifier, type PushSink, type NotificationPayload } from "./notify/ws.ts";
import { IngestService } from "./ingest/ingest.ts";
import { ApiServer } from "./api/server.ts";
import { Resolve, type TenantContext } from "./tenant/tenant.ts";

const delay = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** 组合通知器：实时 WS 推送 + 内存收集（供自检断言） */
class CompositeNotifier implements Notifier {
  private list: Notifier[];
  constructor(list: Notifier[]) {
    this.list = list;
  }
  addSink(t: string, a: string, s: PushSink): void {
    this.list.forEach((n) => n.addSink(t, a, s));
  }
  removeSink(t: string, a: string, s: PushSink): void {
    this.list.forEach((n) => n.removeSink(t, a, s));
  }
  publish(t: string, a: string, p: NotificationPayload): void {
    this.list.forEach((n) => n.publish(t, a, p));
  }
}

function seed(server: InMemoryMailServer): void {
  const base = (over: Partial<StoredMessage>): Omit<StoredMessage, "uid" | "modSeq"> => ({
    from: "noreply@service.com",
    to: ["me@aggregator.dev"],
    subject: "Welcome to the platform",
    bodyText: "Thanks for connecting your mailbox. This is a seed message.",
    internalDate: Date.now() - 1000 * 60 * 60,
    flags: { seen: false, flagged: false, answered: false, deleted: false },
    ...over,
  });
  server.inject(base({ subject: "Welcome aboard", bodyText: "Welcome! Your inbox is now aggregated." }));
  server.inject(base({ subject: "Monthly report", bodyText: "Here is your monthly activity report." }));
  server.inject(base({ subject: "Invoice #1024", bodyText: "Please find attached your invoice." }));
}

export interface App {
  config: AppConfig;
  tenant: TenantContext;
  bus: EventBus;
  registry: ConnectorRegistry;
  orchestrator: SyncOrchestrator;
  ingest: IngestService;
  store: InMemoryMetadataStore;
  content: InMemoryContentStore;
  search: InMemorySearchIndex;
  notifier: CompositeNotifier;
  inMemory: InMemoryNotifier;
  ws: WsNotifier;
  api: ApiServer;
  mailServer: InMemoryMailServer;
}

export function buildApp(config: AppConfig = loadConfig(), tenantCtx: TenantContext = { tenantId: "default", tier: "" }): App {
  const mailServer = new InMemoryMailServer();
  seed(mailServer);

  const tid = Resolve(tenantCtx.tenantId);
  const orchestratorConnector = new IMAPConnector(mailServer);
  const ingestConnector = new IMAPConnector(mailServer);

  const bus = new InMemoryEventBus();
  const store = new InMemoryMetadataStore();
  const content = new InMemoryContentStore();
  const search = new InMemorySearchIndex();
  const inMemory = new InMemoryNotifier();
  const ws = new WsNotifier();
  const notifier = new CompositeNotifier([inMemory, ws]);

  const vault = new CredentialVault(new InMemoryKms());
  const orchestrator = new SyncOrchestrator({
    connector: orchestratorConnector,
    bus,
    cursors: store,
    sealer: vault,
    backoffMaxMs: config.backoffMaxMs,
    tenantId: tid,
  });

  const ingest = new IngestService({ connector: ingestConnector, bus, metadata: store, content });
  ingest.start();

  const registry = new ConnectorRegistry();
  registry.register("imap", () => new IMAPConnector(mailServer));

  // 下游消费：mail-ingested → 索引 + 通知 + 审计（按 tenantId 隔离路由）
  bus.subscribe("mail-ingested", (e) => {
    const ev = e as MailIngestedEvent;
    const evtTid = Resolve(ev.tenantId);
    void search.index(ev.canonicalMail);
    void bus.publish({
      topic: "mail-index",
      partitionKey: ev.accountId,
      idempotencyKey: `idx|${evtTid}|${ev.idempotencyKey}`,
      ts: Date.now(),
      tenantId: evtTid,
      docId: ev.idempotencyKey,
      accountId: ev.accountId,
      bodyText: ev.canonicalMail.bodyText,
    });
    notifier.publish(evtTid, ev.accountId, {
      kind: "new-mail",
      tenantId: evtTid,
      accountId: ev.accountId,
      preview: ev.canonicalMail.subject,
      ts: Date.now(),
    });
    void bus.publish({
      topic: "audit",
      partitionKey: ev.accountId,
      idempotencyKey: `audit|${evtTid}|ingest|${ev.idempotencyKey}`,
      ts: Date.now(),
      tenantId: evtTid,
      actor: "ingest-service",
      action: "mail.ingested",
      accountId: ev.accountId,
      detail: ev.source,
    } as AuditEvent as AppEvent);
  });

  const api = new ApiServer({ metadata: store, search, ws, port: config.httpPort, tenant: tenantCtx });

  return {
    config, tenant: tenantCtx, bus, registry, orchestrator, ingest, store, content,
    search, notifier, inMemory, ws, api, mailServer,
  };
}

export interface ScenarioResult {
  initialCount: number;
  afterInjectCount: number;
  searchHits: number;
  newMailNotifications: number;
  tenantId: string;
}

/** 端到端场景：初始全量 → 断言 → 注入新邮件（模拟 IDLE 推送）→ 断言增量 */
export async function runScenario(app: App, accountId = "acc_demo"): Promise<ScenarioResult> {
  await app.bus.start();
  await app.orchestrator.register(accountId);
  await app.orchestrator.authorizeAndStart(accountId, "demo-access-token");

  const tid = Resolve(app.tenant.tenantId);
  await delay(60); // 等待初始全量链路 settle
  const initialCount = await app.store.count(tid, accountId);
  const searchHits = (await app.search.search(tid, accountId, "Welcome", 10)).length;

  // 模拟上游新邮件到达（触发 IMAP IDLE 推送）
  app.mailServer.inject({
    from: "boss@corp.com",
    to: ["me@aggregator.dev"],
    subject: "Urgent: Q3 review",
    bodyText: "Please review the Q3 numbers before EOD.",
    internalDate: Date.now(),
    flags: { seen: false, flagged: true, answered: false, deleted: false },
  });

  await delay(150); // 等待 IDLE 异步链路完成
  const afterInjectCount = await app.store.count(tid, accountId);
  const newMailNotifications = app.inMemory
    .getReceived()
    .filter((n) => n.kind === "new-mail").length;

  return { initialCount, afterInjectCount, searchHits, newMailNotifications, tenantId: tid };
}
