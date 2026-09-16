#!/usr/bin/env bash
# ============================================================================
# run.sh — 数据库迁移执行脚本
# 用法：./run.sh [up|down|status]
# 默认：up（执行所有未应用的迁移）
# ----------------------------------------------------------------------------
# 连接信息统一由 PG_DSN 提供，并**直接交给 psql** —— psql 原生接受连接 URI /
# conninfo 串，会自行处理 host / port / db / user / password 与 URL 解码。
#
# 此前的实现用 sed 手工拆解 host/port/db/user：
#   1) 漏掉密码 → psql 转交互式提问 → 在 CI / 非 tty 环境下直接挂死；
#   2) 密码含 @ : / 或 URL 编码（%40）时解析结果错误。
# 两者都已由「交给 psql 解析」根治。
#
# 注意：CI 与生产 bootstrap 走的是 deploy/bootstrap.sh 的显式重放路径，
# 本脚本供人工运维使用；两条路径应用的是同一批 [0-9]*.sql 文件。
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

PG_DSN="${PG_DSN:-postgres://agg:agg@localhost:5432/agg}"

# 统一入口：选项在前、连接串在后，避免不同 getopt 实现的参数置换差异
run_psql() { psql -v ON_ERROR_STOP=1 "$@" "$PG_DSN"; }

CMD="${1:-up}"

case "$CMD" in
  up)
    echo "[migrate] applying migrations..."
    for f in "$SCRIPT_DIR"/[0-9]*.sql; do
      echo "  → $(basename "$f")"
      run_psql -f "$f"
    done
    echo "[migrate] done."
    ;;
  down)
    echo "[migrate] dropping tables..."
    run_psql -c "DROP TABLE IF EXISTS account_sync_cursor CASCADE;"
    run_psql -c "DROP TABLE IF EXISTS mail_metadata CASCADE;"
    echo "[migrate] done."
    ;;
  status)
    echo "[migrate] table status:"
    run_psql -c "\dt+ mail_metadata"
    run_psql -c "\dt+ account_sync_cursor"
    ;;
  *)
    echo "Usage: $0 [up|down|status]"
    exit 1
    ;;
esac
