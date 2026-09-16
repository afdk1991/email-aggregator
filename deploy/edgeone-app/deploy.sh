#!/usr/bin/env bash
# ============================================================
#  一键重新部署「邮箱聚合平台」到 EdgeOne Makers（macOS / Linux）
# ============================================================
#
#  本脚本只是 scripts/deploy-edgeone.mjs 的薄壳。
#  发布逻辑**只有一处实现**（那个 Node 脚本），本地与 CI 走完全相同的代码路径。
#  历史教训：本项目曾同时存在 deploy.ps1 / deploy.sh / deploy-edgeone.yml 三份
#  发布实现，同步语义各不相同，导致两次"漏同步、线上跑旧包"。
#
#  发布器依次完成：
#    ① 版本门禁（version.json → 全部落点，漂移即中止）
#    ② 前端构建（npm run build）
#    ③ 产物同步（五处目标 + 逐文件 SHA-256 复验，并清理陈旧残留）
#    ④ 项目绑定（edgeone-project.json → .edgeone/project.json，防止 CLI 另建项目）
#    ⑤ 本地冒烟（演示形态 + 生产形态各一套）→ 部署
#    ⑥ 线上全接口验证（--verify-live）
#
#  用法：
#    ./deploy.sh                        # 全流程
#    ./deploy.sh --skip-build           # 复用已有 dist
#    ./deploy.sh --dry-run              # 演练，不真正部署
#    ./deploy.sh --verify-live          # 部署后跑线上验证
#    ./deploy.sh --project my-project   # 指定项目名（默认读绑定文件）
#    EDGEONE_TOKEN=xxx ./deploy.sh      # 无头环境用令牌部署
# ============================================================
set -euo pipefail

cd "$(dirname "$0")"
ROOT="$(cd ../../ && pwd)"

exec node "$ROOT/scripts/deploy-edgeone.mjs" "$@"
