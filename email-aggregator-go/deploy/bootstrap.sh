#!/usr/bin/env bash
# =============================================================================
# bootstrap.sh — 真实集成栈「一键就绪」脚本
# -----------------------------------------------------------------------------
# 职责（全部在容器内执行，本机无需安装 psql / mc / rpk / curl）：
#   1) docker compose up -d           拉起 PG / PgBouncer / MinIO / OpenSearch / Redpanda
#   2) 等 PG 健康 → 重放迁移 001 + 002（002 在纯 PG 上自动跳过 Citus 分片）
#   3) MinIO 建桶 agg-mail
#   4) OpenSearch 建索引模板 mail-*（按账户隔离）
#   5) Redpanda 建 Kafka 主题 sync-tasks / mail-ingested / mail-index / notifications / audit
#
# 用法：
#   cd deploy
#   cp .env.example .env          # 首次需先准备 .env
#   bash bootstrap.sh
#
# 前置：已安装 Docker + docker compose，且能访问 Docker Hub 拉取镜像（需要外网）。
# 本脚本只编排基础设施，不编译/运行 Go 集成服务；编译集成服务见仓库根 INTEGRATION.md。
# =============================================================================
set -u

# 加载同目录 .env（compose 变量 + 集成服务变量，供本脚本读取 PG_USER 等）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$SCRIPT_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$SCRIPT_DIR/.env"
  set +a
fi

# ── 颜色（无 tty 时降级为纯文本）──
if [ -t 1 ]; then
  C_B="\033[1;34m"; C_G="\033[1;32m"; C_Y="\033[1;33m"; C_R="\033[1;31m"; C_0="\033[0m"
else
  C_B=""; C_G=""; C_Y=""; C_R=""; C_0=""
fi
info()  { printf "${C_B}[bootstrap]${C_0} %s\n" "$1"; }
ok()    { printf "${C_G}[  ok  ]${C_0} %s\n" "$1"; }
warn()  { printf "${C_Y}[ warn ]${C_0} %s\n" "$1"; }
err()   { printf "${C_R}[error]${C_0} %s\n" "$1"; }

# ── 前置检查 ──
need_cmd() { command -v "$1" >/dev/null 2>&1 || { err "未找到命令：$1（请先安装 Docker / Docker Compose）"; exit 1; }; }
need_cmd docker
if docker compose version >/dev/null 2>&1; then COMPOSE="docker compose";
elif docker-compose version >/dev/null 2>&1; then COMPOSE="docker-compose";
else err "未找到 docker compose / docker-compose"; exit 1; fi

cd "$SCRIPT_DIR" || { err "无法进入 $SCRIPT_DIR"; exit 1; }

# ── 等待 TCP 端口可达（bash /dev/tcp，无需本机 curl）──
wait_port() {
  local host="$1" port="$2" tries="${3:-60}" i
  for ((i=1; i<=tries; i++)); do
    if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then
      exec 3>&- 3<&-
      return 0
    fi
    sleep 1
  done
  return 1
}

# 解析 .env 取变量（带默认值，与 compose / server_integration.go 对齐）
PG_USER="${PG_USER:-agg}"
PG_DATABASE="${PG_DATABASE:-mailagg}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-agg}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-agg-secret}"
MINIO_PORT="${MINIO_PORT:-9000}"
OPENSEARCH_PORT="${OPENSEARCH_PORT:-9200}"
OPENSEARCH_INITIAL_ADMIN_PASSWORD="${OPENSEARCH_INITIAL_ADMIN_PASSWORD:-Admin@123}"

# ── 1) 拉起基础设施 ──
info "启动基础设施（docker compose up -d）…"
if ! $COMPOSE up -d; then
  err "docker compose up -d 失败——请确认 Docker 已启动且能访问 Docker Hub 拉取镜像（需要外网）。"
  exit 1
fi
ok "容器已启动"

# ── 2) 等 PG 健康并重放迁移 ──
info "等待 PostgreSQL 就绪（pg_isready）…"
for i in $(seq 1 60); do
  if $COMPOSE exec -T postgres pg_isready -U "$PG_USER" >/dev/null 2>&1; then break; fi
  sleep 1
done
$COMPOSE exec -T postgres pg_isready -U "$PG_USER" >/dev/null 2>&1 \
  || { err "PostgreSQL 60s 内未就绪，请检查容器日志：$COMPOSE logs postgres"; exit 1; }
ok "PostgreSQL 就绪"

info "重放迁移 001_init.sql + 002_citus_sharding.sql + 003_read_flag.sql（幂等，可重跑）…"
# 002 在纯 PG 上会自动跳过 Citus 分片（DO $$ 守卫），不会报错。
# 003 为存量库补齐 mail_metadata.read 列（ADD COLUMN IF NOT EXISTS），新装库自动跳过。
$COMPOSE exec -T postgres psql -U "$PG_USER" -d "$PG_DATABASE" -v ON_ERROR_STOP=1 \
  -f /migrations/001_init.sql -f /migrations/002_citus_sharding.sql -f /migrations/003_read_flag.sql \
  && ok "迁移完成（mail_metadata / account_sync_cursor / read 列 已就绪）" \
  || { err "迁移执行失败，详见上方 psql 输出"; exit 1; }

# ── 3) MinIO 建桶 ──
info "等待 MinIO 端口（$MINIO_PORT）…"
wait_port localhost "$MINIO_PORT" 60 || { err "MinIO 端口 $MINIO_PORT 不可达"; exit 1; }
ok "MinIO 端口可达"

info "创建 MinIO 桶 agg-mail（docker compose run --rm mc，自动接入 compose 网络）…"
# 用 docker-compose.yml 中 profiles:[tools] 的 mc 工具服务，别名仅在该容器内有效，故链式执行。
if $COMPOSE run --rm --entrypoint sh mc -c "mc alias set local http://minio:9000 $MINIO_ROOT_USER $MINIO_ROOT_PASSWORD && mc mb --ignore-existing local/agg-mail" 2>&1 | sed 's/^/    /'; then
  ok "MinIO 桶 agg-mail 就绪（已存在则忽略）"
else
  warn "MinIO 桶创建失败，可手动执行："
  warn "  docker compose run --rm mc sh -c 'mc alias set local http://minio:9000 $MINIO_ROOT_USER $MINIO_ROOT_PASSWORD && mc mb --ignore-existing local/agg-mail'"
fi

# ── 4) OpenSearch 索引模板 ──
info "等待 OpenSearch 就绪（/_cluster/health）…"
# OpenSearch 启动较慢，且镜像默认在 REST 端口启用自签 TLS，故用 https + -k 轮询集群健康端点。
for i in $(seq 1 90); do
  if $COMPOSE exec -i opensearch curl -fsS -u "admin:$OPENSEARCH_INITIAL_ADMIN_PASSWORD" -k \
       "https://localhost:9200/_cluster/health" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
$COMPOSE exec -i opensearch curl -fsS -u "admin:$OPENSEARCH_INITIAL_ADMIN_PASSWORD" -k \
  "https://localhost:9200/_cluster/health" >/dev/null 2>&1 \
  || { err "OpenSearch 90s 内未就绪，请检查容器日志：$COMPOSE logs opensearch"; exit 1; }
ok "OpenSearch 就绪"

TMP_JSON="$(mktemp)"
cat > "$TMP_JSON" <<'JSON'
{
  "index_patterns": ["mail-*"],
  "template": {
    "settings": { "number_of_shards": 1, "number_of_replicas": 0 },
    "mappings": {
      "properties": {
        "accountId":    { "type": "keyword" },
        "subject":      { "type": "text"   },
        "from":         { "type": "keyword" },
        "bodyText":     { "type": "text"   },
        "internalDate": { "type": "date"   }
      }
    }
  }
}
JSON
info "创建 OpenSearch 索引模板 mail-*（按账户隔离索引 mail-<accountId>）…"
# OpenSearch security 插件默认开启 → 需 -u admin:<pass> 且 -k（自签证书）。
# -i 保留 stdin 以读取宿主机临时 JSON 文件（@/dev/stdin）。
if $COMPOSE exec -i opensearch curl -fsS -u "admin:$OPENSEARCH_INITIAL_ADMIN_PASSWORD" -k \
     -X PUT "https://localhost:9200/_index_template/mail" \
     -H 'Content-Type: application/json' -d @/dev/stdin < "$TMP_JSON" 2>&1 | sed 's/^/    /'; then
  ok "OpenSearch 索引模板 mail-* 已就绪（已存在则覆盖更新）"
else
  warn "OpenSearch 索引模板创建失败（可能密码/网络问题），可手动执行 INTEGRATION.md §3.3"
fi
rm -f "$TMP_JSON"

# ── 5) Kafka 主题（apache/kafka，kafka-go 客户端协议兼容）──
info "等待 Kafka 端口（$KAFKA_PORT）…"
wait_port localhost "$KAFKA_PORT" 90 || { err "Kafka 端口 $KAFKA_PORT 不可达，请检查容器日志：$COMPOSE logs kafka"; exit 1; }
ok "Kafka 端口可达"

# 经 `compose exec` 在 broker 容器内创建主题：broker 广播 localhost:9092，
# 从容器内以 kafka:9092 连接后，元数据重定向到 localhost:9092（即 broker 自身），可正常建主题。
# 注意：一次性 `docker run` 容器连 kafka:9092 会被重定向到其自身的 localhost，导致连接超时
# （历史失败点），故不使用该方式。
info "创建 Kafka 主题（sync-tasks / mail-ingested / mail-index / notifications / audit）…"
for t in sync-tasks mail-ingested mail-index notifications audit; do
  $COMPOSE exec -T kafka /opt/kafka/bin/kafka-topics.sh \
    --bootstrap-server kafka:9092 --create --if-not-exists \
    --topic "$t" --partitions 1 --replication-factor 1 2>&1 | sed 's/^/    /' || true
done
ok "Kafka 主题就绪（已存在则忽略；已开启 auto-create 作为兜底）"

# ── 完成 ──
echo
ok "基础设施就绪度完成 ✅"
info "下一步（需具备 Go 1.22+ 与网络以拉取集成依赖）："
info "  cd .. && go build -tags integration ./... && go run -tags integration ./cmd/server_integration.go"
info "  服务监听 :8080（REST + WS /ws），前端连 ws(s)://host/ws?accountId=<账户>"
info "  本机 Docker 守护进程已运行、镜像经 docker.m.daocloud.io 源可拉取——本脚本已实跑验证通过（见下方日志）。"
info "  集成编译本机可完成：用 goproxy.cn 镜像 + 项目内可写 GOMODCACHE（规避 C:\\Users\\addk1\\go 权限拒绝）即可编译："
info "    cd .. && GOPROXY=https://goproxy.cn,direct \\"
info "    GOPATH=\$PWD/.gopath GOMODCACHE=\$PWD/.gopath/pkg/mod GOCACHE=\$PWD/.gocache \\"
info "    go build -tags integration -o integ_server.exe ./cmd/"
info "  服务运行在【宿主机】时须用 127.0.0.1:<已发布端口>（PG 15432 / MinIO 9000 / OpenSearch 9200 / Kafka 9092），"
info "  且 KAFKA_BROKERS=127.0.0.1:9092（broker 已改为广播 localhost:9092，确保宿主客户端可重定向）。详见 INTEGRATION.md §5 与 §7。"
