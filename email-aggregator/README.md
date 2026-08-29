# 邮箱聚合平台 · Phase 0 PoC

单 IMAP 协议端到端闭环的可运行骨架：**授权 → 增量同步 → 落库/落对象存储 → 检索 → 实时推送**。
本 PoC 用 **TypeScript + 仅 Node 内置模块** 实现，**零外部依赖即可本地跑通**；真实基础设施
（PostgreSQL / MinIO / OpenSearch / Kafka）以「接口 + 适配器 stub」形式预留，落地时直接替换。

> 技术栈说明：主架构文档（v1.1）建议 Go / Java21。本环境无 Go 工具链，故选 TypeScript/Node
> 以保证**可编译、可运行、可自检**；架构与 ADR 是语言无关的，模块边界与接口可直接平移到 Go。

## 快速开始（零依赖）

```bash
cd email-aggregator
node --experimental-strip-types src/demo.ts     # 无头自检，退出码 0 表示全部通过
node --experimental-strip-types src/main.ts      # 常驻服务：REST + WebSocket（:8080）
```

自检预期：初始全量 3 封 → 注入 1 封新邮件（模拟 IMAP IDLE 推送）→ 总数 4，
检索命中 ≥1，new-mail 推送 ≥1，且幂等无重复入库。

## 架构与目录

```
src/
  config.ts            全局配置（KMS/DB 连接串不硬编码）
  model/               领域模型
    canonical.ts       统一邮件模型 CanonicalMail / SyncCursor（协议无关）
    connector.ts       连接器契约 Connector / AccountCredential
  connector/           适配器层（ADR-005 适配器注册表）
    registry.ts        按 providerType 路由
    imap.ts            IMAP 连接器 + 内存邮件服务器（模拟 IDLE/QRESYNC）
  events/              事件总线（at-least-once + 幂等 + DLQ）
    contracts.ts       5 类事件契约（sync-tasks/mail-ingested/mail-index/notifications/audit）
    bus.ts             InMemoryEventBus + KafkaAdapter(stub)
  security/            KMS 凭据保险库（信封加密，ADR-004）
    vault.ts           AES-256-GCM + KEK 包裹 + 密钥轮换
  sync/                同步编排
    stateMachine.ts    7 态状态机（UNCONNECTED→…→PAUSED）
    orchestrator.ts    驱动单账户：授权/全量/增量/限流退避/游标持久化
  store/               存储分层
    metadata.ts        元数据（PG 适配器 stub，按 accountId 分片）
    content.ts         内容寻址对象存储（MinIO stub，去重）
  search/              检索索引（OpenSearch stub）
  notify/              WebSocket 实时推送（仅内置模块实现）
  ingest/              采集服务（消费 sync-tasks → 规范化 → 落库 → mail-ingested）
  api/                 BFF 网关（REST + WS 同端口）
  app.ts               装配 + 端到端场景
  main.ts / demo.ts    常驻入口 / 无头自检
```

## 事件驱动闭环

```
[IMAP IDLE] → Connector.subscribe(onExists)
   → Orchestrator.pollChanges → 发 sync-tasks(Kafka)
   → IngestService: fetchMessage → 规范化 CanonicalMail
       → 内容寻址落对象存储 + 元数据落 PG
       → 发 mail-ingested
           → SearchIndex 建索引 + 发 mail-index
           → Notifier 推 WebSocket(new-mail)
           → Audit 写审计
```

## 已覆盖的架构决策（ADR 映射）

- **ADR-001 演进式架构**：本 PoC 即「模块化单体」起步形态，模块边界清晰，后续按边界切微服务。
- **ADR-004 KMS 信封加密**：`security/vault.ts` 生成一次性 DEK 加密凭据、KEK 包裹 DEK、支持轮换。
- **ADR-005 适配器注册表**：`connector/registry.ts` + 统一 `Connector` 接口，新增协议只注册实现。
- **消息队列 at-least-once + 幂等**：`events/bus.ts` 以 `idempotencyKey` 去重，重复派发安全。
- **存储三权分立**：元数据(PG) / 内容(对象存储,内容寻址) / 索引(OpenSearch) 各司其职。

## 从 PoC 到生产（替换点）

| PoC 内存桩 | 生产替换 |
|---|---|
| `InMemoryEventBus` | `KafkaAdapter`（kafkajs，consumer-group 再均衡） |
| `InMemoryMetadataStore` | `PgMetadataStore`（按 account_id 分片 + Patroni HA） |
| `InMemoryContentStore` | `ObjectContentStore`（MinIO/S3，版本化+生命周期） |
| `InMemorySearchIndex` | `OpenSearchIndex`（按 accountId 路由索引） |
| `InMemoryKms` | 云 KMS / Vault Transit（KEK 不出域） |
| `InMemoryMailServer` | 真实 IMAP（IDLE+QRESYNC）/ POP3 / Exchange Graph / Gmail |

详见主架构文档 `邮箱聚合平台系统架构设计.md`（v1.2）与 `docs/邮箱聚合平台_架构深化设计.md`。
