#!/bin/sh
# pg-replica-entrypoint.sh — Phase 3 Multi-AZ：PostgreSQL hot standby 副本初始化
#
# 流程：
#   1. 若 $PGDATA 为空（首次启动）：pg_basebackup 从 primary 拉全量 + 写 standby.signal + 配置连接
#   2. 启动 postgres，自动进入 hot standby 模式
#   3. 后续重启直接进入 hot standby（基于已存在 $PGDATA）
#
# 容器内执行：被 entrypoint 调用，需可读环境变量 PG_PRIMARY_* / POSTGRES_*。
set -e

: "${PGDATA:=/var/lib/postgresql/data}"
: "${POSTGRES_USER:=agg}"
: "${POSTGRES_DB:=mailagg}"
: "${PG_PRIMARY_HOST:=pg-primary}"
: "${PG_PRIMARY_USER:=replica}"
: "${PG_PRIMARY_PASSWORD:=replica-secret}"

# 首次启动：$PGDATA 为空（postgres entrypoint 会创建主节点数据；副本走 basebackup）
if [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
  echo "[replica] first start: pulling base backup from $PG_PRIMARY_HOST"
  mkdir -p "$PGDATA" && chmod 0700 "$PGDATA"
  PGPASSWORD="$PG_PRIMARY_PASSWORD" pg_basebackup \
    -h "$PG_PRIMARY_HOST" \
    -U "$PG_PRIMARY_USER" \
    -D "$PGDATA" \
    -P \
    -R \
    -X stream \
    -c fast
  # -R 自动生成 standby.signal + postgresql.auto.conf 含 primary_conninfo
  echo "[replica] base backup done; standby.signal written"
fi

# 启动 postgres（hot standby 自动开启）
echo "[replica] starting postgres in hot standby mode"
exec docker-entrypoint.sh postgres
