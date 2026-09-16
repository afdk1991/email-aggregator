#!/usr/bin/env bash
# EdgeOne Makers 线上全接口验证 —— **薄壳**，实现见 ../scripts/live-verify.mjs
#
# 用法：bash shots/live-verify.sh "<带 eo_token 的基础 URL>"
#
# ── 为什么只剩一层壳 ────────────────────────────────────────────────────────
# 本机（Windows + PortableGit）的 bash 是残缺 shim，连 `bash --version` 都返回 1
# （"The system cannot execute the specified program"），外部命令 dirname/head/printf
# 时有时无。原来 194 行的 bash 实现在这个平台上根本跑不起来，于是线上验证成了
# **唯一无法本地执行**的环节 —— 恰好它又是发布链路的最后一道门禁，必须能跑。
#
# 实现已移植为 Node（scripts/live-verify.mjs，本仓唯一跨平台无条件可用的运行时），
# 两份并存会漂移，故此处只做转发。CI 与发布器都直接调 .mjs，本文件仅为
# 「习惯敲 bash 的人」与历史文档链接保留。
#
# ── 四个必须绕开的坑（实现细节见 .mjs 头注释，此处留档）────────────────────
#   1. 预览网关把 eo_token 下发为 **HttpOnly** cookie；Node 的 fetch 拿不到 HttpOnly 项，
#      后续请求会全部 401 —— 必须用 curl 的 cookie jar。
#   2. token 在 query 上，path 必须拼在 query **之前**：$ORIGIN + path + "?" + query。
#      直接 "$BASE$path" 会把 "/" 插进 eo_time 的值里，同样全 401。
#   3. 建立会话（拿到 cookie）之后一律**不再带 query**：实测带 `?eo_token=` 的 POST
#      会让云函数拿不到 request body，接口回 400 "id and provider are required"。
#   4. curl 无法把 cookie jar 写到含中文的绝对路径（MSYS 风格 /d/中文/... 会被拒绝），
#      表现为 jar 静默不生成、后续全 401。解决办法：全程使用相对路径（.mjs 在临时目录
#      下建 jar，天然规避）。

set -u

if [ "${1:-}" = "" ]; then
  echo "缺少 URL 参数"
  echo "用法：bash shots/live-verify.sh \"<带 eo_token 的基础 URL>\""
  exit 2
fi

# 用 bash 内建展开求脚本目录，不依赖 dirname（本环境可能缺）
SCRIPT_PATH="${BASH_SOURCE[0]}"
SCRIPT_DIR="${SCRIPT_PATH%/*}"
[ "$SCRIPT_DIR" = "$SCRIPT_PATH" ] && SCRIPT_DIR="."
REPO_DIR="$SCRIPT_DIR/.."

exec node "$REPO_DIR/scripts/live-verify.mjs" "$@"
