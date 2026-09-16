# 数据库迁移脚本

## 文件说明

| 文件 | 说明 |
|------|------|
| `001_init.sql` | 初始 schema：`mail_metadata` + `account_sync_cursor` 表及索引 |
| `002_citus_sharding.sql` | Citus 分布式表（生产环境可选，开发/PoC 跳过） |
| `003_citus_rebalance.sql` | Citus 分片重平衡（Citus 专属，普通 PG 静默跳过） |
| `003_read_flag.sql` | 存量库补 `mail_metadata.read` 列（`ADD COLUMN IF NOT EXISTS`） |
| `004_account_registry.sql` | 账户注册表 `mail_account`（凭据只存 KMS 引用） |
| `005_account_server_host.sql` | 账户注册表补连接端点 `server_host` |
| `006_citus_workers.sql` | Phase 3 真实 Citus 集群 worker 注册（Citus 专属） |
| `run.sh` | 迁移执行脚本（up/down/status） |

## 迁移文件契约（新增迁移必须满足）

CI 会枚举本目录下所有 `[0-9]*.sql` 并**在纯 `postgres:16` 上逐个重放**，因此每个迁移必须：

1. **幂等** —— 重复执行无副作用：`CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` /
   `ADD COLUMN IF NOT EXISTS` / `ON CONFLICT DO NOTHING` / `DO $$` 守卫。
2. **纯 PG 安全** —— 不得无条件依赖 Citus 等扩展。需要扩展时用守卫包裹：

   ```sql
   DO $$
   BEGIN
     IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'citus') THEN
       CREATE EXTENSION IF NOT EXISTS citus;
       PERFORM create_distributed_table('mail_metadata', 'tenant_id');
     ELSE
       RAISE NOTICE 'citus extension not available — skipping (dev/PoC mode)';
     END IF;
   END
   $$;
   ```

   `002` / `003_citus_rebalance` / `006_citus_workers` 均已按此约定加守卫，故可在任意环境安全重放。

## 三条执行路径（同一批文件，切勿分叉）

| 路径 | 入口 | 用途 |
|------|------|------|
| 生产/本地集成栈 | `deploy/bootstrap.sh` | 容器内 `psql` 重放，一键拉起 PG/MinIO/OpenSearch/Kafka 并建桶/索引模板/主题 |
| **CI / 无 Docker 环境** | `email-aggregator/scripts/bootstrap-integration.mjs` | 直接用 `pg` / `minio` 客户端，**不需要 Docker**：重放迁移 + 建桶 + 落点自检 |
| 人工运维 | `migrations/run.sh up` | 连接串交给 `psql` 原生解析（见下） |

> ⚠️ 三条路径必须应用**同一批** `[0-9]*.sql`。历史上 CI 完全没有这一步，
> 导致 TS 集成层测试必然双 FAIL（`relation "mail_metadata" does not exist` /
> `The specified bucket does not exist`），而 Go 集成测试只覆盖 Kafka + OpenSearch，
> 长期掩盖了这个缺口。现已由 `bootstrap-integration.mjs` 补齐，并被 `ci.yml`
> 与 `scripts/ci.ps1 -Integration` 同时调用。

## 快速执行

```bash
# 1. 启动基础设施
cd deploy && docker compose up -d

# 2. 等待 PostgreSQL 就绪
docker compose exec postgres pg_isready -U agg

# 3. 执行迁移
cd migrations && bash run.sh up

# 4. 验证
bash run.sh status
```

`run.sh` 把 `PG_DSN` 原样交给 `psql` 解析（psql 原生接受连接 URI / conninfo 串）。
此前的实现用 `sed` 手工拆 host/port/db/user，**漏掉了密码** —— `psql` 会转交互式提问，
在 CI / 非 tty 环境下直接挂死；且密码含 `@` `:` 或 URL 编码（`%40`）时解析结果错误。
两种情形都已由「交给 psql 解析」根治。

## 与 Docker Compose 集成

迁移采用**显式 bootstrap** 而非 PG 入口自动执行，原因：

- 本目录含 `run.sh`（`.sh` 文件），若挂到 `/docker-entrypoint-initdb.d` 会被
  PG 入口脚本当作初始化脚本误执行；
- `002_citus_sharding.sql` 依赖 Citus 扩展，纯 `postgres:16` 开发环境不应自动跑。

因此 `docker-compose.yml` 将迁移以只读方式挂到 `/migrations`，由
`deploy/bootstrap.sh` 在 PostgreSQL 就绪后显式重放（`IF NOT EXISTS` 保证幂等）：

```bash
# 一键拉起基础设施 + 跑迁移 + 建桶/索引/主题
cd deploy && bash bootstrap.sh
```

等价手动步骤：

```yaml
# docker-compose.yml 中的挂载
postgres:
  volumes:
    - pgdata:/var/lib/postgresql/data
    - ./migrations:/migrations:ro   # 只读挂入，bootstrap 显式执行
```

```bash
# 等 PG 就绪后重放迁移（容器内有 psql，无需本机安装）
docker compose exec postgres psql -U "$PG_USER" -d "$PG_DATABASE" -v ON_ERROR_STOP=1 \
  -f /migrations/001_init.sql -f /migrations/002_citus_sharding.sql
```

> 说明：`002_citus_sharding.sql` 已用 `DO $$` 守卫——无 Citus 扩展时静默跳过，
> 因此可在任意环境安全重放，不会因 `CREATE EXTENSION citus` 失败而中断。

## 迁移工具集成

如需使用 golang-migrate 或 goose 管理迁移版本：

```bash
# golang-migrate
migrate -path migrations -database "$PG_DSN" up

# goose
goose -dir migrations postgres "$PG_DSN" up
```

当前脚本使用纯 `psql` 执行，无额外工具依赖，`IF NOT EXISTS` 保证幂等可重入。
