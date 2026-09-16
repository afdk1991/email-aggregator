#!/usr/bin/env bash
# 一键重新部署「邮箱聚合平台」到 EdgeOne Makers。
#
# 用法：
#   ./deploy.sh                       # 使用默认项目名
#   ./deploy.sh my-project-name       # 指定项目名
#   EDGEONE_TOKEN=xxx ./deploy.sh     # CI/无头环境用 token 部署
#
# 流程：构建前端 → 同步产物到部署根 → 本地冒烟测试 → 部署。
set -euo pipefail

cd "$(dirname "$0")"
ROOT="$(cd ../../ && pwd)"
PROJECT_NAME="${1:-email-aggregator-p003}"
export PAGES_SOURCE=skills

echo "==> [1/4] 构建前端（email-aggregator-web）"
( cd "$ROOT/email-aggregator-web" && npm run build )

echo "==> [2/4] 同步构建产物到部署根"
cp "$ROOT/email-aggregator-web/dist/index.html" ./index.html
rm -rf ./assets && mkdir -p ./assets
cp "$ROOT/email-aggregator-web/dist/assets/"* ./assets/
echo "    index.html + $(ls ./assets | wc -l | tr -d ' ') 个静态资源已就位"

echo "==> [2.5/4] 恢复项目绑定（防止 CLI 按名字另建项目）"
mkdir -p .edgeone
cp edgeone-project.json .edgeone/project.json
echo "    绑定 ProjectId: $(node -e "console.log(require('./edgeone-project.json').ProjectId)")"

echo "==> [3/4] 本地冒烟测试（演示形态 + 生产形态各一套）"
node smoke.mjs
node smoke.mjs --prod

echo "==> [4/4] 部署到 EdgeOne Makers"
if [ -n "${EDGEONE_TOKEN:-}" ]; then
  edgeone makers deploy -n "$PROJECT_NAME" -t "$EDGEONE_TOKEN" --json
else
  edgeone makers deploy -n "$PROJECT_NAME" --json
fi
