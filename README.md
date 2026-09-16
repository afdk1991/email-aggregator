# 邮箱聚合平台（Email Aggregator）· 项目003

统一收件箱 SaaS：把多个邮箱账户（IMAP/后续 POP3/Exchange/Gmail）聚合为**单一全文检索入口**，
提供 REST / WebSocket / Web UI 三端，支持实时新邮件推送、租户级物理隔离、AI 邮件摘要（演示后端）。

> 当前状态：**Phase 0 MVP 已交付并真实联调验证**（2026-08-28/29）。三套实现 + 真实中间件全链路可运行。
> 六端工程化（Windows/macOS/Linux/Android/iOS/HarmonyOS）与统一版本号规范 **MALLV0.0.0** 已落地。

---

## 一、仓库结构

| 目录 / 文件 | 说明 |
|------|------|
| `email-aggregator/` | **TS PoC**：BFF/前端/对照实现（Node，demo/单测零外部依赖；真实集成按需装 pg/minio/kafkajs/undici） |
| `email-aggregator-go/` | **Go 主干（生产主选）**：领域模型 + InMemory 基准 + `-tags integration` 真实集成 + 自包含 SPA 单二进制 |
| `email-aggregator-web/` | **React 前端**（Vite+TS）：对接 Go REST/WS 契约的 Web UI，产物被六端复用 |
| `platforms/desktop/` | **桌面端**（Electron + electron-builder）：Windows/macOS/Linux PC 安装包 |
| `platforms/mobile/` | **移动端**（Capacitor）：Android APK/AAB、iOS xcarchive |
| `platforms/harmony/` | **鸿蒙端**（ArkTS + Web 容器）：HAP |
| `deploy/edgeone-app/` | **官网部署工作区**（EdgeOne Makers：静态 SPA + Node 云函数 + 项目绑定） |
| `deploy/container/` | 容器化完整功能版（Go 内嵌 SPA，含 WebSocket） |
| `scripts/` | 工具链：`version.mjs`（版本号）、`sync-web-assets.mjs`（产物同步）、`deploy-edgeone.mjs`（官网发布器）、`ci.ps1`（本地 CI） |
| `shots/` | UI 验证脚本：`responsive-check.mjs`、`a11y-check.mjs`、`modal-check.mjs`（线上验证已移至 `scripts/live-verify.mjs`） |
| `version.json` / `VERSION` | **版本号唯一权威源**与派生纯文本 |
| `.github/workflows/` | CI（三实现）、官网发布、六端发布 三条流水线 |
| `docs/` | 架构深化设计、overview 等配套文档 |

## 二、快速开始

### PoC（无外部依赖，任选一套）

```powershell
# TS（需 Node >=22.6，--experimental-strip-types）
node --experimental-strip-types email-aggregator/src/demo.ts
node --test --experimental-strip-types email-aggregator/tests/*.test.ts

# Go（需 Go 1.22+）
cd email-aggregator-go && go run ./cmd/demo.go
go test ./...
```

### 真实集成（中间件栈 + 单二进制）

```powershell
# 1) 一键启动（拉起中间件栈 + 启动自包含 SPA 服务）
cd email-aggregator-go
powershell -ExecutionPolicy Bypass -File .\deploy\start-local.ps1      # 默认 http://localhost:8082
# 2) 浏览器打开 http://localhost:8082（SPA + REST + WS 同端口）

# TS 集成层（同中间件栈，端口 8083）
cd email-aggregator && npm i   # 首次
node --experimental-strip-types src/main.integration.ts                # 需配置 PG_DSN/MINIO_*/OPENSEARCH_*/KAFKA_BROKERS/HTTP_PORT=8083
```

> 中间件（PG/MinIO/OpenSearch/Redpanda）跑在 **Docker Desktop**；宿主机侧联调需
> `KAFKA_DIAL_ADDR=127.0.0.1:9092`、`KMS_KEYRING_FILE=bin\kms-keyring.json`。
> 本机未安装 Docker 时，中间件相关的**运行时**验证不可做，仅可做编译级验证。

## 三、架构一览

- **统一邮件模型**：`CanonicalMail` / `SyncCursor`，多协议适配器映射到同一契约。
- **事件驱动**：Kafka `mail-ingested` 事件链（摄取 → 落库/对象存储 → 索引 → 实时推送），至少一次 + 幂等去重。
- **存储**：PG（元数据）· MinIO（内容寻址，Phase 1+ 按 `tenant-<tid>/` 物理隔离）· OpenSearch（`mail-<tid>` 单租户索引）。
- **实时**：WebSocket 订阅 `accountId`，新邮件即时 `new-mail` 帧。
- **多租户（ADR-009）**：`X-Tenant-Id` 头全链路透传，租户级数据/索引/对象物理隔离。
- **安全（ADR-003/004）**：内容加密 + 租户派生 KEK；输入清洗与输出脱敏（AI 面板 redaction）。

## 四、接口摘要（Go 集成服务 `:8082`）

| 端点 | 说明 |
|------|------|
| `GET /api/health` | 健康检查（含 `version` / `versionCode`） |
| `GET /api/accounts` | 账户列表 |
| `GET /api/mails?accountId=&folder=&limit=` | 邮件列表（PG） |
| `GET /api/mails/{id}`、`POST /api/mails/{id}/read` | 邮件详情 / 已读 |
| `GET /api/search?q=&accountId=` | 全文检索（OpenSearch） |
| `POST /api/ai/chat` | AI 摘要（演示后端，tenant/consent/redaction 生效） |
| `POST /api/demo/push` | 演示推送（触发 WS `new-mail`） |
| `WS /ws?accountId=` | 实时推送订阅 |

## 五、验证与 CI

- 一键本地 CI：`powershell -ExecutionPolicy Bypass -File .\scripts\ci.ps1 -Integration`
- GitHub Actions：`.github/workflows/ci.yml`（push/PR 触发，含版本一致性门禁）
- 真实联调：Go `email-aggregator-go\集成验证报告-20260828.md`；TS `email-aggregator\tests\integration_real_e2e.ts`（7/7 PASS）
- 契约等价性：`统一契约与双实现等价性验证.md`（TS/Go 领域模型·状态机·事件·API·安全对齐）
- 版本一致性：`node scripts/version.mjs verify`（21 个落点）
- 前端产物一致性：`node scripts/sync-web-assets.mjs --verify`（五处目标，逐文件 SHA-256）

## 六、部署与发布

**规范总纲：[`部署与发布流程规范.md`](./部署与发布流程规范.md)** —— 环节、触发条件、依赖配置与工具、
同步推送时的对应关系与约束。线上形态与排障见 [`DEPLOY.md`](./DEPLOY.md)；
六端产物与版本号命名细则见 [`多端部署与版本号规范.md`](./多端部署与版本号规范.md)。

```bash
# 官网发布（唯一入口，本地与 CI 同一份实现）
node scripts/deploy-edgeone.mjs                 # 版本门禁 → 构建 → 五处同步 → 冒烟 → 部署
node scripts/deploy-edgeone.mjs --verify-live   # 追加线上 36 项全接口验证（curl 换 cookie）

# 发版：递增版本 → 打标签 → 自动触发六端构建与 GitHub Release
node scripts/version.mjs bump patch
git tag MALLV0.0.1 && git push origin master --tags
```

| 通道 | 触发 | 说明 |
|------|------|------|
| CI 校验 | push / PR | 三套实现的构建、测试、类型检查 + 版本门禁 |
| 官网发布 | push `master`（命中 paths 白名单）/ 手动 | 自动上线并做线上验证 |
| 六端发布 | push `MALLV*` 标签 | 六平台产物 + GitHub Release |
| 本地一键 | 手动 | 同官网发布 |

## 七、文档索引

| 文档 | 内容 |
|------|------|
| `邮箱聚合平台系统架构设计.md` | 主架构设计 v1.10（18 章 + ADR-001~010） |
| `部署与发布流程规范.md` | **部署/发布/同步流程总纲**：环节、触发、配置、对应关系与约束 |
| `DEPLOY.md` | 线上形态（EdgeOne Makers）能力边界、环境变量、排障与回滚 |
| `多端部署与版本号规范.md` | 六端产物清单、MALLV 版本号规范与命名细则 |
| `邮箱聚合平台-全项目部署验证报告-20260916.md` | 三形态部署验证记录（36/36 + 12/12 标签 + 19/19 PoC） |
| `邮箱聚合平台-UI设计规范.md` · `邮箱聚合平台-UI重设计改动说明.md` | UI 设计系统与逐文件改动说明 |
| `docs/邮箱聚合平台_架构深化设计.md` · `docs/overview.md` | 模块接口/容量/HA-DR 深化；总览 |
| `本地运行可行性评估.md` | 本机环境前置与可行性实测 |
| `统一契约与双实现等价性验证.md` | TS/Go 双实现契约对齐 |
| `项目文档与文件索引-20260916.md` · `项目文档通读纪要-20260916.md` | 全仓库文件索引；通读汇总 |
| `email-aggregator-go/README.md` · `INTEGRATION.md` · `集成验证报告-20260828.md` | Go 主干运行/集成/验证 |
| `email-aggregator/README.md` · `TS_PoC_验证报告.md` | TS PoC |
| `email-aggregator-web/README.md` | 前端 |
| `接管状态报告-20260829.md` | 接管体检 + 三项跟进（git/TS联调/CI）执行记录 |

## 八、路线图

- **Phase 0（已完成）**：单 IMAP 闭环、双实现契约对齐、真实中间件联调、CI；
  六端工程化（Electron / Capacitor / ArkTS + 统一版本号规范 MALLV，Windows 安装包已实测出包）。
- **Phase 1（规划）**：多协议适配器（POP3/Exchange/Gmail）、事件化服务拆分、账户服务；TS 集成层真实联调对齐 Go 验证深度。
- **Phase 2+（远期）**：检索拆分/扩展、HA/DR、企业版私有化、多 AZ。
