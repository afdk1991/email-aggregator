# email-aggregator-go（Phase 0 PoC · Go 主干）

本目录是 **邮箱聚合平台** 的 **Go 主干生产版 PoC 骨架**，与 `../email-aggregator/`（TypeScript/Node 版，用作 BFF/前端/对照，见 ADR-007 有界多语言策略）接口契约**一一对齐**。

> **环境就绪**：本机已全局安装 Go 1.26.6（`C:\Program Files\Go`，系统级 PATH），基准 `go build`/`go test`/`go run` 均已验证通过。

## 技术线（ADR-006 + ADR-007）
- **Go 1.22+** = 默认后端主干（connector / sync / ingest / index / notify / auth / account）。
- **Node/TS** = BFF + 前端 + 工具（保留 `../email-aggregator/` 作对照）。
- **Java（例外区）** = 仅企业边界连接器（EWS/SSO/SCIM），须凭 ADR 入场、锁在 `Connector` 接口后。

## 目录结构
```
email-aggregator-go/
├─ go.mod
├─ src/
│  ├─ model/        # CanonicalMail / SyncCursor / Connector 接口（ADR-005 契约基座）
│  ├─ events/       # 5 类 Kafka 事件契约 + 总线（InMemory 实现；真实 KafkaAdapter 见 integration/）
│  ├─ security/     # KMS 信封加密凭据保险库（ADR-004）
│  ├─ syncsvc/      # 7 态同步状态机 + 编排器（驱动 Connector 采集 → 发布 mail-ingested）
│  └─ connector/    # 适配器注册表 + IMAP 适配器骨架（含内存服务器模拟，并发安全）
├─ src/
│  ├─ ...            # model / events / security / syncsvc / connector
│  ├─ store/         # 元数据/内容仓储（InMemory 实现；真实 PG/MinIO 见 integration/）
│  ├─ search/        # 检索索引（InMemory 实现 + OpenSearch 集成见 integration/）
│  ├─ notify/        # 通知（InMemory + WS 帧封装）
│  ├─ ingest/        # ★ 事件驱动摄取消费侧 IngestWorker（消费 mail-ingested → 落库+索引+通知+审计）
│  ├─ aigateway/     # ★ AI 能力路由网关（ADR-010：AIProvider + 路由策略 Decide + Guardrail 脱敏 + 单测）
│  └─ integration/   # ★ 真实适配器（构建标签 integration；Kafka/PG/MinIO/OS/WS）
├─ cmd/demo.go            # 端到端无依赖演示
├─ cmd/server_integration.go # 真实集成常驻服务（构建标签 integration）
├─ INTEGRATION.md          # 真实集成接入骨架指南
└─ deploy/                 # docker-compose（真实集成栈）
```

## 运行
> 本机已**全局安装 Go 1.26.6**（路径 `C:\Program Files\Go`，已写入系统级 Machine PATH）。
> 以下基准命令均可直接执行；集成构建需 Docker 起真实中间件栈（见 INTEGRATION.md）。
> TS 版（`../email-aggregator/`）可直接用 Node 22.6+ 运行，无需安装 Go。

```bash
# 端到端演示（零外部依赖）
go run ./cmd/demo.go

# 基准编译校验（仅标准库，零外部依赖）
go build ./...

# 单元测试（InMemory 实现，零依赖）
go test ./...

# 仅跑摄取消费管线单测（落库/索引/通知/mail-index 发布/幂等 五项断言）
go test ./src/ingest/... -run TestIngestWorker_FullPipeline -v

# 仅跑 AI 能力路由单测（私有强制自托管 / 公共PII未同意拒绝 / 第三方脱敏 / 审计 等六项断言）
go test ./src/aigateway/... -v
```

### 真实集成（替换 InMemory 实现）
详见 `INTEGRATION.md`。真实适配器用 `//go:build integration` 保护，默认构建不动它们：

```bash
# 1) 拉取依赖（需公网）
go get github.com/segmentio/kafka-go@latest github.com/jackc/pgx/v5@latest \
        github.com/opensearch-project/opensearch-go/v2@latest \
        github.com/minio/minio-go/v7@latest github.com/gorilla/websocket@latest

# 2) 集成构建 + 运行常驻服务（REST :8080 + WS /ws）
go build -tags integration ./...
go run -tags integration ./cmd/server_integration.go
```

> 注：本机已全局安装 Go 1.26.6（见上文「环境就绪」），上述命令可直接执行。
> 集成构建运行前需 Docker/WSL 起真实依赖栈方可实际连通。

**推荐的一键本地运行（自包含 SPA + REST + WS，已验证）**：本项目已封装 `deploy/start-local.ps1`，
自动拉起 WSL 内中间件栈（PG/MinIO/OpenSearch/Kafka）并启动单二进制 `bin/integ-webui.exe`
（`go build -tags "integration webui"`，同端口提供 SPA + REST + WS）：

```powershell
powershell -ExecutionPolicy Bypass -File .\deploy\start-local.ps1     # 默认 8082
powershell -ExecutionPolicy Bypass -File .\deploy\start-local.ps1 -Rebuild   # 强制重编
powershell -ExecutionPolicy Bypass -File .\deploy\stop-local.ps1      # 停止宿主服务
```
集成端到端验证证据见 `集成验证报告-20260828.md` 与根目录 `接管状态报告-20260829.md`。

## 与 TS 版的关系
| 层 | TS 版（BFF/对照） | Go 版（主干） |
|----|------------------|--------------|
| Connector 契约 | `src/model/connector.ts` | `src/model/connector.go` |
| 7 态状态机 | `src/sync/stateMachine.ts` | `src/syncsvc/statemachine.go` |
| 事件契约 | `src/events/contracts.ts` | `src/events/contracts.go` |
| KMS 信封 | `src/security/vault.ts` | `src/security/vault.go` |

真实基础设施（PG / MinIO / OpenSearch / Kafka / KMS）在 Go 版中以**接口 + InMemory 实现**提供，真实适配器集中在 `src/integration/`（`//go:build integration`），落地时替换装配即可。

## 下一步
- **IngestWorker 已落地（事件驱动摄取消费侧）**：`src/ingest/worker.go` 订阅 `mail-ingested`，
  对每封邮件完成 ① 元数据幂等落库（MetadataStore）② 正文内容寻址（ContentStore 去重）
  ③ 字段/全文索引（SearchIndex）④ 发布 `mail-index` ⑤ 实时通知 `new-mail`（Notifier/WS）
  ⑥ 审计 `audit`。`cmd/demo.go` 已重写串联**端到端闭环**（注册表→IMAP→7 态→KMS→编排→采集→
  IngestWorker→检索验证），`src/ingest/worker_test.go` 以 InMemory 实现断言 5 项（落库/索引/
  通知/mail-index/幂等）。`src/events/contracts.go` 的 `MailIngestedEvent` 已补 `From/Subject/
  BodyText`（omitempty，向后兼容），`src/syncsvc/orchestrator.go` 的 `buildMailIngested` 同步补齐。
- **编排器已修复（闭环采集）**：`src/syncsvc/orchestrator.go` 的 `HandleSyncTask` 现在完整驱动
  UNCONNECTED→AUTHORIZING→INITIAL_FULL→INCREMENTAL 全链路：调用 `Connector.Connect` 建立连接、
  `InitialFullSync`/`IncrementalSync` 执行采集、内置 `busSink` 将邮件转为 `mail-ingested` 事件并发布到总线。
  `cmd/demo.go` 已更新为编排器驱动完整闭环（移除手动 `demoSink`）。
  IngestWorker 自动消费 `mail-ingested` 完成落库/索引/通知/审计。
- **真实编译验证（已通过）**：本机全局安装 Go 1.26.6 后执行验证：
  `go build ./...`、`go vet ./...`、**全绿**（9 个包 `ok`，aigateway/ingest/syncsvc 等单测通过）；
  `go run ./cmd/demo.go` 端到端跑通——编排器驱动 UNCONNECTED→AUTHORIZING→INITIAL_FULL→INCREMENTAL，
  经内置 `busSink` 发布 3 封 `mail-ingested`，IngestWorker 自动消费并完成落库/索引/通知/审计。
  集成构建需 `go build -tags integration ./...` + Docker 真实栈（骨架见 `INTEGRATION.md`）。
- **AI 能力层已落地（ADR-010）**：`src/aigateway/` 实现 `AIProvider` 统一接口 + `Decide` 纯函数路由策略（租户类别×敏感度×同意→后端）+ `RegexRedactor` Guardrail 脱敏 + `Router.RouteAndChat`（路由+脱敏+audit）；详见主文档 §17。
- **ADR-008 评估**：企业版 Exchange（EWS）是否触发 Java 边界连接器例外区（已落档主文档 §2）。
- **与 TS 版对接联调**：Go 主干与 Node/TS BFF 经同一 Kafka 事件契约 / `Connector` 接口协作。
