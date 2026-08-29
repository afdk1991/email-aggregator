# Go 主干 PoC · 编译/运行/测试验证报告

> 背景：此前本机无 Go 工具链，Go PoC 代码仅做了"接口签名静态自检"，从未真正编译运行（见 `docs/overview.md` 与 `邮箱聚合平台系统架构设计.md` 的诚实声明）。本次补齐工具链并真正编译、运行、测试，补全了缺失的验证环节与两处真实编译错误。
> 验证日期：2026-08-15 ｜ Go 工具链：go1.26.6（隔离安装于 `C:/Users/addk1/.workbuddy/binaries/go/go/bin`）

## 1. 此前未完成的工作项与上下文

| 项 | 状态（修复前） | 阻塞原因 |
|----|---------------|----------|
| Go 基准 PoC 编译 | ❌ 未编译 | 本机 `go: command not found` |
| Go demo 运行验证 | ❌ 未运行 | 无 Go 工具链 |
| Go 单元测试 | ❌ 未运行 | 无 Go 工具链 |
| Go 集成构建验证 | ❌ 未验证 | 无 Go 工具链 + 无外网拉包 |

原始目标（ADR-006/007）：Go 作为默认后端主干，接口契约与 TS 版 PoC 一一对齐；Phase 0 用 InMemory 桩，零外部依赖即可跑通端到端闭环。

## 2. 本次补全的实现细节

1. **安装 Go 1.22+ 工具链**：下载官方 go1.26.6 发行包并隔离解压（不污染用户系统 PATH/环境）。
2. **修复真实编译错误 ①**（`src/search/index.go`）：`joinAddrs` 入参应为单值 `model.Address`（`CanonicalMail.From` 是单值而非切片），原为 `[]model.Address` 导致类型不匹配。
3. **修复真实编译错误 ②**（`cmd/demo.go`）：`demo.go` 与 `server_integration.go` 同处 `package main` 且都声明 `main()`，`-tags integration` 构建时 `main` 重复声明。已给 `demo.go` 加 `//go:build !integration`，使两个入口互斥，文档运行路径保持不变。
4. **拉取集成依赖**：沙箱内 `proxy.golang.org` 仅解析到不可达 IPv6，改用 `GOPROXY=https://goproxy.cn,direct` 成功拉取 kafka-go / pgx-v5 / minio-go-v7 / opensearch-go-v2 / gorilla-websocket。

## 3. 验证结果（全部绿灯）

| 命令 | 结果 |
|------|------|
| `go build ./...`（基准） | ✅ RC=0 |
| `go vet ./...`（基准） | ✅ RC=0 |
| `go test ./...`（基准） | ✅ RC=0，**0 FAIL**（connector/events/ingest/security/store/syncsvc 全 ok） |
| `go run ./cmd/demo.go`（端到端） | ✅ 闭环：状态机→KMS 信封→全量采集→IngestWorker（落库/索引/WS推送/审计）→检索命中 invoice/sync |
| `go build -tags integration ./...` | ✅ RC=0 |
| `go vet -tags integration ./...` | ✅ RC=0 |

## 4. 运行方式（本机已验证可用）

```bash
# 基准（零依赖，全内存桩）
cd email-aggregator-go
go run ./cmd/demo.go

# 基准单测
go test ./...

# 真实集成（需先起 deploy/docker-compose.yml 的 PG/MinIO/OpenSearch/Redpanda）
go run -tags integration ./cmd/server_integration.go
```

## 5. 前置条件与限制（诚实声明）

- **Go 版本下限变化**：`go get` 拉取集成依赖时，因依赖要求，`go.mod` 的 `go` 指令由 `1.22` 自动升到 **`1.25.0`**。当前项目实际最低需 **Go 1.25+**（已用 1.26.6 验证）。纯基准用户如需退回 1.22，可移除 `require` 块并下调 `go` 指令，但集成构建将要求 1.25。
- **集成构建仅完成"编译验证"**：运行时仍需 Docker 起 Postgres/MinIO/OpenSearch/Redpanda 并配置环境变量（见 `deploy/.env` 与 `INTEGRATION.md`），本环境未实际连通这些服务。
- **设计目标生产栈（K8s/Citus/Patroni/多 AZ/多租户私有化）不在本验证范围**，那是架构文档的远期目标，非 Phase 0 PoC 要求。

## 结论

Go 主干 PoC 已从"未编译/不可运行"转为**已编译、已跑通 demo 全链路、已通过单元测试、集成构建亦可编译**。此前两处仅由大模型静态自检遗漏的真实编译错误已修复。代码逻辑完整、功能可用，满足 Phase 0 PoC 目标。
