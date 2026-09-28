#!/usr/bin/env bash
#
# CodeGate Web 前端真实渲染验证
#
# 用法：
#   bash scripts/web-render-check.sh              # 默认端口
#   PORT=18833 bash scripts/web-render-check.sh
#
# # 为什么需要这个脚本
#
# `server-smoke.sh` 用 curl 验证的是「HTTP 层对了」—— 状态码、Content-Type、
# 缓存头、资源可达。但**这些都证明不了页面真的能跑起来**：
#
#   - JS 有没有语法错误（构建成功不代表运行时不抛）
#   - Vue 有没有成功挂载（挂载失败会留下一片空白，但 HTTP 依然是 200）
#   - 路由守卫有没有把 /login 也挡掉（那会变成无限重定向）
#   - 样式有没有加载（首屏是一堆无样式文本，也能算"200"）
#
# 这些只有真浏览器能回答。所以这里用系统已装的 Chrome 无头模式：
# 加载页面 → 等 JS 跑完 → dump DOM → 检查**由 JS 注入的**节点是否存在。
#
# 检查 DOM 而不是截图：截图要靠人看，DOM 里出现 `id="email"` 这种
# 由 Vue 渲染出来的元素，是"框架跑通了"的硬证据。
#
# 全程临时目录 + 随机端口，不碰用户数据。

set -uo pipefail

cd "$(dirname "$0")/.."

BIN=./bin/codegate-server.exe
PORT="${PORT:-18822}"
BASE="http://127.0.0.1:${PORT}"

# 本机专属：访问 localhost 必须绕过那个坏代理，否则 502
CURL=(curl -s --noproxy '*')

CHROME=""
for cand in \
  "/c/Program Files/Google/Chrome/Application/chrome.exe" \
  "/c/Program Files (x86)/Google/Chrome/Application/chrome.exe" \
  "/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" \
  "${CHROME_PATH:-}"
do
  if [ -n "$cand" ] && [ -x "$cand" ]; then
    CHROME="$cand"
    break
  fi
done

if [ -z "$CHROME" ]; then
  echo "!! 找不到 Chrome/Edge，无法做渲染验证。用 CHROME_PATH=... 指定。" >&2
  exit 2
fi

if [ ! -x "$BIN" ]; then
  echo "!! 找不到 $BIN。先构建：go build -o $BIN ./cmd/codegate-server" >&2
  exit 1
fi

TMP="$(mktemp -d)"
SERVER_PID=""
FAILED=0

cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

pass() { printf '  [ OK ] %s\n' "$1"; }
fail() { printf '  [!!]  %s\n' "$1"; FAILED=$((FAILED + 1)); }

check() { # check <描述> <文件> <期望出现的串>
  if grep -q -- "$3" "$2" 2>/dev/null; then
    pass "$1"
  else
    fail "$1 —— DOM 里没找到「$3」"
  fi
}

# ---------------------------------------------------------------------------

export CODEGATE_DB_PATH="$TMP/codegate.db"
export CODEGATE_LISTEN="127.0.0.1:${PORT}"
export CODEGATE_JWT_SECRET="0123456789abcdef0123456789abcdef"
export CODEGATE_ALLOW_SIGNUP="true"
export CODEGATE_LOG_LEVEL="warn"

printf '\n=== 启动服务 ===\n'
"$BIN" serve > "$TMP/server.log" 2>&1 &
SERVER_PID=$!

ready=0
for _ in $(seq 1 60); do
  if "${CURL[@]}" -o /dev/null "$BASE/healthz" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.2
done
if [ "$ready" != "1" ]; then
  echo "!! 服务未就绪：" >&2
  cat "$TMP/server.log" >&2
  exit 1
fi
printf '  服务已就绪（pid=%s, port=%s）\n' "$SERVER_PID" "$PORT"

# ---- 前置：二进制里有没有完整的前端产物 ----
#
# 没有的话后面的断言全是假失败（页面是说明页，当然没有登录表单）。
# 跳过必须**响** —— 静默跳过会把「前端没构建」伪装成「渲染没问题」，
# 而渲染验证存在的意义正是抓「构建成功但页面跑不起来」。
INDEX_PROBE="$("${CURL[@]}" "$BASE/")"
if ! printf '%s' "$INDEX_PROBE" | grep -q 'id="app"'; then
  printf '\n'
  printf '  [跳过] 二进制里没有完整的前端产物，渲染验证无从进行。\n'
  printf '         先跑：make web && make server，然后重跑本脚本。\n'
  printf '         （当前 / 返回的是 webui 内置的说明页，不是前端外壳）\n'
  printf '\n渲染验证跳过。\n'
  exit 0
fi

# ---- 渲染登录页 ----
#
# --virtual-time-budget 让 Chrome 在"虚拟时间"里快进，
# 等 JS 跑完再 dump —— 否则 dump 到的是空壳。
# --user-data-dir 必须给绝对路径且每次换新目录，否则会复用旧 profile 卡住。
printf '\n=== 渲染 /login ===\n'
"$CHROME" \
  --headless=new --disable-gpu --no-sandbox \
  --user-data-dir="$TMP/cp-login" \
  --virtual-time-budget=10000 \
  --window-size=430,900 \
  --screenshot="$TMP/login.png" \
  --dump-dom "$BASE/login" > "$TMP/login.html" 2> "$TMP/login.err"

if [ ! -s "$TMP/login.html" ]; then
  fail "DOM 是空的（Chrome 没输出）"
  sed -n '1,20p' "$TMP/login.err" >&2
else
  # 这些节点全部由 Vue 渲染出来 —— 静态 HTML 里只有一个空的 #app。
  check "Vue 已挂载（出现了登录表单的邮箱输入框）" "$TMP/login.html" 'id="email"'
  check "密码输入框已渲染" "$TMP/login.html" 'id="password"'
  check "品牌标题已渲染" "$TMP/login.html" 'CodeGate'
  check "没有停在启动占位页" "$TMP/login.html" 'class="login'

  # 反向断言：如果 Vue 没挂载，#app 里会残留 index.html 里的启动占位。
  #
  # ⚠️ 必须匹配「元素上的 class 属性」（带引号），不能匹配裸词。
  # `--dump-dom` 会把 HTML 注释一起 dump 出来，注释里一旦出现那个裸词，
  # 断言就会匹配到自己的注释 —— 把"挂载成功"误判成"挂载失败"。
  # 这个坑真踩过一次，所以 web/index.html 的注释里也刻意不写该字面值。
  if grep -q 'class="boot-splash"' "$TMP/login.html"; then
    fail "页面还停在启动占位（占位元素没被替换）—— Vue 没挂载成功"
  else
    pass "启动占位已被替换（说明挂载完成）"
  fi

  # 控制台里的 JS 错误会出现在 stderr
  if grep -qiE 'Uncaught|SyntaxError|TypeError' "$TMP/login.err"; then
    fail "浏览器控制台有 JS 错误："
    grep -iE 'Uncaught|SyntaxError|TypeError' "$TMP/login.err" | head -5 >&2
  else
    pass "浏览器控制台没有 JS 错误"
  fi
fi

# ---- 渲染设备列表（需要登录，未登录应被守卫送回登录页）----
printf '\n=== 路由守卫：未登录访问 /devices ===\n'
"$CHROME" \
  --headless=new --disable-gpu --no-sandbox \
  --user-data-dir="$TMP/cp-guard" \
  --virtual-time-budget=10000 \
  --window-size=430,900 \
  --dump-dom "$BASE/devices" > "$TMP/guard.html" 2> /dev/null

if grep -q 'id="email"' "$TMP/guard.html"; then
  pass "被重定向到登录页（守卫生效）"
else
  fail "未登录访问 /devices 没有被拦到登录页"
fi

# ---------------------------------------------------------------------------

printf '\n'
if [ "$FAILED" -gt 0 ]; then
  printf '渲染验证失败：%d 项\n' "$FAILED"
  printf '（DOM 与截图留在 %s，脚本退出时会删）\n' "$TMP"
  echo "--- server.log ---"
  cat "$TMP/server.log"
  # 保留现场供排查
  cp "$TMP/login.html" /tmp/codegate-login-dom.html 2>/dev/null || true
  cp "$TMP/login.png" /tmp/codegate-login.png 2>/dev/null || true
  echo "已把 DOM 与截图复制到 /tmp/codegate-login-dom.html 与 /tmp/codegate-login.png"
  exit 1
fi
printf '渲染验证全部通过。\n'
