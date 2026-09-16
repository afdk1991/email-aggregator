#!/usr/bin/env bash
# EdgeOne Makers 线上全接口验证（curl 驱动）
# 用法：bash shots/live-verify.sh "<带 eo_token 的基础 URL>"
#
# 四个必须绕开的坑（否则会得到一片假的 401/400）：
#   1. 预览网关把 eo_token 下发为 **HttpOnly** cookie。Node 的 fetch 拿不到 HttpOnly 项，
#      会导致后续请求全部 401 —— 必须用 curl 的 cookie jar。
#   2. token 在 query 上，path 必须拼在 query **之前**：$ORIGIN + path + "?" + query。
#      直接 "$BASE$path" 会把 "/" 插进 eo_time 的值里，同样全 401。
#   3. 建立会话（拿到 cookie）之后一律**不再带 query**：实测带 `?eo_token=` 的 POST
#      会让云函数拿不到 request body，接口回 400 "id and provider are required"。
#   4. 本环境 curl 无法把 cookie jar 写到含中文的绝对路径（MSYS 风格 /d/中文/... 会被
#      拒绝），表现为 jar 静默不生成、后续全 401。解决办法：cd 进脚本目录后全程相对路径。

set -u
RAW="${1:-}"
if [ -z "$RAW" ]; then echo "缺少 URL 参数"; exit 2; fi

ORIGIN="${RAW%%\?*}"
QUERY=""
case "$RAW" in *\?*) QUERY="${RAW#*\?}";; esac

# 本环境的 shell 可能缺少 `dirname`，故用 bash 内建展开求脚本目录，不依赖外部命令。
SCRIPT_PATH="${BASH_SOURCE[0]}"
SCRIPT_DIR="${SCRIPT_PATH%/*}"
[ "$SCRIPT_DIR" = "$SCRIPT_PATH" ] && SCRIPT_DIR="."
cd "$SCRIPT_DIR" || exit 3

JAR=".ck.txt"
CODEP=".code.txt"
RESP=".resp.txt"
WARM=".warm.txt"
rm -f "$JAR" "$CODEP" "$RESP" "$WARM"

build_url() { printf '%s%s' "$ORIGIN" "$1"; }

# 建立会话：唯一一次带 token 的请求，换取 eo_token / eo_time 两个 HttpOnly cookie。
curl -sL --max-time 30 -c "$JAR" -b "$JAR" "$ORIGIN/?$QUERY" -o "$WARM"

echo "=== 目标：$ORIGIN"
if [ ! -s "$JAR" ] || ! grep -q eo_token "$JAR" 2>/dev/null; then
  echo "!! 警告：未取得预览会话 cookie，后续请求将全部 401。请确认 URL 中的 eo_token 未过期。"
fi
echo

PASS=0; FAIL=0; FAILS=""
ck() {
  if [ "$2" -eq 0 ]; then echo "PASS  $1"; PASS=$((PASS+1))
  else echo "FAIL  $1${3:+ → $3}"; FAIL=$((FAIL+1)); FAILS="$FAILS
  - $1"; fi
}
code() { cat "$CODEP" 2>/dev/null || echo 0; }

# GET 类请求
req() {
  local path="$1"; shift
  local out
  out="$(curl -sL --max-time 30 -c "$JAR" -b "$JAR" -w '\n%{http_code}' "$@" "$(build_url "$path")")"
  printf '%s' "$out" | tail -n1 > "$CODEP"
  printf '%s' "$out" | sed '$d'
}

# 带 body 的请求（body 作为单个参数直传，不经 stdin）
post() {
  local path="$1" method="$2" payload="$3"
  local out
  out="$(curl -sL --max-time 30 -c "$JAR" -b "$JAR" \
    -X "$method" -H 'content-type: application/json' \
    --data-binary "$payload" -w '\n%{http_code}' "$(build_url "$path")")"
  printf '%s' "$out" | tail -n1 > "$CODEP"
  printf '%s' "$out" | sed '$d'
}

# ---------- 1. 静态资源 ----------
HTML="$(req /)"
ck "GET / 返回 200" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"

JS_PATH="$(printf '%s' "$HTML" | grep -o '/assets/index-[A-Za-z0-9_-]*\.js' | head -n1)"
CSS_PATH="$(printf '%s' "$HTML" | grep -o '/assets/index-[A-Za-z0-9_-]*\.css' | head -n1)"
ck "HTML 引用 JS 产物" $([ -n "$JS_PATH" ] && echo 0 || echo 1) "$JS_PATH"
ck "HTML 引用 CSS 产物" $([ -n "$CSS_PATH" ] && echo 0 || echo 1) "$CSS_PATH"
echo "      JS=$JS_PATH  CSS=$CSS_PATH"

if [ -n "$JS_PATH" ]; then
  JSB="$(req "$JS_PATH")"
  ck "JS 产物可获取" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"
  JS_LEN=$(printf '%s' "$JSB" | wc -c)
  ck "JS 产物体量正常(>10KB)" $([ "$JS_LEN" -gt 10000 ] && echo 0 || echo 1) "长度 $JS_LEN"
  # 生产包必须不含演示逻辑
  for kw in 模拟收信 demo-tenant "demo/push"; do
    case "$JSB" in
      *"$kw"*) ck "生产包不含 '$kw'" 1 "命中演示关键字";;
      *) ck "生产包不含 '$kw'" 0;;
    esac
  done
fi

if [ -n "$CSS_PATH" ]; then
  CSSB="$(req "$CSS_PATH")"
  ck "CSS 产物可获取" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"
  CSS_LEN=$(printf '%s' "$CSSB" | wc -c)
  echo "      CSS 长度 $CSS_LEN"
  case "$CSSB" in *'#667085'*) ck "无障碍修正已上线(--muted #667085)" 0;; *) ck "无障碍修正已上线(--muted #667085)" 1;; esac
  case "$CSSB" in *'height:24px'*) ck "badge 高度 24px(WCAG 2.5.8)" 0;; *) ck "badge 高度 24px(WCAG 2.5.8)" 1;; esac
  case "$CSSB" in *'@supports'*) ck "含 @supports 兼容兜底" 0;; *) ck "含 @supports 兼容兜底" 1;; esac
  case "$CSSB" in *'data-theme=dark'*|*"data-theme='dark'"*) ck "含暗色主题" 0;; *) ck "含暗色主题" 1;; esac
  case "$CSSB" in *'--sidebar-w'*) ck "含布局令牌" 0;; *) ck "含布局令牌" 1;; esac
fi

echo
# ---------- 2. health ----------
H="$(req /api/health)"
ck "GET /api/health → 200" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"
case "$H" in *'"ok":true'*) ck "health.ok === true" 0;; *) ck "health.ok === true" 1 "$H";; esac
case "$H" in *'"mode":"serverless-blob"'*) ck "Blob 强一致持久化生效" 0;; *) ck "Blob 强一致持久化生效" 1 "$H";; esac
case "$H" in *'"demo":false'*) ck "生产形态 demo=false" 0;; *) ck "生产形态 demo=false" 1 "$H";; esac
echo "      $H"

echo
# ---------- 3. 账户 CRUD ----------
TID="acc_verify_$(date +%s)"
CREATED="$(post /api/accounts POST "{\"id\":\"$TID\",\"provider\":\"imap\",\"email\":\"$TID@example.com\",\"displayName\":\"验证账户\"}")"
ck "POST /api/accounts → 201" $([ "$(code)" = "201" ] && echo 0 || echo 1) "实际 $(code) $(printf '%s' "$CREATED" | head -c 100)"

LIST="$(req /api/accounts)"
ck "GET /api/accounts → 200" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"
case "$LIST" in *"$TID"*) ck "新建账户出现在列表" 0;; *) ck "新建账户出现在列表" 1 "$(printf '%s' "$LIST" | head -c 150)";; esac

UPD="$(post "/api/accounts/$TID" PUT '{"displayName":"验证账户-已改"}')"
ck "PUT 部分更新 → 200" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"
case "$UPD" in *'"provider":"imap"'*) ck "PUT 保留 provider 字段" 0;; *) ck "PUT 保留 provider 字段" 1 "$UPD";; esac

DEL="$(req "/api/accounts/$TID" -X DELETE)"
ck "DELETE 账户 → 200" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"

AFTER="$(req /api/accounts)"
case "$AFTER" in *"$TID"*) ck "删除后账户不可见" 1 "账户仍存在";; *) ck "删除后账户不可见" 0;; esac

echo
# ---------- 4. 退役演示账户防护 ----------
BAN="$(post /api/accounts POST '{"id":"acc_demo3","provider":"imap","email":"x@example.com"}')"
ck "重建已下线演示账户被拒绝" $([ "$(code)" -ge 400 ] 2>/dev/null && echo 0 || echo 1) "实际 $(code)"
FINAL="$(req /api/accounts)"
case "$FINAL" in
  *acc_demo3*|*acc_demo32*|*acc_personal1*|*acc_personal11*|*acc_work1*|*acc_work11*)
    ck "线上无任何退役演示账户" 1 "发现退役账户";;
  *) ck "线上无任何退役演示账户" 0;;
esac

echo
# ---------- 5. 检索 ----------
S="$(req '/api/search?q=test')"
ck "GET /api/search → 200" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"
case "$S" in *'"hits"'*|*'"results"'*) ck "检索返回结果集合" 0;; *) ck "检索返回结果集合" 1 "$(printf '%s' "$S" | head -c 120)";; esac

echo
# ---------- 6. 生产形态演示接口必须关闭 ----------
req /api/demo/push -X POST >/dev/null
ck "生产形态 /api/demo/push → 404" $([ "$(code)" = "404" ] && echo 0 || echo 1) "实际 $(code)"

echo
# ---------- 7. AI 降级行为 ----------
AI="$(post /api/ai/chat POST '{"messages":[{"role":"user","content":"你好"}]}')"
AIC="$(code)"
ck "POST /api/ai/chat 未发生未捕获崩溃(非 500)" $([ "$AIC" != "500" ] && echo 0 || echo 1) "实际 $AIC"
case "$AI" in
  *'[demo]'*) ck "AI 不伪造模型输出" 1 "检测到 [demo] 伪造回复";;
  *) ck "AI 不伪造模型输出" 0;;
esac
echo "      AI 状态 $AIC：$(printf '%s' "$AI" | head -c 110)"

echo
# ---------- 8. 错误处理 ----------
req /api/__no_such_path__ >/dev/null
ck "未知 /api 路径 → 404" $([ "$(code)" = "404" ] && echo 0 || echo 1) "实际 $(code)"

echo
# ---------- 9. 邮件列表 ----------
M="$(req /api/mails)"
ck "GET /api/mails → 200" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"
case "$M" in *'"mails"'*|*'"items"'*) ck "邮件列表结构正确" 0;; *) ck "邮件列表结构正确" 1 "$(printf '%s' "$M" | head -c 120)";; esac

echo
# ---------- 10. SPA 深链回退 ----------
req /some/deep/link >/dev/null
ck "SPA 深链回退到 index" $([ "$(code)" = "200" ] && echo 0 || echo 1) "实际 $(code)"

rm -f "$JAR" "$CODEP" "$RESP" "$WARM"
echo
echo "结果（线上全接口）：$PASS 通过 / $FAIL 失败"
if [ "$FAIL" -gt 0 ]; then printf '失败项：%s\n' "$FAILS"; fi

# 不用 exit(1)：本环境下会让 shell 收到 SIGTERM 而截断输出。读最后一行判定即可。
true
