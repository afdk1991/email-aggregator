#!/usr/bin/env bash
# 跨平台发布流水线：在前端 SPA 内嵌的前提下，为 Windows / macOS / Linux
# 的 amd64 / arm64 交叉编译出自包含单文件二进制，并打包为 zip / tar.gz。
#
# 用法：
#   ./build.sh            # 全平台构建 + 打包到 ./release
#   ./build.sh windows    # 仅 Windows 全架构
#   ./build.sh linux amd64 # 仅 Linux/amd64
#
# 前置：Go 1.22+（交叉编译无需 CGO；本仓库零 C 依赖）、Node 18+（构建前端）。
set -euo pipefail

# 定位仓库根（脚本位于 email-aggregator-go/）
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WEB="$ROOT/../email-aggregator-web"
OUT="$ROOT/release"
GO_BIN="${GO_BIN:-go}"

# 目标矩阵：os|arch
TARGETS=(
  "windows|amd64"
  "windows|arm64"
  "darwin|amd64"
  "darwin|arm64"
  "linux|amd64"
  "linux|arm64"
)

# 过滤（若脚本带参数，仅保留匹配项）
if [ "$#" -gt 0 ]; then
  FILTERED=()
  for t in "${TARGETS[@]}"; do
    IFS='|' read -r os arch <<<"$t"
    for arg in "$@"; do
      if [ "$arg" = "$os" ] || [ "$arg" = "$arch" ]; then FILTERED+=("$t"); break; fi
    done
  done
  TARGETS=("${FILTERED[@]}")
fi

echo "==> [1/4] 构建前端 SPA ($WEB)"
cd "$WEB"
npm ci >/dev/null 2>&1 || npm install >/dev/null 2>&1
npm run build

echo "==> [2/4] 复制 dist 到后端 webroot/"
rm -rf "$ROOT/webroot/dist"
cp -r "$WEB/dist" "$ROOT/webroot/dist"

echo "==> [3/4] 交叉编译（CGO_ENABLED=0 -tags webui）"
cd "$ROOT"   # 回到模块根（步骤 [1/4] 曾 cd 到前端目录）
mkdir -p release
for t in "${TARGETS[@]}"; do
  IFS='|' read -r os arch <<<"$t"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  bin="email-aggregator-$os-$arch$ext"
  echo "    -> $bin"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" "$GO_BIN" build -tags webui -ldflags="-s -w" \
    -o "release/$bin" ./cmd/server
done

echo "==> [4/4] 打包归档"
cd release
# 先清空上轮残留归档：避免对已有 .zip/.tar.gz 二次打包（曾产生 .tar.gz.tar.gz 垃圾文件）
rm -f ./*.zip ./*.tar.gz
for f in email-aggregator-*; do
  [ -e "$f" ] || continue
  case "$f" in
    *.exe)
      # tar -a 按输出扩展名自动选用 zip 格式（不依赖 zip 二进制）
      tar -a -cf "${f%.exe}.zip" "$f" && echo "    -> ${f%.exe}.zip" ;;
    *.tar.gz|*.zip) continue ;;   # 跳过已生成的归档，绝不再嵌套打包
    *)
      tar -czf "${f}.tar.gz" "$f" && echo "    -> ${f}.tar.gz" ;;
  esac
done

echo "==> 完成。产物位于 $ROOT/release"
ls -lh
