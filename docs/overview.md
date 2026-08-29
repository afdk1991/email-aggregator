# 概览：邮箱聚合平台系统架构设计（v1.6）

## 交付物
- **主文档**：`邮箱聚合平台系统架构设计.md`（v1.6，17 章 + 10 ADR + 术语表）
- **深化文档（已折叠）**：原 `docs/邮箱聚合平台_架构深化设计.md` 的四方向内容已并入主文档第 13–16 章。
- **Phase 0 PoC 代码**：`email-aggregator/`（TypeScript/Node，BFF/前端/对照，零外部依赖可运行，自检 5/5 通过）；`email-aggregator-go/`（**Go 主干生产版骨架**，接口契约与 TS 版一一对齐：model / events / security / syncsvc / connector / store / search / notify / **ingest（IngestWorker 事件驱动摄取消费侧，已落地）** / **aigateway（ADR-010 实现：AIProvider 接口 + 路由策略 Decide + Guardrail 脱敏 + 单测，零依赖）** / api + cmd/demo（已重写串联端到端闭环）+ `src/ingest/worker_test.go`（InMemory 断言 5 项）+ `src/aigateway/router_test.go`（6 项断言）；并附**真实集成骨架** `src/integration/` + `INTEGRATION.md`——Kafka(PG/MinIO/OpenSearch/WS) 适配器以 `//go:build integration` 保护，默认构建零依赖。**本机已隔离安装 Go 1.26.6，基准 `go build ./...` / `go vet ./...` / `go test ./...`（含 aigateway 6/6、ingest 5/5）全绿、`go run ./cmd/demo.go` 端到端闭环通过；集成构建 `go build -tags integration ./...` 亦通过（RC=0）。运行时连通已本机真实验证（详见 `../.workbuddy/analysis/问题修复总结.md`）：`docker compose up -d` 起 PG/MinIO/OpenSearch/Kafka 真实栈后，`go run -tags integration ./cmd/server_integration.go`（宿主地址 `127.0.0.1:<端口>`）监听 `:8080`，REST + WebSocket 实时推送 + Kafka→PG/MinIO/OpenSearch 事件驱动摄取管线全链路本机端到端跑通；React 前端 `npm run build` 通过、dev 代理连通后端；TS 参考实现自检 5/5 PASS**。
- **架构图（对话内可视化）**：整体分层架构图 / 多可用区部署拓扑 / 分阶段里程碑时间线 / 新邮件同步时序图 / 容量规划计算器 / 多租户隔离与数据驻留图（ADR-009）/ **AI 能力路由架构图（ADR-010）**。

## 主文档章节（v1.2）
| 章 | 内容 |
|----|------|
| 0 | 阅读指引（含 13–16 章映射） |
| 1–2 | 业务背景/NFR、设计原则；ADR-001~005 |
| 3 | C4 上下文与八层分层架构 |
| 4 | 六大核心模块（接入/同步/存储/索引/权限/API） |
| 5 | 微服务划分与分布式部署 |
| 6 | 水平扩展与负载均衡（专章） |
| 7 | 高可用与容灾（RPO/RTO） |
| 8 | 第三方邮箱接入适配（IMAP/POP3/Exchange/Gmail） |
| 9 | 技术演进路线与容量规划 |
| 10 | 监控与运维体系（可观测性） |
| 11 | 分阶段里程碑（Phase 0→4） |
| 12 | 风险、权衡与开放问题 |
| 13 | **模块接口与时序设计（深化）**：Connector/CanonicalMail 契约、7 态状态机、Kafka 5 事件契约、IDLE 增量时序 |
| 14 | **容量规划计算模型（深化）**：参数/公式/三档场景/扩展杠杆 |
| 15 | **Phase 0 PoC 落地计划（深化 + 已交付代码）**：选型、58 人天拆解、验收标准、目录结构、运行验证 |
| 16 | **HA/DR 与备份深化**：Patroni、跨区复制矩阵、PITR、混沌演练、降级模式 |
| 17 | **AI 能力层设计（深化，ADR-010 实施）**：AIGateway 边界上下文、AIProvider 统一接口、路由策略表、Guardrail 脱敏、与 audit/tenant 上下文集成、Go PoC 落地 |

## 已交付 PoC（email-aggregator/）
- 端到端闭环：授权 → IMAP 增量同步 → 落库(内容寻址)/落对象存储 → 检索 → WebSocket 推送。
- 自检 `src/demo.ts`：**5/5 PASS**（初始全量 3 封、注入增量 1 封、检索命中、new-mail 推送、幂等无重复）。
- REST + WebSocket（`src/main.ts`，已实测 `:8080` 返回正确数据）。
- 真实基础设施以接口 + 适配器 stub 预留（PG/MinIO/OpenSearch/Kafka/KMS），连接器可平移到 Go/Java。

## 关键 ADR
- ADR-001 演进式架构（模块化单体 → 微服务）
- ADR-002 Kafka 为主总线（至少一次 + 幂等 + 可重放）
- ADR-003 存储三权分立（元数据/内容/索引）
- ADR-004 KMS 信封加密凭据保险库（AES-256-GCM + KEK 包裹 + 轮换）
- ADR-005 适配器注册表 + 统一邮件模型
- ADR-006 技术线选型：Go 主干（★ 已锁定；备选 Java21 / 全 Node/TS）
- ADR-007 有界多语言策略：Go 默认主干 + Node/TS 作 BFF/前端 + Java 作边界连接器例外区（须凭 ADR 入场，锁在 Connector/事件边界）
- ADR-008 触发 Java 边界连接器例外区：企业版 Exchange/EWS 正式以独立 Java 边界服务落地（实现 Connector 接口、仅经 Kafka 事件通信），复用 JVM 成熟企业集成库，不改变 Go 主干默认语言
- ADR-009 多租户隔离与数据驻留：逻辑多租户共享栈（按 account_id 分片/路由，tenant_id 全链路强制透传）+ 企业版专属栈/私有化（数据不出境）；同一二进制、部署形态参数化，租户隔离基座复用 ADR-003/004/005
- ADR-010 AI 能力策略（能力路由网关 · 混合后端 · 租户感知）：**Accepted**。统一 `AIGateway`（`AIProvider` 接口，复用 ADR-005 适配器注册表范式）按 `tenant_id` + 数据敏感度选路——企业/私有租户强制自托管不出境、公共 SaaS 租户走第三方 API 须显式同意 + PII 边界脱敏 + audit 审计；边界 Guardrail（脱敏/注入防御）+ 质量评估集。

### ADR-009 落地状态（数据面核心骨架 · 2026-08-18）

**形态选择**：核心骨架（非「仅文档」、非「完整隔离（多轮）」）。即请求 → 存储 → 检索 → 通知 **tenant_id 全链路强制透传**，单节点以 `default` 租户运行；隔离基座复用 ADR-003/004/005，接口签名保持「加法」扩展（新增 tenantID 首参），不破坏既有 PoC 行为。

**已落地（Go 主干 `email-aggregator-go/`）**：
- 新增 `src/tenant` 包：零依赖基座。`DefaultTenantID="default"`、`HeaderTenantID="X-Tenant-Id"`、`FromRequest(r)` 解析（缺省回退 default）、`Resolve(id)` 空值归一。
- 领域锚点：`model.CanonicalMail.TenantID`、`notify.NotificationPayload.TenantID` 随实体/载荷全链路携带。
- 数据面接口显式透传：`store.MetadataStore`/`CursorStore` 所有方法首参加 `tenantID`（内存实现按 `tenant\x00key` 命名空间隔离；PG 实现加 `tenant_id` 列 + WHERE 过滤）；`search.SearchIndex` 的 `Index/Search/Remove` 携带 `tenantID`（内存按 `tenant\x00id`、OpenSearch 索引名 `mail-<tenant>-<account>`）；`notify.Notifier.Publish(tenantID, accountID, payload)` 写入载荷。
- API 层：`src/api/server.go` 各 handler 经 `tenant.FromRequest(r)` 解析租户并透传；`/api/mails`、`/api/mails/{id}`、`/api/mails/{id}/read`、`/api/accounts`、`/api/search`、`/api/demo/push` 均按租户归属读写。
- 摄取侧（`src/ingest/worker.go`）：事件驱动摄取以 `default` 租户落库/索引/通知（单节点形态；事件契约补 `TenantID` 后改为从事件透传，接口不变）。
- 迁移：`deploy/migrations/001_init.sql` 加 `tenant_id` 列 + `(tenant_id, account_id)` 索引；`account_sync_cursor` 主键改为 `(tenant_id, account_id, folder)`。`002_citus_sharding.sql` 注明物理隔离时分布键可切 `tenant_id`。
- 验证：`go build ./...` / `go vet ./...` / `go test ./...` 全绿（含新增 `TestTenantIsolationViaAPI` HTTP 级隔离、`TestInMemoryMetadataStore_TenantIsolation`）；`go build -tags integration ./cmd` 亦通过（RC=0）。

**后续「完整隔离（多轮）」路线（骨架之上叠加，无需重写接口）**：① 事件契约（`MailIngestedEvent`/`NotificationEvent`）补 `TenantID`，摄取侧改为从事件透传；② `tenant_id` 升级为 Citus 分布键实现物理共置；③ 企业/私有租户独立二进制 + 数据驻留（数据不出境），与 ADR-010 自托管路由协同。

## 开放问题（待决策）
- ~~是否做私有化交付形态？企业版是否需要数据驻留（数据不出境）？~~ **已决议（ADR-009）**：支持公共 SaaS 共享栈 + 企业版专属栈/私有化三种形态，同一二进制、部署形态参数化，数据驻留边界由部署拓扑保证。
- ~~AI 能力自研还是接入第三方大模型（合规约束）？~~ **已决议（ADR-010）**：能力路由网关（混合策略）——按租户类别与数据敏感度选路，企业/私有租户强制自托管不出境，公共 SaaS 租户走第三方须显式同意 + PII 边界脱敏 + audit 审计。
- 语言策略：**Go 默认主干 + Node/TS 作 BFF/前端 + Java 边界连接器例外（ADR-006 + ADR-007 + ADR-008）**；多租户隔离与数据驻留经 **ADR-009** 落档（tenant_id 全链路强制透传）；AI 能力策略经 **ADR-010** 落档（AIGateway 混合后端 + 租户感知路由）。PoC 用 TypeScript 因工具链限制，核心模块已平移为 Go（接口契约不变），企业版 Exchange/EWS 经 ADR-008 触发 Java 例外区（独立边界服务）。
