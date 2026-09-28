#!/usr/bin/env bash
#
# CodeGate 端到端验证：真 Agent + 真 Server + 真终端字节。
#
# # 这个脚本补的是哪一段
#
# 现有两个验证各管一半，**都碰不到终端数据通路**：
#
#   scripts/server-smoke.sh      HTTP 层：状态码 / 认证 / 缓存头 / 优雅退出
#   scripts/web-render-check.sh  前端能不能挂载（真 Chrome 渲染）
#
# 而这条链路没有被任何东西覆盖：
#
#   Agent（真 ConPTY，跑真进程）──二进制帧──▶ Server（中继+ring buffer）──▶ 客户端
#
# 它断掉的症状是「网页能打开、能登录、能点新建会话，然后终端永远空白」——
# 前面所有断言都是绿的。
#
# # 流程
#
#   1. 起一个临时 Server（临时数据库、随机端口）
#   2. 注册用户
#   3. 跑 `codegate-agent pair`，从它的输出里抓配对码
#   4. 用 REST 完成两步配对（预览 → 确认）
#   5. 跑 `codegate-agent run`，等它上线
#   6. 用 scripts/e2e-client.mjs 扮演浏览器：建会话 → attach → 收终端字节
#   7. 断言输出里有本次运行独有的标记
#
# 全程临时目录 + 随机端口，不碰用户已有数据。
#
# # 用法
#
#   bash scripts/e2e-terminal.sh
#   PORT=18844 bash scripts/e2e-terminal.sh

set -uo pipefail

cd "$(dirname "$0")/.."

SERVER_BIN="${SERVER_BIN:-./bin/codegate-server.exe}"
AGENT_BIN="${AGENT_BIN:-./bin/codegate-agent.exe}"
PORT="${PORT:-18844}"
BASE="http://127.0.0.1:${PORT}"

# 本机专属：访问 localhost 必须绕过那个坏代理，否则 502
CURL=(curl -s --noproxy '*')

FAILED=0
pass() { printf '  [ OK ] %s\n' "$1"; }
fail() { printf '  [!!]  %s\n' "$1"; FAILED=$((FAILED + 1)); }
info() { printf '         %s\n' "$1"; }
section() { printf '\n=== %s ===\n' "$1"; }

for b in "$SERVER_BIN" "$AGENT_BIN"; do
  if [ ! -x "$b" ]; then
    echo "!! 找不到 $b。先跑：make build" >&2
    exit 1
  fi
done

if ! command -v node >/dev/null 2>&1; then
  echo "!! 找不到 node（e2e-client.mjs 需要 Node 22+，WebSocket 是内置的）" >&2
  exit 1
fi

TMP="$(mktemp -d)"
# ★ Agent / Server 都是原生 Windows 程序，读不懂 MSYS 的 /tmp/... 路径。
#   统一转成 Windows 形式（C:/Users/...）。
TMPW="$(cd "$TMP" && pwd -W)"
SERVER_PID=""
AGENT_PID=""
PAIR_PID=""

cleanup() {
  for p in "$AGENT_PID" "$PAIR_PID" "$SERVER_PID"; do
    if [ -n "$p" ] && kill -0 "$p" 2>/dev/null; then
      kill -TERM "$p" 2>/dev/null || true
      wait "$p" 2>/dev/null || true
    fi
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

MARKER="CODEGATE_E2E_${RANDOM}${RANDOM}"

# ---------------------------------------------------------------------------

section "启动 Server"
export CODEGATE_DB_PATH="${TMPW}/e2e.db"
export CODEGATE_LISTEN="127.0.0.1:${PORT}"
export CODEGATE_JWT_SECRET="0123456789abcdef0123456789abcdef"
export CODEGATE_ALLOW_SIGNUP="true"
export CODEGATE_LOG_LEVEL="info"

"$SERVER_BIN" serve > "$TMP/server.log" 2>&1 &
SERVER_PID=$!

ready=0
for _ in $(seq 1 60); do
  if "${CURL[@]}" -o /dev/null "$BASE/healthz" 2>/dev/null; then ready=1; break; fi
  sleep 0.2
done
if [ "$ready" != "1" ]; then
  echo "!! Server 未就绪：" >&2; cat "$TMP/server.log" >&2; exit 1
fi
pass "Server 已就绪（pid=$SERVER_PID port=$PORT）"

section "注册用户"
TOKEN="$("${CURL[@]}" -X POST "$BASE/api/v1/auth/register" \
  -H 'Content-Type: application/json' \
  -d '{"email":"e2e@example.com","password":"correct-horse-battery"}' \
  | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{process.stdout.write(JSON.parse(s).access_token||"")}catch{}})')"
if [ -n "$TOKEN" ]; then pass "已注册并拿到 access token"; else fail "注册失败：$(cat "$TMP/server.log" | tail -3)"; exit 1; fi

# ---------------------------------------------------------------------------
# Agent 配置
# ---------------------------------------------------------------------------
#
# allowed_roots / allowed_commands 必须显式声明 —— Agent 拒绝在没有安全边界
# 的情况下启动（架构 §21.1）。这里给一个临时工作目录和一条 echo 命令。
#
# ★ command 里的正斜杠是**故意**的。
#
# `C:/Windows/System32/cmd.exe` 是用户最容易写出的形式，也是 cmd.exe
# 唯一会踩坑的形式：它用字符串扫描找 /c 开关，路径里的 `/cmd.exe`
# 会先命中，于是报「命令语法不正确。」
#
# 所以这条配置同时验证了两件事：端到端通路是通的，以及
# internal/agent 的 normalizeCommandPath 确实把正斜杠兜住了。
# （回归细节见 TestNormalizeCommandPath 与 TestPrepareNormalizesCommandPaths）

section "配置 Agent"
mkdir -p "$TMP/work" "$TMP/agent-state"
WORKW="${TMPW}/work"

COMMAND_ID="e2e-echo"
COMMAND_ARGS='["/c", "echo", "'"${MARKER}"'"]'
if [ "${E2E_CLI_CONTROL:-0}" = "1" ] || [ "${E2E_TUI_CONTROL:-0}" = "1" ] || [ "${E2E_OPENCODE_CONTROL:-0}" = "1" ]; then
  COMMAND_ID="e2e-cli"
  COMMAND_ARGS='["/Q", "/K"]'
fi

cat > "$TMP/agent.json" <<EOF
{
  "server_url": "ws://127.0.0.1:${PORT}/api/v1/ws/agent",
  "insecure": true,
  "state_dir": "${TMPW}/agent-state",
  "device_name": "e2e-agent",
  "allowed_roots": ["${WORKW}"],
  "allowed_commands": [
    {
      "id": "${COMMAND_ID}",
      "label": "E2E CLI",
      "command": "C:/Windows/System32/cmd.exe",
      "args": ${COMMAND_ARGS},
      "kind": "shell"
    }
  ]
}
EOF
pass "配置已写入（allowed_roots=${WORKW}）"

# ---------------------------------------------------------------------------

section "配对"
"$AGENT_BIN" pair -config "$TMP/agent.json" > "$TMP/pair.out" 2>&1 &
PAIR_PID=$!

CODE=""
for _ in $(seq 1 60); do
  CODE="$(grep -oE '[A-Z0-9]{4}-[A-Z0-9]{4}' "$TMP/pair.out" 2>/dev/null | head -1)"
  [ -n "$CODE" ] && break
  if ! kill -0 "$PAIR_PID" 2>/dev/null; then break; fi
  sleep 0.3
done

if [ -z "$CODE" ]; then
  fail "没拿到配对码。Agent 输出："
  sed -n '1,20p' "$TMP/pair.out" >&2
  exit 1
fi
pass "配对码 = $CODE"

# jsonfield 从 JSON 里取一个（可嵌套的）字段，取不到就返回空串。
#
# ★ 用 node 解析而不是 grep 字段名。
#
# 两个配对接口的响应结构**不一样**：预览是扁平的
# `{"device_id": ...}`，确认是嵌套的 `{"device": {"id": ...}}`。
# 拿 grep 去匹配字段名，等于把断言绑死在响应结构上 ——
# 结构一变测试就"失败"，而真正的问题只是断言写错了。
# 这里踩过一次：确认其实成功了（响应里有完整 device 对象），
# 却因为查不到顶层 device_id 而报失败。
jsonfield() {
  node -e '
    let s = "";
    process.stdin.on("data", d => s += d).on("end", () => {
      try {
        let v = JSON.parse(s);
        for (const k of process.argv.slice(1)) v = v?.[k];
        process.stdout.write(typeof v === "string" ? v : "");
      } catch {}
    });
  ' "$@"
}

# 第一步：预览（此时还没有绑定，只是让用户看清自己在绑哪台机器）
PREVIEW="$("${CURL[@]}" -X POST "$BASE/api/v1/devices/pair" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"code\":\"$CODE\"}")"
PREVIEW_ID="$(printf '%s' "$PREVIEW" | jsonfield device_id)"
if [ -n "$PREVIEW_ID" ]; then
  pass "配对预览返回了设备信息（device_id=$PREVIEW_ID）"
else
  fail "配对预览失败：$PREVIEW"
fi

# 第二步：确认
CONFIRM="$("${CURL[@]}" -X POST "$BASE/api/v1/devices/pair/confirm" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"code\":\"$CODE\"}")"
CONFIRM_ID="$(printf '%s' "$CONFIRM" | jsonfield device id)"

# ★ 断言「预览里那台设备，就是确认后绑定的那台」。
# 只检查「响应里有某个字段」证明不了这一点 —— 那可能绑的是另一台机器。
if [ -n "$CONFIRM_ID" ] && [ "$CONFIRM_ID" = "$PREVIEW_ID" ]; then
  pass "配对已确认，设备已绑定到账号（id=$CONFIRM_ID）"
else
  fail "配对确认失败：预览 id=$PREVIEW_ID，确认 id=$CONFIRM_ID；原始响应：$CONFIRM"
fi

wait "$PAIR_PID" 2>/dev/null || true
PAIR_PID=""

# ---------------------------------------------------------------------------

section "启动 Agent"
"$AGENT_BIN" run -config "$TMP/agent.json" > "$TMP/agent.log" 2>&1 &
AGENT_PID=$!

online=0
for _ in $(seq 1 60); do
  if "${CURL[@]}" -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/devices" \
     | grep -q '"online":true'; then online=1; break; fi
  if ! kill -0 "$AGENT_PID" 2>/dev/null; then break; fi
  sleep 0.3
done

if [ "$online" = "1" ]; then
  pass "Agent 已连上且设备显示为 online"
else
  fail "Agent 没有上线。日志："
  sed -n '1,25p' "$TMP/agent.log" >&2
fi

# ---------------------------------------------------------------------------

section "端到端终端（Agent → Server → 客户端）"
CLIENT_ARGS=(--base "$BASE" --token "$TOKEN" --marker "$MARKER"
  --command-id "$COMMAND_ID" --cwd "$WORKW" --timeout 30000)
if [ "${E2E_CLI_CONTROL:-0}" = "1" ]; then
  # cmd.exe keeps running. The client sends both commands through a real STDIN frame.
  # 用 cmd 的 ^ 转义拆开标记，输入回显里不会包含完整 MARKER。
  # 只有 echo 真正执行后，终端输出才出现未拆开的标记。
  SPLIT_MARKER="${MARKER:0:12}^${MARKER:12}"
  INPUT_TEXT="$(printf 'codex --help\recho %s\r' "$SPLIT_MARKER")"
  CLIENT_ARGS+=(--stdin-text "$INPUT_TEXT" --expect "Codex CLI")
fi
if [ "${E2E_TUI_CONTROL:-0}" = "1" ]; then
  CLIENT_ARGS+=(--tui 1 --timeout 45000)
fi
if [ "${E2E_OPENCODE_CONTROL:-0}" = "1" ]; then
  CLIENT_ARGS+=(--opencode 1 --timeout 45000)
fi
node scripts/e2e-client.mjs "${CLIENT_ARGS[@]}"
CLIENT_EXIT=$?

if [ "$CLIENT_EXIT" != "0" ]; then
  FAILED=$((FAILED + 1))
  echo "--- server.log 尾部 ---" >&2; tail -12 "$TMP/server.log" >&2
  echo "--- agent.log 尾部 ---" >&2;  tail -12 "$TMP/agent.log" >&2
fi

# ---------------------------------------------------------------------------

printf '\n'
if [ "$FAILED" -gt 0 ]; then
  printf '端到端验证失败：%d 项\n' "$FAILED"
  exit 1
fi
printf '端到端验证全部通过。\n'
