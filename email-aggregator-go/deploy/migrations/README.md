# 数据库迁移脚本

## 文件说明

| 文件 | 说明 |
|------|------|
| `001_init.sql` | 初始 schema：`mail_metadata` + `account_sync_cursor` 表及索引 |
| `002_citus_sharding.sql` | Citus 分布式表（生产环境可选，开发/PoC 跳过） |
| `run.sh` | 迁移执行脚本（up/down/status） |

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
