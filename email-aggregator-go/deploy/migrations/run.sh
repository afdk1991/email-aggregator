#!/usr/bin/env bash
# ============================================================================
# run.sh — 数据库迁移执行脚本
# 用法：./run.sh [up|down|status]
# 默认：up（执行所有未应用的迁移）
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 从 .env 或环境变量读取连接信息
PG_DSN="${PG_DSN:-postgres://agg:agg@localhost:5432/agg}"

# 提取 host/port/db/user 用于 psql
PGHOST=$(echo "$PG_DSN" | sed -n 's|.*@\([^:]*\):\([0-9]*\)/.*|\1|p')
PGPORT=$(echo "$PG_DSN" | sed -n 's|.*@\([^:]*\):\([0-9]*\)/.*|\2|p')
PGDB=$(echo "$PG_DSN"  | sed -n 's|.*/\([^?]*\).*|\1|p')
PGUSER=$(echo "$PG_DSN" | sed -n 's|.*://\([^:]*\):.*|\1|p')

export PGHOST PGPORT PGDB PGUSER

CMD="${1:-up}"

case "$CMD" in
  up)
    echo "[migrate] applying migrations..."
    for f in "$SCRIPT_DIR"/[0-9]*.sql; do
      echo "  → $(basename "$f")"
      psql -v ON_ERROR_STOP=1 -f "$f"
    done
    echo "[migrate] done."
    ;;
  down)
    echo "[migrate] dropping tables..."
    psql -v ON_ERROR_STOP=1 -c "DROP TABLE IF EXISTS account_sync_cursor CASCADE;"
    psql -v ON_ERROR_STOP=1 -c "DROP TABLE IF EXISTS mail_metadata CASCADE;"
    echo "[migrate] done."
    ;;
  status)
    echo "[migrate] table status:"
    psql -c "\dt+ mail_metadata"
    psql -c "\dt+ account_sync_cursor"
    ;;
  *)
    echo "Usage: $0 [up|down|status]"
    exit 1
    ;;
esac
