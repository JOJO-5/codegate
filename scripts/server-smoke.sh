#!/usr/bin/env bash
#
# CodeGate Server 端到端冒烟测试
#
# 用法：
#   bash scripts/server-smoke.sh              # 用默认端口
#   PORT=18899 bash scripts/server-smoke.sh   # 指定端口
#
# # 这个脚本存在的理由
#
# 单元测试（internal/server）验证的是「每个 handler 的行为」，
# 它们全都跑在 httptest 里 —— 不经过真实监听端口、不经过真实的
# 进程启动路径、也不经过真实的信号处理。
#
# 而部署时会踩的坑恰好都在那一层：
#   - 配置从环境变量读进来了吗？
#   - 数据库目录建了吗？
#   - Ctrl+C 能优雅退出吗（还是卡住等 WS 连接）？
#   - 二进制真的能起来吗？
#
# 所以这个脚本**启动真进程、连真端口、发真信号**。
# 它跑得比单元测试慢，但它是唯一能证明「这套东西能部署」的东西。
#
# 全程使用临时目录与随机端口，不碰用户已有的数据库。

set -euo pipefail

cd "$(dirname "$0")/.."

BIN=./bin/codegate-server
PORT="${PORT:-18811}"
BASE="http://127.0.0.1:${PORT}"

# ---- 本机专属：curl 必须绕过坏代理 ----
#
# 环境变量里的 http_proxy 指向 127.0.0.1:<坏端口>，一律 502。
# 访问 localhost 必须显式 --noproxy '*'。
CURL=(curl -s --noproxy '*')

PY=""
for cand in \
  "${PYTHON:-}" \
  "C:/Users/JOJO/.workbuddy-ai/binaries/python/versions/3.13.12/python.exe" \
  "python3" "python"
do
  if [ -n "$cand" ] && command -v "$cand" >/dev/null 2>&1; then
    PY="$cand"
    break
  fi
done

if [ ! -x "$BIN" ] && [ ! -x "${BIN}.exe" ]; then
  echo "!! 找不到 $BIN，请先执行: make server" >&2
  exit 1
fi
if [ -x "${BIN}.exe" ]; then BIN="${BIN}.exe"; fi

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

# ---- 断言辅助 ----

# ok <描述> <实际> <期望>
ok() {
  local desc="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    printf '  [ OK ] %s\n' "$desc"
  else
    printf '  [!!]  %s —— 期望 %s，实际 %s\n' "$desc" "$want" "$got"
    FAILED=$((FAILED + 1))
  fi
}

# code <描述> <URL> <期望状态码> [额外 curl 参数...]
code() {
  local desc="$1" url="$2" want="$3"
  shift 3
  local got
  got=$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$@" "$url")
  ok "$desc" "$got" "$want"
}

json_field() {
  # 从 stdin 的 JSON 里取一个顶层字段。
  # 没有 python 时退化成 grep —— 只够用，但至少不让脚本整体失败。
  if [ -n "$PY" ]; then
    "$PY" -c "import sys,json;print(json.load(sys.stdin).get('$1',''))"
  else
    sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -1
  fi
}

section() { printf '\n=== %s ===\n' "$1"; }

# ---------------------------------------------------------------------------

export CODEGATE_DB_PATH="$TMP/codegate.db"
export CODEGATE_LISTEN="127.0.0.1:${PORT}"
export CODEGATE_JWT_SECRET="0123456789abcdef0123456789abcdef"
export CODEGATE_ALLOW_SIGNUP="true"
export CODEGATE_LOG_LEVEL="warn"

section "启动服务"
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
  echo "!! 服务未能在 12 秒内就绪，日志：" >&2
  cat "$TMP/server.log" >&2
  exit 1
fi
echo "  服务已就绪（pid=$SERVER_PID, port=$PORT）"

section "探针与版本"
ok "healthz 返回 ok" \
  "$("${CURL[@]}" "$BASE/healthz" | json_field status)" "ok"
ok "readyz 返回 ready" \
  "$("${CURL[@]}" "$BASE/readyz" | json_field status)" "ready"
ok "version 报告协议范围" \
  "$("${CURL[@]}" "$BASE/api/v1/version" | json_field version)" "dev"

section "认证"
REG="$("${CURL[@]}" -X POST "$BASE/api/v1/auth/register" \
  -H 'Content-Type: application/json' \
  -d '{"email":"smoke@example.com","password":"correct-horse-battery"}')"
TOKEN="$(printf '%s' "$REG" | json_field access_token)"

if [ -n "$TOKEN" ]; then
  echo "  [ OK ] 首个用户注册成功（引导逻辑）"
else
  echo "  [!!]  注册失败: $REG"
  FAILED=$((FAILED + 1))
fi

# ★ 响应体里绝不能出现 refresh_token —— 它只走 HttpOnly cookie。
if printf '%s' "$REG" | grep -q 'refresh_token'; then
  echo "  [!!]  注册响应泄露了 refresh_token"
  FAILED=$((FAILED + 1))
else
  echo "  [ OK ] 响应体不含 refresh_token"
fi

ok "GET /me 返回正确邮箱" \
  "$("${CURL[@]}" "$BASE/api/v1/me" -H "Authorization: Bearer $TOKEN" | json_field email)" \
  "smoke@example.com"

code "无 token 访问 /me 被拒" "$BASE/api/v1/me" "401"
code "伪造 token 被拒" "$BASE/api/v1/me" "401" -H "Authorization: Bearer garbage"

section "设备与票据"
ok "新账号设备列表为空" \
  "$("${CURL[@]}" "$BASE/api/v1/devices" -H "Authorization: Bearer $TOKEN")" \
  '{"devices":[]}'

TICKET="$("${CURL[@]}" -X POST "$BASE/api/v1/ws-ticket" \
  -H "Authorization: Bearer $TOKEN" | json_field ticket)"
if [ -n "$TICKET" ]; then
  echo "  [ OK ] 签发 WS 票据"
else
  echo "  [!!]  票据签发失败"
  FAILED=$((FAILED + 1))
fi

code "无票据连 WS 被拒" "$BASE/api/v1/ws/client" "401"

section "审计"
AUDIT="$("${CURL[@]}" "$BASE/api/v1/audit?limit=5" -H "Authorization: Bearer $TOKEN")"
if printf '%s' "$AUDIT" | grep -q 'user.register'; then
  echo "  [ OK ] 审计里有注册记录"
else
  echo "  [!!]  审计里没有注册记录: $AUDIT"
  FAILED=$((FAILED + 1))
fi

code "非法游标被拒" "$BASE/api/v1/audit?cursor=@@@@bad@@@@" "400" \
  -H "Authorization: Bearer $TOKEN"

section "Web 界面（P5）"
#
# 这一节验证的是「前端产物真的被 go:embed 进去了，而且服务方式正确」。
#
# 单测（internal/server/webui）测的是 handler 的逻辑，跑在 httptest 里 ——
# 它证明不了「这个二进制里到底嵌的是构建产物还是未构建的 dist」。
# 那恰好是最容易出错的一环：忘了跑 make web、或者构建产物没拷进去，
# 症状都是「网页打开只有一句话」，而 API 一切正常。
INDEX="$("${CURL[@]}" "$BASE/")"

# 先判定这个二进制里嵌的是不是完整产物。
#
# 未构建（dist/ 里只有 .gitkeep）是全新 clone 的正常状态，此时伺服的是
# webui 内置的说明页。那种情况下「资源可访问」这类断言没有对象可测。
#
# ★ 但跳过必须**响**：静默跳过等于把「前端根本没构建」伪装成「全部通过」，
#   而那正是这一节存在的意义。所以这里打一条明确的说明。
if printf '%s' "$INDEX" | grep -q 'id="app"'; then
  WEB_BUILT=1
  echo "  [ OK ] / 返回真正的前端产物（不是说明页）"
else
  WEB_BUILT=0
  echo "  [跳过] 二进制里没有完整的前端产物 —— 下面的资源断言无从测起"
  echo "         先跑：make web && make server，然后重跑本脚本"
  echo "         （当前 / 返回的是 webui 内置的说明页）"
fi

code "SPA 深链接回落（/devices）" "$BASE/devices" "200"
code "未知前端路由也回落" "$BASE/some/deep/unknown/route" "200"

if [ "$WEB_BUILT" = "1" ]; then
  # 从 index.html 里解析出真实资源路径，不硬编码哈希文件名 ——
  # 硬编码的话每次前端重新构建这个脚本就失效了。
  ASSET="$(printf '%s' "$INDEX" | sed -n 's#.*src="\(/assets/[^"]*\.js\)".*#\1#p' | head -1)"
  if [ -n "$ASSET" ]; then
    code "静态资源可访问（$ASSET）" "$BASE$ASSET" "200"
    if "${CURL[@]}" -D - -o /dev/null "$BASE$ASSET" | grep -qi 'cache-control:.*immutable'; then
      echo "  [ OK ] 带内容哈希的资源声明了 immutable"
    else
      echo "  [!!]  静态资源缺少 immutable 缓存头"
      FAILED=$((FAILED + 1))
    fi
  else
    echo "  [!!]  没能从 index.html 里解析出资源路径 —— 构建产物结构变了？"
    FAILED=$((FAILED + 1))
  fi
fi

# 外壳页是唯一没有内容哈希的产物，一旦被缓存，发新版后就是白屏。
# 这条在「已构建」和「未构建」（说明页）两种状态下都必须成立 ——
# 说明页被缓存的话，用户跑完 make web 刷新还是看到提示页。
if "${CURL[@]}" -D - -o /dev/null "$BASE/" | grep -qi 'cache-control:.*no-cache'; then
  echo "  [ OK ] 外壳页声明了 no-cache"
else
  echo "  [!!]  外壳页没有 no-cache —— 发新版后会白屏"
  FAILED=$((FAILED + 1))
fi

# ★ 未知 API 绝不能回落成 HTML。
#   回落的话前端会拿到 200 + HTML，然后在 JSON.parse 处报错 ——
#   报错位置离真正原因（路径写错）很远。
API404="$("${CURL[@]}" "$BASE/api/v1/definitely-not-a-route")"
if printf '%s' "$API404" | grep -q '"code":"not_found"'; then
  echo "  [ OK ] 未知 API 返回 404 JSON"
else
  echo "  [!!]  未知 API 响应不对: $API404"
  FAILED=$((FAILED + 1))
fi
if printf '%s' "$API404" | grep -qi '<html'; then
  echo "  [!!]  未知 API 回落成了 HTML —— 前端会把它当 JSON 解析"
  FAILED=$((FAILED + 1))
fi

# 缺失的资源要 404 而不是回落 —— 否则浏览器把 HTML 当 JS 执行，
# 报错是 "Unexpected token '<'"，真正原因（漏传 assets）被掩盖。
code "缺失的静态资源返回 404" "$BASE/assets/nope-deadbeef.js" "404"

section "优雅退出"
kill -TERM "$SERVER_PID" 2>/dev/null || true
stopped=0
for _ in $(seq 1 40); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    stopped=1
    break
  fi
  sleep 0.25
done

if [ "$stopped" = "1" ]; then
  echo "  [ OK ] SIGTERM 后进程已退出（没有卡在 WS 连接上）"
  SERVER_PID=""
else
  echo "  [!!]  SIGTERM 后进程仍在运行 —— 检查 Shutdown 是否漏了 WS 连接"
  FAILED=$((FAILED + 1))
fi

# ---------------------------------------------------------------------------

printf '\n'
if [ "$FAILED" -gt 0 ]; then
  printf '冒烟测试失败：%d 项\n\n' "$FAILED"
  echo "--- server.log ---"
  cat "$TMP/server.log"
  exit 1
fi
printf '冒烟测试全部通过。\n'
