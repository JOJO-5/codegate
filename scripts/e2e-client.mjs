#!/usr/bin/env node
//
// CodeGate 端到端验证 —— 扮演「浏览器那一端」。
//
// # 为什么需要它
//
// `server-smoke.sh` 验证的是 HTTP 层（状态码、头、认证），
// `web-render-check.sh` 验证的是「前端能不能挂载」。
// 两者都不碰**终端数据通路**：
//
//     Agent（真实 ConPTY，跑一个真进程）
//        │  二进制帧
//        ▼
//     Server（中继 + ring buffer + attach 重放）
//        │  二进制帧
//        ▼
//     这个脚本（等价于浏览器里的 ws.ts + xterm）
//
// 这条链路上任何一环断了，前面的验证都是绿的，而用户看到的是一个
// 永远空白的终端。所以必须单独测。
//
// # 为什么用 Node 而不是 Playwright
//
// 这里要证的是「字节有没有从进程流到客户端」。Playwright 会额外引入
// 浏览器版本、WebGL、字体渲染等无关变量。用 Node 内置的 WebSocket
// 能把这个变量彻底去掉 —— 而且 Node 22 起 WebSocket 是内置的，
// 零依赖、零安装。
//
// 浏览器那一端由 `web-render-check.sh` 单独负责（它证明 Vue 能挂载、
// 登录表单能渲染）。两者合起来才是完整的「端到端」。
//
// # 用法
//
//     node scripts/e2e-client.mjs --base http://127.0.0.1:8080 \
//          --token <access_token> --marker CODEGATE_E2E_xxxx \
//          [--command-id e2e-echo] [--cwd /tmp/e2e-work] [--timeout 20000]

import { randomUUID } from 'node:crypto'

// ---- 帧类型（与 internal/protocol/binary.go 对齐）----
const FRAME_STDOUT = 0x02
const FRAME_BUFFER = 0x03
const FLAG_BUFFER_END = 1 << 0
const FLAG_DROPPED = 1 << 1

const FRAME_NAME = { 0x01: 'Stdin', 0x02: 'Stdout', 0x03: 'Buffer', 0x10: 'FileData' }

// ---------------------------------------------------------------------------
// 参数与输出
// ---------------------------------------------------------------------------

function parseArgs(argv) {
  const out = {}
  for (let i = 0; i < argv.length; i += 2) {
    const k = argv[i]
    if (!k?.startsWith('--')) throw new Error(`参数格式错误: ${k}`)
    out[k.slice(2)] = argv[i + 1]
  }
  return out
}

let failures = 0
const ok = (m) => console.log(`  [ OK ] ${m}`)
const bad = (m) => { console.log(`  [!!]  ${m}`); failures++ }
const info = (m) => console.log(`         ${m}`)

const args = parseArgs(process.argv.slice(2))
const BASE = args.base
const TOKEN = args.token
const MARKER = args.marker
const COMMAND_ID = args['command-id'] || 'e2e-echo'
const CWD = args.cwd || ''
const TIMEOUT = Number(args.timeout || 20000)
const STDIN_TEXT = args['stdin-text'] || ''
const EXPECT = args.expect || ''
const TUI = args.tui === '1'

for (const [k, v] of Object.entries({ base: BASE, token: TOKEN, marker: MARKER })) {
  if (!v) { console.error(`缺少必需参数 --${k}`); process.exit(2) }
}

const authHeaders = { Authorization: `Bearer ${TOKEN}` }

// ---------------------------------------------------------------------------
// REST：拿设备列表与 WS 票据
// ---------------------------------------------------------------------------

async function rest(path, init = {}) {
  const res = await fetch(BASE + path, {
    ...init,
    headers: { ...authHeaders, ...(init.headers || {}) },
  })
  const text = await res.text()
  if (!res.ok) throw new Error(`${path} → HTTP ${res.status}: ${text.slice(0, 300)}`)
  try { return JSON.parse(text) } catch { throw new Error(`${path} 返回的不是 JSON: ${text.slice(0, 200)}`) }
}

console.log('\n=== 准备 ===')

const devicesRaw = await rest('/api/v1/devices')
const devices = Array.isArray(devicesRaw) ? devicesRaw : (devicesRaw.devices ?? [])
if (devices.length === 0) {
  bad('设备列表为空 —— 说明 Agent 没有连上或没有配对成功')
  process.exit(1)
}
const device = devices[0]
const deviceId = device.device_id ?? device.id
info(`设备: ${device.name ?? '(无名)'}  id=${deviceId}  online=${device.online}`)
if (device.online === false) bad('设备显示为离线 —— 后面大概率拿不到终端输出')

const ticket = await rest('/api/v1/ws-ticket', { method: 'POST' })
ok(`拿到 WS 票据（protocol ${ticket.protocol?.min}-${ticket.protocol?.max}）`)

// ---------------------------------------------------------------------------
// WS 客户端：等价于前端的 lib/ws.ts（简化版）
// ---------------------------------------------------------------------------

const wsURL = BASE.replace(/^http/, 'ws') + `/api/v1/ws/client?ticket=${encodeURIComponent(ticket.ticket)}`
const ws = new WebSocket(wsURL)
ws.binaryType = 'arraybuffer'

const pending = new Map()          // request_id → {resolve, reject}
const controlLog = []              // 收到的控制消息类型，用于失败时给线索

function send(type, payload, sessionId = '') {
  const id = randomUUID()
  const env = { v: 1, type, request_id: id, session_id: sessionId, ts: Date.now(), payload }
  ws.send(JSON.stringify(env))
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id)
      reject(new Error(`等待 ${type} 的响应超时（${TIMEOUT}ms）。收到的控制消息: ${controlLog.join(', ') || '(无)'}`))
    }, TIMEOUT)
    pending.set(id, { resolve, reject, timer })
  })
}

// 终端输出累积
let stdoutBytes = 0
let bufferBytes = 0
let sawBufferEnd = false
let sawDropped = false
const textDecoder = new TextDecoder('utf-8', { fatal: false })
let received = ''

ws.addEventListener('message', (ev) => {
  // ---- 控制消息（文本）----
  if (typeof ev.data === 'string') {
    let env
    try { env = JSON.parse(ev.data) } catch { return }
    controlLog.push(env.type)

    if (env.type === 'error') {
      // 有 request_id 的错误是对某个请求的应答
      const p = env.reply_to && pending.get(env.reply_to)
      if (p) { clearTimeout(p.timer); pending.delete(env.reply_to); p.reject(new Error(`服务端错误: ${JSON.stringify(env.payload)}`)) }
      return
    }
    if (env.reply_to && pending.has(env.reply_to)) {
      const p = pending.get(env.reply_to)
      clearTimeout(p.timer)
      pending.delete(env.reply_to)
      p.resolve(env.payload)
    }
    return
  }

  // ---- 二进制帧 ----
  const buf = ev.data
  if (buf.byteLength < 20) return
  const u8 = new Uint8Array(buf)
  const dv = new DataView(buf)
  const type = u8[1]
  const flags = dv.getUint16(2, false)
  const payload = u8.subarray(20)

  if (flags & FLAG_DROPPED) sawDropped = true
  if (flags & FLAG_BUFFER_END) sawBufferEnd = true

  if (type === FRAME_STDOUT) {
    stdoutBytes += payload.length
    received += textDecoder.decode(payload, { stream: true })
  } else if (type === FRAME_BUFFER) {
    bufferBytes += payload.length
    received += textDecoder.decode(payload, { stream: true })
  }
})

const opened = new Promise((resolve, reject) => {
  ws.addEventListener('open', resolve)
  ws.addEventListener('error', () => reject(new Error('WebSocket 连接失败')))
  ws.addEventListener('close', (e) => reject(new Error(`WebSocket 被关闭: code=${e.code} reason=${e.reason}`)))
})

console.log('\n=== 建连 ===')
await opened
ok(`WebSocket 已连接（${wsURL.replace(/ticket=.*/, 'ticket=***')}）`)

// ---------------------------------------------------------------------------
// 开会话 → attach → 收终端字节
// ---------------------------------------------------------------------------

console.log('\n=== 建会话 ===')
const created = await send('session.create', {
  device_id: deviceId,
  name: 'e2e',
  command_id: COMMAND_ID,
  cwd: CWD,
  cols: 100,
  rows: 30,
})
const sessionId = created?.session?.session_id
if (!sessionId) {
  bad(`session.create 没返回 session_id: ${JSON.stringify(created)}`)
  process.exit(1)
}
ok(`会话已建立 id=${sessionId} status=${created.session.status}`)

console.log('\n=== attach（含 ring buffer 重放）===')
const attached = await send('session.attach', { session_id: sessionId, since: 0, cols: 100, rows: 30 }, sessionId)
ok(`attach 成功 role=${attached.role} seq=${attached.seq_from}..${attached.seq_to}`)

// Send user keystrokes as a real protocol STDIN frame (not a control JSON message).
function sendInput(input) {
  const payload = new TextEncoder().encode(input)
  const frame = new Uint8Array(20 + payload.length)
  frame[0] = 1
  frame[1] = 1 // FrameStdin
  const id = sessionId.replaceAll('-', '')
  if (!/^[0-9a-f]{32}$/i.test(id)) throw new Error(`invalid session ID: ${sessionId}`)
  for (let i = 0; i < 16; i++) frame[4 + i] = Number.parseInt(id.slice(i * 2, i * 2 + 2), 16)
  frame.set(payload, 20)
  ws.send(frame)
  ok(`已向会话写入 ${payload.length} 字节输入`)
}
if (TUI) sendInput("codex --no-daemon\r")
else if (STDIN_TEXT) sendInput(STDIN_TEXT)
if (attached.seq_from > 0) {
  bad(`seq_from=${attached.seq_from} > 0 —— 说明 ring buffer 已经丢过数据`)
} else {
  ok('重放区间从 0 开始（没有丢帧）')
}

// 等终端输出出现 marker，或者超时
console.log('\n=== 收终端字节 ===')
const deadline = Date.now() + TIMEOUT
while (Date.now() < deadline && !(TUI ? received.length > 500 : received.includes(MARKER))) {
  await new Promise((r) => setTimeout(r, 100))
}

info(`Stdout ${stdoutBytes} 字节 / Buffer ${bufferBytes} 字节`)

if (TUI) {
  info(`TUI raw prefix: ${JSON.stringify(received.slice(0, 2000))}`)
  info(`Alternate screen: ${received.includes("\x1b[?1049h")}`)
  sendInput("\x1b[B")
  await new Promise((r) => setTimeout(r, 2000))
  info(`After arrow: ${JSON.stringify(received.slice(-1200))}`)
  sendInput("\x03")
  if (received.includes("Error:") || received.includes("error:")) bad("Codex interactive startup reported an error")
  else if (received.includes("\x1b[?1049h") && received.includes("OpenAI Codex") && received.includes("Sign in with ChatGPT")) ok("Codex TUI rendered and responded to arrow input with the sign-in menu")
  else bad("Codex TUI did not render its interactive sign-in menu")
} else if (EXPECT) {
  if (received.toLowerCase().includes(EXPECT.toLowerCase())) ok(`收到编程 CLI 输出：${EXPECT}`)
  else {
    bad(`未收到预期的编程 CLI 输出：${EXPECT}`)
    info(`收到的前 1800 字符：${JSON.stringify(received.slice(0, 1800))}`)
  }
}
if (TUI) {
  info("TUI mode: marker assertion skipped")
} else if (received.includes(MARKER)) {
  ok(`终端输出里找到了标记「${MARKER}」—— Agent → Server → Client 全链路通`)
} else {
  bad(`终端输出里没有标记「${MARKER}」`)
  info(`实际收到 ${received.length} 字符，前 300 字：${JSON.stringify(received.slice(0, 300))}`)
}

if (bufferBytes > 0) ok('收到了 Buffer 帧（attach 重放路径被真实走到）')
else info('没有 Buffer 帧 —— 输出是在 attach 之后实时到达的（也正常）')

if (sawBufferEnd) ok('收到 BufferEnd 标志（重放结束边界正确）')
else info('没有 BufferEnd 标志（重放已在此之前结束）')

if (sawDropped) bad('收到 Dropped 标志 —— ring buffer 溢出，有输出被丢弃')

// ---- 清理 ----
console.log('\n=== 关闭会话 ===')
try {
  await send('session.close', { session_id: sessionId }, sessionId)
  ok('会话已关闭')
} catch (e) {
  bad(`关闭会话失败: ${e.message}`)
}
ws.close()

console.log('')
if (failures > 0) {
  console.log(`端到端终端验证失败：${failures} 项`)
  process.exit(1)
}
console.log('端到端终端验证全部通过。')
process.exit(0)
