# 真实集成接入骨架（Integration Skeleton）

本文件说明如何把 Phase 0 PoC（`email-aggregator-go/`）中的 **接口 + InMemory 实现**
替换为 **真实基础设施**（Kafka / PostgreSQL / MinIO / OpenSearch / WebSocket），
完成"接真实中间件跑集成联调"这一步（用户指令 ③）。

---

## 1. 设计原则：默认构建零依赖

仓库默认 `go build ./...` / `go test ./...` **只含标准库**，因此本开发机（~~无 Go 工具链、无外网拉包~~ **更新（2026-08-29）：已装 Go 1.26.6，依赖经 goproxy.cn 镜像可拉取**）也能保持 `go.mod` 干净、可自检。

真实适配器集中在 `src/integration/integration.go` 与 `cmd/server_integration.go`，
两者均用构建标签保护：

```go
//go:build integration
```

只有显式 `go build -tags integration ./...` 时才会编译，并需要下列外部依赖。
这样 **基准验证（无依赖）与集成验证（有依赖）互不干扰**。

---

## 2. 依赖与获取

在有 Go 1.22+ 且能访问公网的环境一次性拉取：

```bash
cd email-aggregator-go
go get github.com/segmentio/kafka-go@latest \
        github.com/jackc/pgx/v5@latest \
        github.com/opensearch-project/opensearch-go/v2@latest \
        github.com/minio/minio-go/v7@latest \
        github.com/gorilla/websocket@latest
```

| 能力 | 库 | 适配器 |
|------|----|--------|
| 事件总线 | `segmentio/kafka-go` | `integration.KafkaAdapter` → `events.EventBus` |
| 元数据存储 | `jackc/pgx/v5` | `integration.PgMetadataStore` → `store.MetadataStore` + `store.CursorStore` |
| 内容存储 | `minio/minio-go/v7` | `integration.ObjectContentStore` → `store.ContentStore` |
| 检索索引 | `opensearch-project/opensearch-go/v2` | `integration.OpenSearchIndex` → `search.SearchIndex` |
| 实时推送 | `gorilla/websocket` | `integration.WsHub` → `notify.Notifier` |

> 注意：所有真实适配器都实现与 InMemory 版本**完全相同的接口**，因此替换编排 /
> 采集 / API 逻辑时 **零改动**——只需在装配处换实现（见 §6）。

---

## 3. 基础设施（docker-compose 已就绪）

`deploy/docker-compose.yml` 已提供：PostgreSQL / MinIO / OpenSearch / **apache/kafka**（KRaft 单节点）。
（说明：参考/生产设计原用 Redpanda，但本机 Docker 镜像源 `docker.m.daocloud.io` 对
`redpandadata/redpanda` 与 `edoburu/pgbouncer` 等镜像均返回 403，无法拉取，故一键栈改用
`apache/kafka:latest`——`kafka-go` 客户端协议兼容，集成服务零改动；生产可换回 Redpanda。）
环境变量与集成服务配置统一收敛在 `deploy/.env.example`（复制为 `.env` 即同时驱动 compose 与集成服务，二者已对齐：`PG_USER/PG_PASSWORD/PG_DATABASE` 起库，`PG_DSN` 连同一库）。

**一键初始化（推荐）**：

```bash
cd deploy
cp .env.example .env
bash bootstrap.sh     # 拉起容器 + 重放迁移 + 建 MinIO 桶 + 建 OpenSearch 索引模板 + 建 Kafka 主题
```

`bootstrap.sh` 在容器内执行全部初始化（本机无需安装 `psql`/`mc`/`rpk`/`curl`），各步骤幂等可重放；迁移挂在 `/migrations:ro`，由脚本显式重放（不依赖 PG 入口自动执行，避免 `run.sh`/Citus 误触发）。

如需分步手动初始化，步骤如下：

### 3.1 PostgreSQL DDL

> 独立迁移脚本已就绪：`deploy/migrations/001_init.sql`（初始 schema）+ `002_citus_sharding.sql`（Citus 分布式表）。
> 可通过 `bash deploy/migrations/run.sh up` 一键执行，或与 docker-compose volume 映射自动初始化。

```sql
CREATE TABLE IF NOT EXISTS mail_metadata (
  id            TEXT PRIMARY KEY,
  account_id    TEXT NOT NULL,
  provider      TEXT,
  folder        TEXT,
  subject       TEXT,
  from_addr     TEXT,
  body_text     TEXT,
  internal_date BIGINT,
  size_bytes    BIGINT,
  raw_object_key TEXT,
  cursor_json   JSONB
);
CREATE INDEX IF NOT EXISTS idx_mail_account_folder ON mail_metadata(account_id, folder);

CREATE TABLE IF NOT EXISTS account_sync_cursor (
  account_id TEXT,
  folder     TEXT,
  cursor_json JSONB,
  PRIMARY KEY (account_id, folder)
);
-- 分片（生产）：SELECT create_distributed_table('mail_metadata','account_id');
```

### 3.2 MinIO 桶

```bash
mc alias set local http://localhost:9000 agg agg-secret
mc mb --ignore-existing local/agg-mail
```

### 3.3 OpenSearch 索引模板（按账户隔离）

```bash
curl -X PUT http://localhost:9200/_index_template/mail \
  -H 'Content-Type: application/json' \
  -d '{"index_patterns":["mail-*"],"template":{"settings":{"number_of_shards":1}}}'
```

### 3.4 Kafka 主题（apache/kafka）

```bash
# 容器内 kafka-topics.sh（bootstrap.sh 已自动执行；手动等价命令）
docker compose exec kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --create --if-not-exists --topic sync-tasks --partitions 1 --replication-factor 1
# 其余主题：mail-ingested / mail-index / notifications / audit（同上）
# 亦可依赖 auto-create：compose 已设 KAFKA_AUTO_CREATE_TOPICS_ENABLE=true
```

> 若改回 Redpanda，将 compose 中 `kafka` 服务换为 `redpandadata/redpanda` 并把上面的
> `kafka-topics.sh` 换成 `rpk topic create sync-tasks mail-ingested mail-index notifications audit` 即可。

---

## 4. 构建与运行

```bash
# 集成构建（含真实适配器）
go build -tags integration ./...

# 跑集成常驻服务（REST :8080 + WS /ws）
go run -tags integration ./cmd/server_integration.go

# 仅基准单测（仍走 InMemory，零依赖）
go test ./...
```

---

## 5. 环境变量

| 变量 | 默认（对齐 docker-compose） | 说明 |
|------|------------------------------|------|
| `PG_DSN` | `postgres://agg:agg@postgres:5432/agg` | PG 连接串 |
| `MINIO_ENDPOINT` | `minio:9000` | 对象存储端点 |
| `MINIO_BUCKET` | `agg-mail` | 桶名 |
| `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` | `agg` / `agg-secret` | 凭据 |
| `MINIO_SECURE` | `false` | 是否 HTTPS |
| `OPENSEARCH_ADDR` | `https://opensearch:9200` | OpenSearch 地址（镜像默认 REST 端口启用自签 TLS，故为 https） |
| `OPENSEARCH_USER` / `OPENSEARCH_PASS` | `admin` / `Kp3mQ9@vL2*rT7xA8` | OpenSearch 基础鉴权（security 插件启用后写入必带，否则 401） |
| `KAFKA_BROKERS` | `kafka:9092` | Kafka broker（本地用 apache/kafka 替代原 redpanda） |
| `HTTP_PORT` | `8080` | 监听端口 |

---

> **宿主机运行必读（关键）**：`cmd/server_integration.go` 运行在 **宿主机**（Bash 工具所在环境），**不在** compose 网络内，因此必须用宿主发布的端口地址，而非 compose 服务名（`postgres`/`minio`/`opensearch`/`kafka`）。表格"默认"列是 compose 网络内地址，仅当服务容器化部署进 compose 网络时才用。
>
> 宿主机运行请显式覆盖为：
> - `PG_DSN=postgres://agg:agg-secret@127.0.0.1:15432/mailagg`
> - `MINIO_ENDPOINT=127.0.0.1:9000`（`MINIO_SECURE=false`，明文 http）
> - `OPENSEARCH_ADDR=https://127.0.0.1:9200`（自签 TLS 仍需 `InsecureSkipVerify` + 带 `OPENSEARCH_USER`/`OPENSEARCH_PASS` 鉴权，否则 401）
> - `KAFKA_BROKERS=127.0.0.1:9092`
> - `KAFKA_DIAL_ADDR=127.0.0.1:9092` —— **宿主机场景必设**：Kafka advertised 通告的是容器内网名
>   `deploy-kafka-1`，宿主机无法解析。缺此变量会导致 `mail-ingested` 事件生产失败、摄取 worker
>   收不到事件、**邮件永不落库**（对外表现为「邮件正文一直为空」）。宿主机跑集成服务请同时设置两者。
>
> `deploy/docker-compose.yml` 已将 Kafka 改为 `PLAINTEXT://0.0.0.0:9092` 监听 + `PLAINTEXT://localhost:9092` 广播，使宿主 kafka-go 客户端 bootstrap 后被正确重定向（否则会拿到 `kafka:9092` 而解析失败）。
>
> **监听端口**：默认 `8080`，可用环境变量 `HTTP_PORT` 覆盖（`cmd/server_integration.go:93` 与
> `cmd/server/main.go:131` 均已接线）。注意**只认 `HTTP_PORT`，设置 `PORT` 无效**；
> 本机多项目并存时用 `HTTP_PORT=8090` 避开项目002 占用的 `8080`。
>
> *（本条原写作「监听端口硬编码为 `8080`（无 `HTTP_PORT` 读取）」，经核对代码与实际不符，已于 2026-09-16 更正。）*

## 6. 替换步骤（生产落地）

1. 在装配层（如 `cmd/server_integration.go`）以 `integration.Wire(ctx, cfg)` 取得真实适配器；
2. 将返回的 `Metadata / Content / Index / Bus / Notifier` 注入既有
   `Orchestrator` / `ApiServer` / 采集器——**接口签名不变，调用方零改动**；
3. 真实运行链路：
   - `sync-tasks` → 编排器推进 7 态状态机 → `Connector.Connect` → `InitialFullSync`/`IncrementalSync`
     → `busSink` 发布 `mail-ingested` → 落 `PgMetadataStore` 与 `ObjectContentStore`；
   - `mail-ingested` 消费者 → `OpenSearchIndex.Index` → 发布 `mail-index`；
   - 关键事件 → `notifications` 主题 → `WsHub` 实时推送给该账户 WS 订阅者。
4. 幂等与可重放：消费端一律以 `EventEnvelope.Key`（`accountId:mailId`）去重，
   与 TS 版契约一致。

---

## 7. 已知边界（诚实声明）

- 本文件、`server_integration.go` 与 `deploy/` 基础设施为 **参考骨架 + infra-as-code 就绪度补齐**。
  撰写时完成了接口一致性人工核查与配置对齐（`go.mod` 已固定集成依赖版本号），
  本开发机 **已实测通过**：Docker 守护进程运行中，`docker compose up -d` 拉起 PG/MinIO/OpenSearch/Kafka 并由 `bash bootstrap.sh` 一键初始化（迁移/桶/索引模板/5 主题全绿）；
  `go build -tags integration` 也已在本机突破——`GOPROXY=https://goproxy.cn,direct` + 项目内可写 `GOMODCACHE`/`GOPATH`/`GOCACHE`（`.gopath`/`.gocache`，规避 `C:\Users\addk1\go` 权限拒绝）即可编译；
  集成服务以 **宿主地址（127.0.0.1:<已发布端口>）** 运行并跑通端到端：REST（health/accounts/mails 返回 200）+ Kafka `mail-ingested` 驱动摄取管线落 PG/MinIO/OpenSearch 并发布 `mail-index`/`audit` 到 Kafka，OpenSearch 检索返回 200 命中（详见问题修复总结「integration 端到端真实验证」段）。
  唯一残留注意点：OpenSearch security 插件启用后写入必带 `OPENSEARCH_USER`/`OPENSEARCH_PASS` 鉴权；Kafka 须用宿主可达的 `127.0.0.1:9092`（broker 已广播 `localhost:9092`）。
- `deploy/bootstrap.sh` 已在代码层保证幂等：迁移 `IF NOT EXISTS`；`002_citus_sharding.sql`
  用 `DO $$` 守卫（无 Citus 扩展时静默跳过）；MinIO 桶 `--ignore-existing`；OpenSearch
  索引模板覆盖更新；Kafka 主题创建容忍已存在。因此可安全重跑。
- 真实环境还需补齐：PG 连接池参数 / 重试 / 超时；OpenSearch mapping 调优；
  Kafka 消费者 DLQ 与退避；MinIO 预签名上传替代服务端直存；WS 鉴权（JWT）。
- 这些点在主文档 §13（接口与时序）、§16（HA/DR）已有对应设计，落地时直接对齐。
