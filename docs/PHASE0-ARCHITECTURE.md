# CodeGate — Phase 0 架构设计

> 状态：Draft v1 · 2026-09-28  
> 阶段目标：**只做设计，不写业务代码。** 本文档是 Phase 1–10 的唯一架构基准。  
> 一句话定义：**CodeGate 是一个 Remote PTY Transport，不是 Terminal，不是 IDE，不是 AI Agent。**

---

## 0. 一页纸结论

### 0.1 关键技术决策（推荐方案一览）

| #   | 决策点                 | 推荐方案                                                                              | 备选                                                    | 为什么                                                                                                                                     |
| --- | ------------------- | --------------------------------------------------------------------------------- | ----------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| D1  | 仓库形态                | 单 Go module 的 monorepo（server + agent 两个 binary）+ 同仓 `web/`                       | 多 module / 多仓                                         | 协议类型要共享，多 module 只会带来 replace 地狱；MVP 一个 module 足够                                                                                       |
| D2  | Server 与 Agent 是否分仓 | 同仓不同 `cmd/`                                                                       | 分开                                                    | 协议演进同步，CI 一次跑完                                                                                                                          |
| D3  | MVP 数据库             | SQLite（`modernc.org/sqlite`，纯 Go 无 cgo）                                           | PostgreSQL 起步                                         | 单文件、零运维、无 cgo 才能 `CGO_ENABLED=0` 交叉编译；查询都很简单，后期换 PG 成本可控                                                                                |
| D4  | DB 访问层              | 手写 SQL + `database/sql`，一个 `storage.Store` 接口、一个实现                                | sqlc / GORM / ent                                     | 表少（6 张）、SQL 简单；ORM 会带来"为抽象而抽象"。`Store` 接口存在的唯一理由是将来加 PG 实现                                                                              |
| D5  | 迁移                  | 自写 30 行 migrator + `go:embed` 的 `NNNN_*.sql`                                      | golang-migrate / goose                                | 少一个依赖，少一个部署件                                                                                                                            |
| D6  | Windows PTY         | **ConPTY**，通过 `aymanbagabas/go-pty` 封装，外面再套自己的 `terminal.Terminal` 接口             | ① 手写 `CreatePseudoConsole` ② `UserExistsError/conpty` | 手写 ConPTY 的坑（属性列表、句柄生命周期、Close 顺序）不值得在 MVP 踩；`go-pty` 同时提供 Unix 实现，接口统一。**但必须留好逃生通道**：接口是我们自己的，Phase 2 PoC 若发现 `go-pty` 有问题，换成手写只影响一个文件 |
| D7  | Unix PTY            | 同一个 `go-pty`（底层 `creack/pty` 系）                                                   | 直接用 `creack/pty`                                      | 统一接口，减少两套代码                                                                                                                             |
| D8  | Ring Buffer         | 定长 `[]byte` 环形缓冲 + 单调递增 `total_written` 序号                                        | 分块链表                                                  | 定长数组分配一次、无 GC 抖动；序号让 attach 时能算"缺多少"，不必重放整段                                                                                             |
| D9  | Buffer 重放语义         | 直接重放尾部原始字节                                                                        | 在 Agent 侧维护 VT 屏幕模拟器，attach 时合成一次全屏重绘                 | MVP 简单优先。**代价要说清楚：TUI 状态下重放可能还原不精确**（见 R2）                                                                                              |
| D10 | 控制面 vs 数据面          | REST 只负责 auth / device / ws-ticket / health；**session 全生命周期走 client WebSocket**   | session 也走 REST                                       | 见 §14 的详细论证。核心：session 的最终执行者是 Agent，事件（exit）本来就只能从 WS 来，走 REST 会形成双写与状态分歧                                                              |
| D11 | 浏览器 WS 鉴权           | `POST /ws-ticket` 换一次性 ticket，`?ticket=` 连接                                       | JWT 直接放 query / Cookie                                | 避免长期凭证进 URL（日志、Referer、浏览器历史）。ticket 30s 有效、单次使用                                                                                        |
| D12 | Agent 鉴权            | **Ed25519 挑战-应答**（nonce 单次、30s TTL）                                               | 长期 bearer token                                       | 规范要求 keypair；Ed25519 在 Go 里就 5 行代码，且服务端只存公钥——即使 DB 泄露也伪造不了 Agent                                                                        |
| D13 | Pairing Code        | Server 生成（Agent 通过 WS 索取后打印），Crockford Base32、8 位、TTL 10 分钟、单次使用                  | Agent 本地生成                                            | 单一真相源、避免碰撞、便于统一限流与审计                                                                                                                    |
| D14 | 终端数据帧               | 20 字节二进制头 + 裸 payload，走 WS Binary Frame                                           | Base64 塞进 JSON / 纯裸字节                                 | 见 §16。**绝对不 Base64，绝对不 JSON 包 stdout**                                                                                                  |
| D15 | 同 session 多客户端      | **同账号允许多客户端同时 attach**（fan-out），跨账号一律拒绝                                                             | 多路 fan-out                                            | resize 语义在多客户端下无解，必须选一个权威：推荐「主控客户端」制（首个 attach 者为 controller，只有它的 resize 生效，断开则顺位）。JOJO 要求同账号多端同看（手机 + 电脑），所以不能简单禁止多端                                                                                            |
| D16 | 前端终端库               | `@xterm/xterm`（v5.5+ 的新包名）+ fit / weblinks / search / unicode11                   | 旧包名 `xterm`                                           | 旧包已停止更新；Unicode11 对中文/emoji 宽度必需                                                                                                        |
| D17 | WebGL 渲染            | MVP **不用** WebGLAddon                                                             | 用                                                     | 浏览器 WebGL context 有上限（~16），SPA 里反复挂载会 context lost。等真需要再开                                                                               |
| D18 | 前端产物分发              | `web/dist` 构建后拷进 `internal/server/webui/`，用 `go:embed` 打进 Server 单文件              | Nginx 单独托管 / 独立容器                                     | 部署 = 一个二进制 + 一个 DB。开发期仍用 Vite devserver 代理                                                                                              |
| D19 | 部署形态                | 一个 Server 容器 + 一个挂载卷（SQLite）；Agent 是用户机器上的裸二进制                                    | 全套 compose（PG+Redis+…）                                | 规范第 40 条。MVP 不引 Redis/Kafka/gRPC/K8s                                                                                                    |
| D20 | 信号语义                | `Terminal.Signal(SIGINT)`：Unix 走真实 `kill(-pgid, SIGINT)`；Windows 走"向 PTY 写入 0x03" | Windows 上返回 `ErrUnsupported`                          | Windows 没有 POSIX 信号，ConPTY 也没有公开的注入 Ctrl+C 的 API。写 0x03 是唯一"透明"的做法：前台是控制台程序时等价于 Ctrl+C，是 raw mode 的 TUI 时它就是个字节（这正是我们想要的行为）             |

### 0.2 五条不可动摇的原则（来自规范第 43 条，落到实现上）

1. **Agent 是唯一执行边界** —— 任何 PTY 创建、命令解析、路径校验都发生在 Agent 进程内。Server 不 `exec` 任何东西。
2. **Server 不解析终端字节流** —— 它的 relay 代码里不应出现任何 `[]byte` → 字符串的转换（除了必要的长度检查）。
3. **Session 生命周期 ⊥ 浏览器连接生命周期** —— 代码层面体现在：`session.Manager` 不持有任何 WS 连接引用，只持有 `io.Writer` 抽象。
4. **Agent 永远主动外连** —— 代码层面体现在：`internal/agent` 里没有任何 `net.Listen`。
5. **稳定优先于功能** —— 每个 Phase 的验收标准是"跑得稳"，不是"功能多"。

### 0.3 已确认的决策（2026-09-28，JOJO 确认）

| #  | 问题              | 结论                                                                                                                                                            | 落到哪                                      |
| -- | --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| Q1 | 注册策略            | ✅ **第一个注册用户自动成为管理员，之后注册默认关闭**（`allow_signup: false`）。避免公网 Server 被陌生人注册                                                                                    | §10.5                                    |
| Q2 | 自定义命令           | ✅ **白名单制**。Agent 侧 `allowed_commands` 预置常用项；`allow_custom_commands` 默认 `false`，要开得改本机配置——**这等于本机用户自己授权**，符合"Agent 是本机执行边界"。<br>✅ **关闭 Session 必须二次确认**（点击「关闭」→ 再点「确认关闭」），防误触。<br>✅ **常用 agent 命令预置到界面**，并支持用户自定义。 | §21（CLI 启动与恢复）                          |
| Q3 | 多客户端            | ✅ **同一账号可以多端同看同一 session**（手机 + 电脑）；**跨账号一律禁止**。暂无共享/分享功能。                                                                                                     | D15、§7.6                                 |
| Q4 | 部署环境            | ✅ **开发阶段跑在本机**；后续上服务器，但**服务器内存小** → 强化单二进制 + SQLite，不做重型中间件；Server 增加低内存模式（见 §22）                                                                            | §22                                      |
| Q5 | 手机端优先级          | ✅ **优先级高**。JOJO 常在手机上开发（要陪家人，不能总坐电脑前），上班时用电脑操控家里机器。→ **移动端 UX 提升为 MVP 必做项**，不再是"响应式能用就行"                                                                          | §20（移动端 UX 设计）                           |

**另外两条被 JOJO 直接点出的需求：**

- **对话恢复**：希望有「恢复之前的对话」按钮 → 设计为 **CLI 自身的 resume 机制**，不是终端 buffer 重放。见 §21.3。
- **R1 验证方式**：JOJO 提出"看看 opencode / codex / claude code 都是咋渲染画面的，我们试试" → **已执行完毕**，结论见 `docs/PHASE0-R1-CONPTY-FINDINGS.md`。

---

## 1. Architecture Overview

### 1.1 三个组件

| 组件                  | 形态                            | 语言                | 网络角色                                            |
| ------------------- | ----------------------------- | ----------------- | ----------------------------------------------- |
| **CodeGate Server** | 单个 Go binary（可容器化）            | Go 1.24+          | 被动：`Listen` HTTPS + WSS。是 Control Plane + Relay |
| **CodeGate Agent**  | 用户机器上的 Go binary（未来做 Service） | Go 1.24+          | 主动：只发起 outbound WSS，永不监听端口                      |
| **CodeGate Web**    | 静态资源，编译进 Server               | Vue 3 + TS + Vite | 浏览器里跑                                           |

### 1.2 两条数据通路

**控制面（JSON，低频）**

```
Browser ──HTTPS──> Server ──WSS──> Agent
Browser ──WSS(JSON)──> Server ──WSS(JSON)──> Agent
```

**数据面（Binary Frame，高频）**

```
Agent(PTY 读) ──WSS Binary──> Server(不解析) ──WSS Binary──> Browser
Browser(xterm.onData) ──WSS Binary──> Server ──WSS Binary──> Agent(PTY 写)
```

### 1.3 为什么不需要公网 IP

Agent 建立的是**出站** TLS 连接。NAT/CGNAT/企业防火墙默认允许出站 443，因此：

- 家庭路由器不用做端口映射
- 动态 IP 不影响（Agent 断线后按指数退避重连，Server 地址是域名）
- 企业代理：需支持 `HTTPS_PROXY` 环境变量（Go 的 `http.ProxyFromEnvironment` 默认行为）+ 自定义 CA（`SSL_CERT_FILE`）

### 1.4 Server 的"无状态"边界（重要澄清）

规范要求"Server 尽量无状态"，但 Server **必须**持久化：用户、设备注册表、设备公钥、会话元数据、审计日志。所以准确的说法是：

> **Server 不持有任何"活"状态。** 所有运行中的 PTY、字节缓冲、连接都只存在于 Agent 和 Server 的内存中；Server 进程重启后，**Agent 重连时会重新上报真实 session 列表，Server 据此对账 DB**。

这条决定了 §7.4 的"对账"机制，也是"Server 重启不丢会话"的实现方式。

---

## 2. Component Diagram

见对话中的组件图。文字版：

```
┌──────────────────────────┐         ┌─────────────────────────────┐         ┌──────────────────────────┐
│  Browser                 │         │  CodeGate Server            │         │  CodeGate Agent          │
│  ─────────────           │         │  ────────────────           │         │  ──────────────          │
│  Vue 3 + xterm.js        │ HTTPS   │  Auth / REST API            │   WSS   │  Connection (HB/Reconn)  │
│  Pinia store             │◄───────►│  Session Router / Relay     │◄───────►│  Session Manager         │
│  WS Client (自动重连)     │  WSS    │  Agent Registry             │ (Agent  │  Ring Buffer             │
│                          │         │  Device Registry            │  主动)  │  PTY Adapter             │
└──────────────────────────┘         │  Store (SQLite → PG)        │         └────────────┬─────────────┘
                                     └─────────────────────────────┘                      │
                                                                                          ▼
                                                                            ┌──────────────────────────┐
                                                                            │  ConPTY (Windows)        │
                                                                            │  Unix PTY (Linux/macOS)  │
                                                                            └────────────┬─────────────┘
                                                                                         ▼
                                                              PowerShell / cmd / wsl / bash / zsh
                                                              Claude Code / Codex / OpenCode / vim …
```

**要点：Server 到 Agent 的箭头是"Agent 发起、长连接复用"，不是 Server 拨号。** 这个箭头方向是整个项目能免端口映射的原因，任何代码都不允许反转它。

---

## 3. Agent Architecture

### 3.1 进程内组件

```
cmd/codegate-agent/main.go
  └─ run
      ├─ config.Load()                 配置 + 环境变量覆盖
      ├─ identity.LoadOrCreate()        设备 Ed25519 密钥 + device_id
      ├─ terminal.Detect()              探测 ConPTY 可用性 / PTY 可用性
      ├─ session.Manager                ★ 核心：持有所有 Session
      │    ├─ map[sessionID]*Session
      │    ├─ *Session.PTY   (terminal.Terminal)
      │    ├─ *Session.Ring  (buffer.Ring)
      │    └─ *Session.attached (io.Writer, 可为 nil)
      └─ conn.Client                    连接状态机 + 读写泵
           ├─ readPump   → 解析控制消息 / 二进制帧 → 分发
           ├─ writePump  → 单写者，bounded channel
           ├─ heartbeat  → ticker
           └─ backoff    → 重连策略
```

### 3.2 连接状态机

```
Disconnected ──connect()──> Connecting ──TLS/upgrade OK──> Authenticating
      ▲                                                          │
      │                                                     verify OK
      │                                                          ▼
      └──backoff──── Reconnecting <──链路断开/心跳超时──── Connected
```

- `Connecting`：DNS + TCP + TLS + WS Upgrade，超时 15s
- `Authenticating`：hello → challenge → auth → ready，超时 10s
- `Connected`：心跳 ticker 起，双向泵起
- `Reconnecting`：退避 `min * 2^n`，上限 `reconnect_max`，**乘 0.5–1.5 的 jitter**（防 Server 重启后的惊群）
- 收到 `agent.ready` 时执行 **session 对账**（§7.4）
- 收到 Server 主动关闭且 code 属于"致命"（4401 认证失败、4426 版本不兼容）→ **不重连**，日志报错并退出（避免无效刷屏）；其余 code 一律重连

### 3.3 心跳与超时

| 项                            | 默认值              | 说明                                                  |
| ---------------------------- | ---------------- | --------------------------------------------------- |
| `heartbeat_interval`         | 20s              | Agent → Server `agent.heartbeat`（JSON，带 session 摘要） |
| Server 侧 `heartbeat_timeout` | 60s              | 超过则 Server 判 Agent 离线并 `Close`                      |
| WS Ping 帧                    | 由 Server 每 30s 发 | 检测半开连接（TCP 已死但未 RST），比应用层心跳更底层                      |
| 写超时                          | 10s              | 单帧写不完 → 判定对端卡死                                      |
| 读超时                          | 90s              | 读不到任何帧（含 Ping/Pong）→ 断链重连                           |

### 3.4 Agent 明确不做的事（代码评审红线）

- 不做任何 ANSI / OSC 序列的解析、过滤、改写
- 不做任何 `strings.Contains(output, "...")` 式的输出嗅探
- 不记录 stdin 内容到日志
- 不把 stdout 写入日志文件（除 `--log-terminal` 显式开启且带醒目警告）
- 不把本机环境变量整体上报 Server

---

## 4. Server Architecture

### 4.1 进程内组件

```
cmd/codegate-server/main.go
  └─ serve
      ├─ http.Server (chi router)
      │    ├─ /api/v1/auth/*        登录/刷新/登出
      │    ├─ /api/v1/devices/*     设备 CRUD + pair
      │    ├─ /api/v1/ws-ticket     ★ 一次性 WS 票据
      │    ├─ /api/v1/ws/agent      ★ Agent 长连接入口
      │    ├─ /api/v1/ws/client     ★ 浏览器长连接入口
      │    └─ /                           内嵌 webui (go:embed)
      ├─ registry.Agents            map[deviceID]*AgentConn (RWMutex)
      ├─ registry.Clients           map[connID]*ClientConn
      ├─ session.Cache              服务端侧的 session 元数据视图（DB 的写回缓冲）
      ├─ relay.Router               ★ 唯一的跨连接数据通路
      └─ storage.Store              SQLite/PG
```

### 4.2 Relay 的唯一职责

```
Browser frame ──> Router.RouteToAgent(deviceID, frame)
Agent frame   ──> Router.RouteToClient(connID, frame)
```

`Router` 的实现里**只有三件事**：

1. 从已认证的 WS 连接上取"这条连接是谁"（`userID` / `deviceID` / 已 attach 的 `sessionID`）
2. 校验目标是否属于当前调用者（**每次转发都校验，不缓存授权结论**）
3. 投递到目标连接的有界发送队列；队满则按 §25 的策略处理

**Router 不认识 payload 的结构**。二进制帧在 Router 里是 `[]byte`，最多读前 20 字节头做路由（因为 session_id 在头里）。


### 4.3 连接注册表

```go
type AgentConn struct {
    DeviceID  uuid.UUID
    UserID    uuid.UUID
    Conn      *websocket.Conn
    Send      chan []byte   // bounded, 1024
    Sessions  map[uuid.UUID]struct{}  // 该 Agent 当前持有的 session（用于鉴权快速判断）
    LastSeen  atomic.Int64
    Caps      AgentCaps     // platform, agent_version, max_sessions
}
```

`Send chan []byte` 是**唯一**允许向 WS 写入的通道；写由单个 `writePump` goroutine 负责。gorilla/websocket 不支持并发写，这条纪律必须守住。

---

## 5. Web Architecture

### 5.1 技术选型

| 层  | 选择                                                                           | 说明                                               |
| -- | ---------------------------------------------------------------------------- | ------------------------------------------------ |
| 框架 | Vue 3 `<script setup>` + TypeScript                                          | 规范指定                                             |
| 构建 | Vite                                                                         | 规范指定                                             |
| 路由 | vue-router                                                                   |                                                  |
| 状态 | Pinia                                                                        | 只放：auth / devices / sessions 列表。**终端字节不进 Pinia** |
| 终端 | `@xterm/xterm` + `@xterm/addon-fit` + `-weblinks` + `-search` + `-unicode11` |                                                  |
| 样式 | 原生 CSS 变量 + 极简 dark theme                                                    | 不引 Tailwind/UI 库。Developer tool 风格就是深灰 + 等宽字体    |

### 5.2 页面

```
/login                     登录
/                          重定向到 /devices
/devices                   设备列表（在线状态、平台、agent 版本、last seen）
/devices/:id               设备详情 + 会话列表 + New Session
/sessions/:id              终端页（xterm.js 全屏）
/settings                  改密码、Token 管理、审计日志
```

### 5.3 终端页的数据流（关键）

```
xterm.onData(str) ──TextEncoder──> binaryFrame(STDIN) ──WS──> Server ──> Agent ──> PTY.Write
xterm.onBinary(str) ──────────────────────────────┘（8-bit 输入，鼠标/某些粘贴协议会走这条）

WS ──> binaryFrame(STDOUT/BUFFER) ──> payload 直接 xterm.write(Uint8Array)
```

**三条纪律：**

1. `xterm.write()` 收到的是 `Uint8Array`，**不做 UTF-8 解码、不做字符串处理**（xterm.js 自己会做增量解码，正确处理跨帧的多字节字符）
2. 不要 `attachCustomKeyEventHandler` 去改 Ctrl+C/Tab/方向键 —— xterm.js 默认行为已经正确。只拦截浏览器级快捷键：`Ctrl+Shift+V`（粘贴）、`Ctrl+Shift+F`（搜索）、`Ctrl+Shift+C`（复制）
3. 组件卸载时必须 `terminal.dispose()`；`FitAddon` 的 `ResizeObserver` 必须 `disconnect()`（否则路由切换会泄漏）

### 5.4 resize 链路与防抖

```
ResizeObserver(container) ──debounce 120ms──> fitAddon.fit() ──> 比较 cols/rows 是否变化
                                                            └─ 变化才发 session.resize
```

不做防抖 + 不做"值未变则不发"的判断 → 拖窗口时会产生 resize 风暴（每个像素一发），这是 §24 里明确要测的项。

---

## 6. PTY / ConPTY Abstraction

### 6.1 接口（在规范基础上补两处）

```go
package terminal

type Terminal interface {
    Start(ctx context.Context, cfg StartConfig) error

    // Read 返回 PTY 原始字节。允许返回任意切分，调用方不得假设 UTF-8 边界。
    Read(p []byte) (int, error)
    Write(p []byte) (int, error)

    Resize(cols, rows uint16) error
    Signal(sig Signal) error
    Wait() error          // 返回进程退出结果，含 ExitCode
    Close() error         // 幂等
}

type StartConfig struct {
    Command  string
    Args     []string
    Dir      string
    Env      []string   // 完整环境（由 Agent 构造，不来自网络）
    Cols     uint16
    Rows     uint16
}

type ExitResult struct {
    ExitCode int
    Err      error      // 非 nil 表示是异常终止（信号/崩溃）
}
```

**对规范的两处修改建议：**

| 项                | 规范原文       | 建议                                                                         | 理由                                                          |
| ---------------- | ---------- | -------------------------------------------------------------------------- | ----------------------------------------------------------- |
| `Wait() error`   | 返回 `error` | 返回 `ExitResult`（或提供 `ExitCode() int`）                                      | "进程正常退出但 exit code = 1"和"PTY 出错"是两回事，必须能区分。用 `error` 表达会丢信息 |
| `Signal(Signal)` | 通用信号       | 保留，但明确 **Windows 只支持 `SignalInterrupt`**（实现为写入 0x03），其余返回 `ErrUnsupported`。**⚠️ 该实现对 shell 无效，见 §6.5** | 不要假装 Windows 有 POSIX 信号                                     |

### 6.2 Windows 实现要点（ConPTY）

调用链：`CreatePseudoConsole` → 两个匿名管道 → `CreateProcess` 带 `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` → `ResizePseudoConsole` → `ClosePseudoConsole`。

必须处理的细节：

1. **可用性探测**：ConPTY 需要 Win10 1809 (build 17763)+。启动时 `GetProcAddress(kernel32, "CreatePseudoConsole")` 判空；不支持则 `codegate-agent doctor` 明确报错。
2. **句柄关闭顺序**：`CreateProcess` 之后必须立刻关闭自己这侧的管道末端，否则读端永远读不到 EOF。`ClosePseudoConsole` 必须在子进程退出后调用，且**只调一次**。
3. **退出前必须把输出读干净 —— 但 Windows 上不能靠 EOF 判断**：子进程退出后管道里可能还有未读完的数据，必须收尾，否则会丢最后几行（编译错误、CLI 的退出提示）。

   **⚠️ 2026-09-28 实测修正**：Windows 上子进程退出后 `Read` **不会返回 EOF** —— conhost 仍持有管道写端，直到我们调用 `Close()` 才断开（Unix 相反：从端关闭后 master 立刻 EOF）。所以「Wait 返回 → 继续 drain 到 EOF」在 Windows 上是**一个永远等不到的事件，照抄会挂死**。正确顺序：

   ```
   Wait() 拿到退出码 → 给一个短的静默窗口收尾（吸收最后几行） → 主动 Close() → 发 session.exit
   ```

   证据：`cmd/ttyprobe` 的 `TestProbeReadDoesNotEOFOnExit`；详见 `docs/PHASE2-CONPTY-VERIFICATION.md` §5.2。
4. **不泄漏 conhost**：用 **Job Object + `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`** 包住子进程。Agent 崩溃时 OS 自动回收，不会留下一堆孤儿 conhost.exe。
5. **ConPTY 会重绘（re-render）**：ConPTY 不是透明字节管道，它内部维护屏幕缓冲区并按需重绘。这意味着 Agent 读到的字节流与"真实终端会看到的字节流"在时序/合并上可能不同。**这是本项目最大的兼容性风险（R1），Phase 2 PoC 必须用真实 TUI 实测。**

### 6.3 Unix 实现要点

- 用 `openpty` 系（`/dev/ptmx` + `grantpt`/`unlockpt`/`ptsname`）
- 子进程 `setsid()` + `ioctl(TIOCSCTTY)` 取得控制终端
- `Resize` → `ioctl(TIOCSWINSZ)`，内核会向前台进程组发 `SIGWINCH`
- `Signal` → `syscall.Kill(-pgid, sig)`（负号 = 整个进程组）
- `Close` → 关闭 master fd，前台进程组收到 `SIGHUP`

### 6.4 测试夹具（很重要的一个设计）

不要用"跑 `ls` 看输出对不对"这种方式测 PTY —— 太脆弱，而且它测的是 `ls` 不是 PTY。做一个专用探针程序：

```
cmd/ttyprobe/          ← 实际位置（2026-09-28 修正）
```

> **为什么不在 `testdata/` 下**（原建议是 `internal/terminal/testdata/ttyprobe/`）：
> Go 工具链会**完全忽略** `testdata` 目录 —— 那里的代码不会被 `go build` / `go vet` /
> `go test` 编译检查，也就永远不会因为改动而报错。一个不会被编译的测试夹具是维护陷阱。
> 放 `cmd/` 下能被正常检查，也能被集成测试直接构建出来用。

它做这些事：

1. 启动时打印自己的 `cols x rows`（`TIOCGWINSZ` / `GetConsoleScreenBufferInfo`）
2. **打印 stdin 的控制台输入模式** —— 这一项决定了 0x03 走哪条路径，见 §6.5
3. 收到 resize 时再打印一次（Unix 靠 `SIGWINCH`；Windows 靠 **150ms 轮询**，不是事件）
4. 把 stdin 原样回显（`cat` 行为）
5. 收到 0x03 时报告但**不退出**（模拟 raw mode TUI）
6. 支持 `width`（定宽样本）/ `raw`（切 raw mode）/ `exit` 命令

这样一个探针就能在 Windows 和 Unix 上**同时验证**：创建、读写、resize、Ctrl+C、退出码。
**已于 2026-09-28 交付**，验收结果见 `docs/PHASE2-CONPTY-VERIFICATION.md`。

> 关于 resize 的语义：Windows 上「窗口变化」不是一个事件，而是需要被发现的**状态变化**
> （ConPTY 不会通知子进程 `ResizePseudoConsole` 被调用过）。直接后果是**快速连续 resize 时
> 中间尺寸可能被整个跳过**。这不是 bug，但 Phase 6 前端做 resize 防抖时必须知道 ——
> 否则会把「防抖窗口小于轮询周期导致的丢帧」误判成后端 resize 链路断了。

### 6.5 ★ Windows 上 Ctrl+C 的限制与前端兜底方案（2026-09-28 实测新增）

**这是 Phase 2 风险关卡卡出来的真问题，不是理论担忧。**

#### 现象

写 `0x03` 到 ConPTY 输入后，行为**取决于子进程自己的控制台模式**：

| 子进程模式 | 谁在用 | 写 0x03 的结果 |
| --- | --- | --- |
| **cooked**（默认，`PROCESSED_INPUT` 开着） | `cmd.exe`、PowerShell、任何普通控制台程序 | ❌ 字节被 conhost 拦截 → 子进程 **stdin EOF**，命令**不中断** |
| **raw**（应用自己关掉 `PROCESSED_INPUT`） | vim、htop、**Claude Code、Codex、OpenCode** | ✅ 0x03 作为字节送达，进程存活，由应用自己解释 |

实测数据：ConPTY 交给子进程的默认输入模式是 `0x01f7`
（`PROCESSED_INPUT|LINE_INPUT|ECHO_INPUT|MOUSE_INPUT|INSERT_MODE|QUICK_EDIT_MODE|EXTENDED_FLAGS`）。

**这修正了 R1 的结论**（`PHASE0-R1-CONPTY-FINDINGS.md` §7 说「0x03 不产生 CTRL_C_EVENT，
子进程收到的是字符」）—— 那只对 raw mode 成立；R1 当时测的全是 TUI，所以没暴露这一面。

#### 为什么不在后端修

三条路全部实测过：

| 路径 | 结果 |
| --- | --- |
| ① 写 0x03（当前实现） | ❌ 见上表 |
| ② `FreeConsole` → `AttachConsole(子进程)` → `GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)` | ❌ **API 全部返回成功但信号无效**。用 `GetConsoleProcessList` 确认 `AttachConsole` 真的生效（列表含 cmd/ping/测试进程），排除了「调用姿势不对」 |
| ③ `conpty.dll!ConptyGenerateConsoleCtrlEvent`（Windows Terminal 用的私有接口） | ⚠️ 需随包分发 `conpty.dll`；开发机上不存在，无法验证 |

**结论：系统自带的 ConPTY（kernel32 的 `CreatePseudoConsole`）不支持可靠地中断 cooked mode 子进程。**

#### 决策（JOJO 2026-09-28）：前端兜底

**不在后端引入新依赖**（不打包 `conpty.dll`），改为在前端把这件事**如实呈现给用户**：

1. **会话类型标记**：创建会话时按命令分类为 `shell` 或 `tui`（预置命令列表带这个属性）。
   - `tui`（Claude Code / Codex / vim / htop…）：Ctrl+C 按钮**正常工作** —— 它们设了 raw mode。
   - `shell`（cmd / PowerShell / bash…）：Ctrl+C 按钮**置灰或隐藏**，改为提供明确的说明性提示 +
     「终止会话」按钮作为替代动作。
2. **不静默失败**：绝不能让用户点了 Ctrl+C 之后什么都没发生 —— 那比没有按钮更糟。
   按钮要么可用且有效，要么明确不可用并给出替代路径。
3. **文案要具体**：提示写「该命令在 Windows 上无法接收中断信号，可改用『终止会话』」，不要写「暂不支持」。

**后续若要在后端彻底解决**：唯一被证明可靠的通路是分发 `conpty.dll`
（Windows Terminal 的做法），需单独评估体积 / 许可 / 版本兼容。**不要在 MVP 里做。**

#### 影响范围

| 场景 | 影响 |
| --- | --- |
| Claude Code / Codex / OpenCode（**主用例**） | **无影响** |
| vim / htop 等全屏 TUI | 无影响 |
| cmd / PowerShell 里的长时间命令（`npm install`、构建、`ping`） | Ctrl+C 不可用，用「终止会话」替代 |

证据与复现：`docs/PHASE2-CONPTY-VERIFICATION.md` §4；测试 `cmd/ttyprobe/interrupt_windows_test.go`。

---

## 7. Session Lifecycle

### 7.1 状态模型（对规范的一处重构建议）

规范给的是扁平枚举：`starting / running / detached / exited / failed / terminated`。

**问题**：`running` 和 `detached` 在语义上不是同一个维度 —— 一个是"进程活着"，一个是"有人看着"。把它们并列会导致状态机不自洽（比如"detached 且已退出"该填哪个？）。

**建议**：内部用两个正交字段，对外仍暴露规范要求的扁平 `status`（保证 API 兼容）。

```go
type ProcState  uint8  // starting | running | exited | failed | terminated
type AttachState uint8 // none | attached

// 对外 status 的派生规则：
//   terminated            → 已 Close，进程已回收
//   failed                → ProcState=failed（PTY 创建失败 / 异常崩溃）
//   exited                → ProcState=exited（正常退出，含非 0 exit code）
//   starting              → ProcState=starting
//   running               → ProcState=running && AttachState=attached
//   detached              → ProcState=running && AttachState=none
```

好处：`session.list` 返回的 `status` 依然是规范里的 6 个值，但内部不会出现"该字段到底代表什么"的歧义；前端也能额外拿到 `exit_code`。

### 7.2 状态迁移

```
                create
                  │
                  ▼
             ┌─────────┐   PTY 创建成功    ┌──────────────┐  client WS 关闭  ┌──────────┐
             │ starting│──────────────────>│ running      │─────────────────>│ detached │
             └────┬────┘                   │ (attached)   │<─────────────────└────┬─────┘
                  │                        └──────┬───────┘      attach           │
        PTY 创建失败│                    进程退出 │                                │ close session
                  ▼                              ▼                                ▼
             ┌─────────┐                   ┌──────────┐                    ┌────────────┐
             │ failed  │                   │  exited  │                    │ terminated │
             └─────────┘                   └──────────┘                    └────────────┘
```

**关键不变量：**

| 不变量 | 含义                                                                                |
| --- | --------------------------------------------------------------------------------- |
| I1  | `detached` 状态下 PTY **必须**继续运行，CLI 继续跑                                             |
| I2  | Browser WS 断开 只能触发 detach，绝不能触发 Close                                             |
| I3  | `exited` / `failed` 后 Session 对象保留在内存 **5 分钟**（可配），期间允许 attach 看最后的输出；超时后回收       |
| I4  | `terminated` 是唯一"用户显式杀掉"的状态；与 `exited` 区分，用于审计                                    |
| I5  | Session 与浏览器连接之间**没有任何强引用**：`Session.attached` 是一个 `io.Writer` 接口，nil 就是 detached |

### 7.3 生命周期与浏览器连接的分离（实现层面的保证）

这是整个项目最容易写错的地方，所以给出具体的防错设计：

```go
// internal/session/session.go
type Session struct {
    ID       uuid.UUID
    DeviceID uuid.UUID
    UserID   uuid.UUID

    pty  terminal.Terminal
    ring *buffer.Ring

    mu       sync.Mutex
    attached io.Writer   // ← 关键：只是一个 Writer，不是 *websocket.Conn
    lastSeq  uint64      // 已发送给当前 attached 客户端的最大序号
    ...
}

// Detach 只是把 attached 置 nil。PTY、Ring、读循环全部不受影响。
func (s *Session) Detach(w io.Writer) {
    s.mu.Lock()
    if s.attached == w { s.attached = nil }
    s.mu.Unlock()
    s.lastAttachedAt.Store(time.Now().UnixMilli())
    // 注意：这里不 Close(pty)，不 cancel(ctx)
}
```

**读循环的生命周期属于 Session，不属于连接**：`Session` 创建时起一个 `go s.readLoop(ctx)`，`ctx` 只在 `Close()` 时被 cancel。这样"浏览器断开"物理上不可能终止读循环。

### 7.4 Session 对账（Server 重启 / Agent 重启）

| 场景                          | 结果                | 机制                                                                                                                                                                                                               |
| --------------------------- | ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Browser 刷新 / 断网**         | Session 无损        | detach → attach（I2）                                                                                                                                                                                              |
| **Server 重启**               | Session 无损        | Agent 重连后 `agent.ready` → Agent 主动发 `session.sync`（全量 session 摘要），Server 用它对账 DB：DB 里有、Agent 没报的 → 标记 `terminated(reason=agent_lost)`                                                                            |
| **Agent 重启**                | **Session 全部丢失**  | ⚠️ 这是硬限制：Agent 进程死了，ConPTY/PTY 句柄随之关闭，子进程被回收。**没有可靠的跨进程 PTY 继承方案**（Windows 上尤其不可能）。所以：Agent 启动时把所有 DB 中状态为 running/detached 且属于本设备的 session 标记为 `terminated(reason=agent_restart)`。**必须在文档和 UI 里说清楚，不能让用户以为能恢复** |
| **Server 长时间离线，Agent 一直重连** | Session 继续运行，本地无感 | Agent 侧 session 完全独立于连接                                                                                                                                                                                          |

### 7.5 Session 回收策略

| 策略                        | 默认值   | 说明                                                      |
| ------------------------- | ----- | ------------------------------------------------------- |
| `max_sessions_per_device` | 20    | 超过则拒绝 `session.create`                                  |
| `session_idle_timeout`    | 0（关闭） | detached 超过 N 分钟自动 kill。**默认关闭**，因为"锁屏 10 分钟回来还在跑"是核心卖点 |
| `session_max_lifetime`    | 0（关闭） | 防止忘记关的 session 永远占资源                                    |
| exited session 保留         | 5 min | 允许 attach 看最后的输出（I3）                                    |

### 7.6 多客户端 attach（Q3 确认后新增）

**规则：同账号允许多端同看，跨账号一律拒绝。**

```
Session
 ├─ attached: map[connID]*ClientView   ← 同账号的多个浏览器/手机
 │    ├─ controller: connID            ← 唯一有权 resize 的那个
 │    └─ lastSeq:   map[connID]uint64  ← 每个客户端各自的进度，支持差量补发
 └─ ring: *buffer.Ring
```

| 项 | 设计 | 理由 |
|---|---|---|
| 权限 | attach 时校验 `session.user_id == current_user.id`。**同账号多端 OK，跨账号 403** | JOJO 明确要求；跨账号是硬红线 |
| resize 权威 | **主控客户端（controller）制**：首个 attach 者成为 controller，只有它的 `session.resize` 生效；controller 断开则顺位给下一个 attached | 手机 40 列 + 电脑 120 列同时在线时，若都能 resize，TUI 会来回横跳。必须有一个权威 |
| controller 切换 | 在 `session.attached` / `session.detached` 里下发 `role: controller\|viewer`，前端据此显示「主控」标识、灰掉自己的 resize | 让用户看得见"现在谁说了算" |
| 手动夺取 | 前端提供「接管控制」按钮 → `session.claim_control`，原 controller 降级为 viewer | 手机上想接管电脑的会话时需要 |
| 发送 | 每个客户端一个**独立的有界队列**（§25）。某个客户端慢了只丢它自己的，不影响其他客户端，更不影响 PTY | fan-out 的核心代价必须隔离 |
| 差量补发 | 每个客户端独立记录 `lastSeq`，各自 `since` 语义独立 | 手机断网 10 秒只补它缺的那段 |

> **跨账号的未来扩展**：JOJO 说暂无共享/分享功能。若将来要做，正确做法是新增 `session_shares(session_id, user_id, role)` 表，把授权判断从"属主"扩展为"属主 ∪ 被分享者"，而不是在现有代码里塞 if。

### 7.7 Terminal Mode Tracker（R1 实测后新增，**attach 必需**）

**问题**：实测发现三个 CLI 的模式设置全部集中在启动头 300 字节内（`?1049h` 备用屏幕、`?2027h` 字素簇、`?2004h` 括号粘贴、`?1000~1006h` 鼠标）。用户断线 10 分钟后 attach，浏览器里是一个全新的 xterm.js——它**在主缓冲区**，而应用一直**在备用缓冲区**里画。ring buffer 里早已没有 `?1049h` 那条。**结果是画面直接乱掉。**

**设计**：每个 Session 维护一个极小的增量扫描器，只跟踪「当前处于 ON 的 DEC 私有模式集合」。

```go
// internal/terminal/modes.go
type ModeTracker struct {
    on   map[uint16]struct{}   // 当前 ON 的模式
    st   int                   // 状态机：Normal / Esc / Csi / CsiParam
}

func (m *ModeTracker) Scan(p []byte)          // 增量扫描，只认 ESC [ ? ... h/l
func (m *ModeTracker) Preamble() []byte       // 生成重建当前状态的前导序列
```

**attach 的发送顺序（6 步，顺序不能变）：**

```
1. ESC[?1049l  ESC[2J  ESC[H     ← 先把客户端拉回干净的已知状态
2. Preamble()                     ← 重放当前所有 ON 的模式（含 1049 备用屏幕）
3. ESC[2J  ESC[H                  ← 在正确的缓冲区里清屏
4. ring buffer 尾部字节            ← 重放（从安全边界起，见 R2）
5. ESC[?2026l                     ← 无条件补一个，防同步输出卡死（见下）
6. 进入实时流
```

**第 5 步为什么必需**：Codex 和 OpenCode 都用 `?2026h ... ?2026l` 把整帧包起来防撕裂。**如果 ring buffer 的切片恰好落在一个 2026 块中间**，客户端就会收到 `2026h` 而永远等不到配对的 `2026l` —— xterm.js 停在同步模式，**画面完全冻住，什么都不渲染**。这是低概率但后果严重的 bug，而且**只在重连路径上复现**，日常开发碰不到。

**这不违反「不解析输出」原则**：它只记录模式开关的**存在性**，从不理解内容、不修改字节、不重排序列。原则的注脚应该写明这一点——**「跟踪终端状态」和「理解 CLI 业务内容」是两件事**。

---

## 8. WebSocket Connection Lifecycle

### 8.1 Agent 连接

```
1. Dial wss://<server>/api/v1/ws/agent    (支持 HTTPS_PROXY / 自定义 CA)
2. Upgrade
3. Agent → {"type":"agent.hello",  payload:{protocol:1, device_id, name, platform, arch, agent_version, caps:{max_sessions}}}
4. Server → {"type":"agent.challenge", payload:{nonce:"<32B base64>", server_time:...}}
5. Agent → {"type":"agent.auth",  payload:{signature:"<Ed25519(nonce||device_id||server_time)>"}}
6. Server 校验公钥 → {"type":"agent.ready", payload:{heartbeat_interval, server_time, limits:{max_sessions, max_frame_size, max_buffer_size}}}
7. Agent → {"type":"session.sync", payload:{sessions:[...]}}    ← 对账
8. 进入稳态：心跳 + 双向泵
```

**失败路径**

| 情况                 | 动作                                                                                                 |
| ------------------ | -------------------------------------------------------------------------------------------------- |
| device_id 未注册      | `error{code:"device_not_paired"}` + close 4401 → Agent 打印 pairing 流程提示                             |
| 签名校验失败             | `error{code:"auth_failed"}` + close 4401，Server 记审计日志（含 IP），**不重连**                                |
| nonce 过期/重用        | 同上（防重放）                                                                                            |
| 协议版本不兼容            | close 4426 → Agent 日志提示升级                                                                          |
| 同 device_id 已有活跃连接 | **新连接顶掉旧连接**（旧连接 close 4409 `duplicate_connection`）。理由：Agent 重启/网络切换时旧连接可能还没超时，若拒绝新连接会导致最长 60s 不可用 |

### 8.2 Browser 连接

```
1. POST /api/v1/ws-ticket   (Bearer access_token)  → {ticket, expires_in:30}
2. WS wss://<server>/api/v1/ws/client?ticket=<ticket>
3. Server 校验 ticket（存在 + 未过期 + 未使用 + 绑定 user_id）→ 立即失效该 ticket
4. 校验 Origin 头 ∈ allowed_origins
5. ~~Server → {"type":"client.ready", payload:{user_id, devices:[...], server_time}}~~
6. 之后：JSON 控制消息 + 二进制终端帧
```

> **实现偏差（P5 发现，2026-09-28）**：第 5 步的 `client.ready` **从未实现**。
> `internal/protocol/types.go` 的常量表里没有 `client.ready`，只有 `agent.ready`
> （那是 Server → Agent 握手的一环，方向完全不同）。
> `internal/server/ws_client.go` 在建连后**不下发任何消息**。
>
> 后果与补偿：浏览器建连后自己发 `session.list` 去问设备/会话，
> 也就是把「Server 推」换成了「客户端拉」。功能上等价，代价是多一次往返；
> 好处是 Server 少维护一份「连接建立时要发什么」的状态。
>
> **本文档保留第 5 步并加删除线，是为了留下设计意图的痕迹** ——
> 将来若要补上（比如为了省那次往返），照这里写就行。
> 在那之前，任何依赖 `client.ready` 的代码都会挂掉。

**浏览器侧重连策略**：指数退避 1s → 2s → 4s → … → 30s 上限 + jitter；重连成功后自动重新 `session.attach`（带 `since: lastSeq`），实现"断网 10 秒后无感恢复"。

### 8.3 连接与 Session 的关系矩阵

| 连接               | 断开时对 Session 的影响                                        |
| ---------------- | ------------------------------------------------------- |
| Agent ↔ Server   | Session 全部继续运行（Agent 本地），Server 标记 Agent 离线；Agent 重连后对账 |
| Browser ↔ Server | 当前 attach 的 Session 转 `detached`，PTY 不动（I2）             |

---

## 9. Protocol Design

### 9.1 分层

| 层       | 载体                                     | 用途               |
| ------- | -------------------------------------- | ---------------- |
| Control | WS **Text** Frame，UTF-8 JSON           | 请求/响应/事件，低频      |
| Data    | WS **Binary** Frame，20 字节头 + 裸 payload | 终端字节流，高频、必须二进制透明 |


### 9.2 控制消息信封

```json
{
  "v": 1,
  "type": "session.create",
  "request_id": "01J8ZQ7K3M9P4T2X6B1C0D5E7F",
  "reply_to": null,
  "session_id": null,
  "ts": 1759036000123,
  "payload": { }
}
```

| 字段           | 必填   | 说明                                       |
| ------------ | ---- | ---------------------------------------- |
| `v`          | ✓    | 协议版本，整数。当前 `1`                           |
| `type`       | ✓    | 消息类型，见 §15                               |
| `request_id` | 请求必填 | ULID/UUIDv7，客户端生成，用于 request-response 匹配 |
| `reply_to`   | 响应必填 | 回填被响应请求的 `request_id`                    |
| `session_id` | 可选   | 便于日志与路由（session 相关消息建议都带）                |
| `ts`         | 可选   | 毫秒时间戳，仅作参考，**不用于任何逻辑判断**                 |
| `payload`    | ✓    | 类型相关的对象；无内容时为 `{}`                       |

**为什么用 `request_id`/`reply_to` 而不是统一 `id`**：可读性。日志里一眼能看出哪个是请求哪个是响应，排障成本低。控制消息频率低，多一个字段的开销无所谓。

### 9.3 错误对象（统一结构）

```json
{
  "type": "error",
  "reply_to": "01J8ZQ...",
  "payload": {
    "code": "session_not_found",
    "message": "session does not exist or has been closed",
    "retryable": false
  }
}
```

错误码枚举（MVP）：

```
invalid_message       信封结构错误 / type 未知
invalid_payload        payload 字段缺失或非法
unauthenticated       未认证
forbidden             越权（不属于你的 device/session）
device_offline        目标 Agent 不在线
device_not_paired     设备未绑定
session_not_found
session_limit_reached
cwd_not_allowed        cwd 不在 allowed_workspaces 内
command_not_allowed   命令不在白名单内
session_already_attached
frame_too_large
rate_limited
internal
```

### 9.4 版本协商

- Agent 在 `agent.hello` 带 `protocol: 1`
- Browser **没有** `client.ready` 这条消息（见 §8.2 的实现偏差说明）。版本协商走 `POST /ws-ticket` 的响应体：`{ticket, expires_in, protocol:{min,max}}`，前端在**建连之前**就能拒绝不兼容的页面 —— 这比连上再被 close 4426 更好，省掉一次无谓的握手
- Server 维护 `minSupported`/`maxSupported`。不兼容 → 明确 close code + `error`，不做静默降级

**升级策略**：`v` 只在大改时递增。新增可选字段不动 `v`（向后兼容）；删除/改语义才递增。Server 必须能同时服务 `v=1` 和 `v=2` 的 Agent 至少一个版本周期。

---

## 10. Authentication and Device Pairing

### 10.1 用户认证

| 项             | 方案                                                                                                                           |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| 密码哈希          | **Argon2id**（`golang.org/x/crypto/argon2`），参数 `time=3, memory=64MB, threads=4, keyLen=32`                                    |
| Access Token  | JWT（HS256），TTL **15 分钟**，只放 `sub`(userID) / `exp` / `iat` / `jti`。**不放任何权限声明**（权限每次查库，避免吊销延迟）                                |
| Refresh Token | 不透明随机 32 字节，**存哈希**，TTL 30 天，**每次刷新轮换**（旧 token 立即失效）；检测到已用过的 refresh token 被重放 → 吊销该用户全部 refresh token                      |
| 传输            | Access Token 走 `Authorization: Bearer`（前端存内存，**不放 localStorage**）；Refresh Token 走 `HttpOnly; Secure; SameSite=Strict` Cookie |
| 登出            | 吊销当前 refresh token + 前端清内存                                                                                                   |

**为什么 Refresh Token 走 Cookie 而 Access 走内存**：Cookie 天然防 XSS 读取（HttpOnly），但天然受 CSRF 影响 → 用 `SameSite=Strict` 挡住。Access Token 只活 15 分钟且不落盘，XSS 拿不到持久凭证。

### 10.2 设备身份

Agent 首次运行：

```
1. ed25519.GenerateKey()
2. device_id = UUIDv7()
3. 写入:
   Windows: %APPDATA%\CodeGate\device.key    (0600, 内容 = base64(private||public))
   Linux:   ~/.config/codegate/device.key
   macOS:   ~/Library/Application Support/CodeGate/device.key
4. private key 永不出本机。Server 只存 public_key。
```

**Windows 上的私钥保护**：MVP 用文件权限（NTFS ACL 继承 + `os.Chmod`），并在 `doctor` 里提示"文件权限保护有限"。Phase 8 加固时改用 **DPAPI**（`CryptProtectData`，绑定当前用户账户）。

### 10.3 Pairing 流程

```
Agent                                     Server                          Browser
  │                                          │                                │
  │ 1. agent.pair.begin                      │                                │
  │    {device_id, public_key, name,         │                                │
  │     platform, arch, agent_version}       │                                │
  ├─────────────────────────────────────────>│                                │
  │                                          │ 生成 code（Crockford B32×8）    │
  │                                          │ 存 pairing_codes(code_hash,     │
  │                                          │   device_id, public_key, …,      │
  │                                          │   expires_at=+10min, used_at=null)
  │ 2. agent.pair.code {code, expires_in}     │                                │
  │<─────────────────────────────────────────┤                                │
  │                                          │                                │
  │ 3. 终端打印:                              │                                │
  │    CodeGate Agent                        │                                │
  │    Pair this device: 7F2K-93LM           │                                │
  │    (expires in 10:00)                    │                                │
  │                                          │                                │
  │                                          │ 4. POST /devices/pair {code}    │
  │                                          │<───────────────────────────────┤
  │                                          │ 5. 校验 + 返回设备详情供确认     │
  │                                          │   {name, platform, agent_ip,     │
  │                                          │    already_paired:false}         │
  │                                          ├───────────────────────────────>│
  │                                          │                                │
  │                                          │ 6. 用户点击"确认绑定"            │
  │                                          │    POST /devices/pair/confirm   │
  │                                          │<───────────────────────────────┤
  │                                          │ 7. 写 devices 表, 标记 code 已用 │
  │                                          │    → devices.user_id = me        │
  │ 8. agent.pair.completed {device_id}       │                                │
  │<─────────────────────────────────────────┤                                │
  │ 9. 进入正常连接状态机                      │                                │
```

### 10.4 Pairing 的安全细节（这里有真实的攻击面）

**攻击：Pairing Code 猜解 → 拿下别人的电脑。**

这个攻击很微妙，值得写清楚：攻击者在自己的账号里输入一个猜到的 code。如果 Server 直接按 code 找到 pending 记录并绑定到当前用户，那么**猜中别人的 code 就等于把别人的机器绑到攻击者账号上**——因为 pending 记录里的 `device_id`/`public_key` 属于受害者。

必须叠加的防护：

| 防护                    | 值                                          | 作用                                                               |
| --------------------- | ------------------------------------------ | ---------------------------------------------------------------- |
| Code 空间               | Crockford Base32（去掉 I/L/O/U）8 位 = **2^40** | 猜中概率可忽略                                                          |
| TTL                   | 10 分钟                                      | 缩小窗口                                                             |
| 单次使用                  | `used_at` 非空即失效                            | 防重放                                                              |
| **两阶段确认**             | pair → 展示设备名/平台/IP → confirm               | ★ 关键。即使猜中，用户也会看到"JOJO-PC / Windows 11 / 203.0.113.7"，不是自己的机器就会取消 |
| 限流                    | 每用户 5 次/分钟，每 IP 20 次/分钟，连续失败 10 次锁 30 分钟   | 防暴力                                                              |
| 审计                    | 每次 pair 尝试（成功/失败）都写 audit_logs             | 可追溯                                                              |
| （Phase 8 可选）Agent 侧确认 | Agent 打印 code 后，若收到配对完成通知，本机弹出确认           | 最强，但 MVP 不做                                                      |

**另外两条：**

- code 在 DB 里存 **hash**，不存明文（防 DB 泄露后被用来抢注）
- code 只能被**已登录用户**使用；`allow_signup: false` 时外部无法注册账号来试

### 10.5 授权模型（MVP）

```
User ──1:N──> Device ──1:N──> Session
```

**唯一规则：`session.device.user_id == current_user.id`。**

实现上收敛到一个函数，所有入口都调它：

```go
// internal/server/authz.go
func (s *Server) authorizeSession(u *User, sessionID uuid.UUID) (*SessionRef, error)
func (s *Server) authorizeDevice(u *User, deviceID uuid.UUID) (*Device, error)
```

**纪律**：任何 handler / WS message handler **不允许**自己写 SQL 去查 session/device，必须走这两个函数。这是防 IDOR 最有效的手段（§29 里 IDOR 是重点测试项）。

---

## 11. Security Threat Model

### 11.1 资产与信任边界

| 资产                 | 位置         | 最高威胁             |
| ------------------ | ---------- | ---------------- |
| 用户本地 CLI 的完整控制权    | Agent 所在机器 | 越权访问（A 控制 B 的机器） |
| 终端字节流（可能含密钥、代码、输出） | 内存中        | 中间人窃听、日志泄漏       |
| 设备私钥               | Agent 本机磁盘 | 本地窃取             |
| Refresh Token      | 浏览器 Cookie | XSS / CSRF       |
| Server DB          | Server 磁盘  | 泄露导致设备被伪造        |

**信任边界**：

- Browser ←→ Server：**不可信**（公网，浏览器环境不可控）
- Server ←→ Agent：**不可信传输，但 Agent 身份可信**（Ed25519 认证 + TLS）
- Agent ←→ PTY ←→ CLI：**可信**（同一台机器、同一用户）

**明确不在威胁模型内**：本机已经被攻破（攻击者已在用户机器上能读 `device.key`）、用户自己把自己机器搞坏、恶意 CLI 本身。

### 11.2 STRIDE 分析

| 威胁                  | 具体场景               | 缓解                                                                               |
| ------------------- | ------------------ | -------------------------------------------------------------------------------- |
| **S**poofing        | 伪造 Agent 连上 Server | Ed25519 挑战-应答 + nonce 单次 + 30s TTL（防重放）                                          |
|                     | 伪造浏览器会话            | 短 TTL JWT + refresh 轮换 + 重用检测                                                    |
| **T**ampering       | 中间人改终端字节流          | TLS（WSS）强制；HSTS；禁止降级                                                             |
|                     | 篡改 session 归属      | 每次转发都做 `authorizeSession`，不缓存结论                                                  |
| **R**epudiation     | 谁杀了谁的 session      | audit_logs 记录所有敏感操作（pair、session.create/close、登录失败）                              |
| **I**nfo disclosure | 日志里出现终端内容          | slog handler 里做字段白名单；禁止记录 `stdin`/`stdout`/token 字段名                             |
|                     | DB 泄露 → 伪造设备       | 只存 public_key；私钥永不离开本机                                                           |
|                     | DB 泄露 → 抢注设备       | pairing code 存 hash                                                              |
| **D**oS             | 超大 WS frame        | `SetReadLimit`：控制消息 1 MB，二进制帧 4 MB；超限直接 close 1009                               |
|                     | 高频消息刷爆             | 每连接令牌桶（200 msg/s，突发 400）                                                         |
|                     | 慢消费者拖死 Agent       | 有界发送队列 + 丢弃策略（§25）                                                               |
|                     | 开 10000 个 session  | `max_sessions_per_device`                                                        |
| **E**levation       | IDOR 访问别人 session  | `authorizeSession` 强制走                                                           |
|                     | 路径穿越读整个磁盘          | cwd 白名单 + `EvalSymlinks` + 前缀校验（§18）                                             |
|                     | 命令注入               | **不做 shell 拼接**。`Command`/`Args` 直接传给 `exec.Cmd`，永不经过 `sh -c`。命令来自白名单 ID，不是自由字符串 |

### 11.3 浏览器侧专项

| 项             | 措施                                                                                                                                                                                                                                                          |
| ------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Origin 校验** | WS Upgrade 时校验 `Origin` ∈ `allowed_origins`。缺失或不在列表 → 拒绝（403）                                                                                                                                                                                               |
| **CSRF**      | 状态变更接口全用 `Authorization: Bearer`（天然免疫 CSRF）；唯一用 Cookie 的 `/auth/refresh` 用 `SameSite=Strict`                                                                                                                                                                |
| **XSS**       | xterm.js 把字节流渲染到 canvas/DOM，**不经过 innerHTML**，这是安全的。但要注意：<br />① `WebLinksAddon` 会基于终端文本创建 `<a>`，需限制协议（只允许 http/https）<br />② OSC 8 超链接（终端内嵌 URL）可被恶意 CLI 用来构造钓鱼链接 → MVP 里 xterm.js 的 `linkHandler` 只允许 http(s)，其余忽略<br />③ 前端**禁止**用 `v-html` 渲染任何来自后端的字符串 |
| **点击劫持**      | `X-Frame-Options: DENY` + `frame-ancestors 'none'`                                                                                                                                                                                                          |
| **CSP**       | `default-src 'self'; connect-src 'self' wss://<host>; img-src 'self' data:; script-src 'self'`（Vite 生产构建无 inline script）                                                                                                                                    |
| **Cookie 属性** | refresh cookie：`HttpOnly; Secure; SameSite=Strict; Path=/api/v1/auth`                                                                                                                                                                                       |

### 11.4 命令与环境的边界

| 项    | 规则                                                                                                                      |
| ---- | ----------------------------------------------------------------------------------------------------------------------- |
| 命令来源 | Agent 配置 `allowed_commands`（id → {command, args, cwd 默认}）。Web 只能传 id，或（当 `allow_custom_commands: true` 时）传 command+args |
| 参数拼接 | **绝不** `sh -c` / `cmd /c` 拼接用户输入。`exec.CommandContext(ctx, cfg.Command, cfg.Args...)`                                   |
| 环境变量 | Agent 构造完整 env（继承自身 + `AllowedEnv` 白名单覆盖）。**永不上报 Server**。Web 端不能读取或设置任意环境变量                                            |
| cwd  | 见 §18                                                                                                                   |

### 11.5 安全测试清单（Phase 8 逐项验证）

| #   | 测试                                    | 期望                                     |
| --- | ------------------------------------- | -------------------------------------- |
| S1  | 用户 A 用 B 的 session_id 发 attach        | `forbidden`                            |
| S2  | 用户 A 用 B 的 device_id 创建 session       | `forbidden`                            |
| S3  | 过期 JWT                                | `unauthenticated`                      |
| S4  | 复用已用过的 ws-ticket                      | 拒绝                                     |
| S5  | 复用已用过的 nonce                          | 拒绝（auth_failed）                        |
| S6  | 缺失/伪造 Origin 的 WS Upgrade             | 403                                    |
| S7  | 暴力猜 pairing code                      | 5 次后 `rate_limited`，审计有记录              |
| S8  | 重放已用 pairing code                     | 拒绝                                     |
| S9  | cwd = `C:\Windows\..\Users\other`     | `cwd_not_allowed`                      |
| S10 | cwd 指向 AllowedRoots 内的 symlink，实际指向外部 | `cwd_not_allowed`（EvalSymlinks 后校验）    |
| S11 | cwd = `\\?\C:\Windows`                | `cwd_not_allowed`（拒绝 `\\?\`/`\\.\` 前缀） |
| S12 | 5 MB 单帧                               | close 1009                             |
| S13 | 畸形 JSON / 未知 type                     | `invalid_message`，连接不断                 |
| S14 | 未知 session_id                         | `session_not_found`，不泄漏"存在但不属于你"       |
| S15 | 快速开 100 个 session                     | 第 21 个起 `session_limit_reached`        |
| S16 | 日志全量 grep token/password/stdout       | 无命中                                    |

---


## 12. Go Package Structure

### 12.1 对规范目录结构的三处修改建议

| 规范原文                             | 建议                                                   | 理由                                                                                                             |
| -------------------------------- | ---------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| 有 `pkg/` 目录                      | **删掉 `pkg/`**                                        | Go 官方建议：没有外部消费者就不要 `pkg/`。`internal/` 已经表达了"不对外"的意图                                                            |
| `pkg/web/` 放前端                   | 前端源码放仓根 `web/`；构建产物 embed 到 `internal/server/webui/` | `go:embed` 只能 embed **包目录之下**的文件。前端源码跟 Go 代码混在一起会让 `node_modules` 出现在 module 里，`go build ./...` 变慢、`go vet` 报错 |
| `internal/session/repository.go` | 合并进 `internal/storage/`                              | session 的持久化逻辑属于存储层；Agent 侧根本没有 DB。放在 `internal/session` 会让 Agent 二进制 import 数据库代码                             |

### 12.2 最终目录结构

```
codegate/
├── go.mod                          module github.com/<you>/codegate
├── go.sum
├── Makefile                        build / web / test / race / lint / docker
├── README.md
├── LICENSE
├── .gitignore                      device.key / *.db / web/dist / webui/*
├── .golangci.yml
│
├── cmd/
│   ├── codegate-server/main.go     serve | migrate | version | doctor
│   ├── codegate-agent/main.go      run | pair | status | config | version | doctor
│   └── ttyprobe/main.go            ★ PTY 测试夹具（见 §6.4）
│
├── internal/
│   ├── protocol/                   ★ 两端共享，纯数据 + 编解码，零业务逻辑
│   │   ├── version.go              协议版本常量、兼容性判断
│   │   ├── message.go              控制消息信封 + 所有 payload 结构体
│   │   ├── types.go                消息类型常量 + 方向表
│   │   ├── errors.go               错误码枚举 + CodeError 类型
│   │   ├── codec.go                JSON 编解码（带大小校验）
│   │   ├── binary.go               ★ 二进制帧编解码（20 字节头）
│   │   └── *_test.go               含 fuzz 测试
│   │
│   ├── terminal/                   ★ PTY 抽象层
│   │   ├── terminal.go             Terminal 接口 + StartConfig + Signal + ExitResult
│   │   ├── terminal_windows.go     ConPTY 实现（go-pty adapter）
│   │   ├── terminal_unix.go        Unix PTY 实现（build tag: !windows）
│   │   ├── detect.go               平台能力探测（ConPTY 是否可用）
│   │   ├── signal.go               Signal 类型 + 平台映射
│   │   └── terminal_test.go        integration test（build tag: integration）
│   │
│   ├── buffer/
│   │   ├── ring.go                 ★ 定长环形缓冲 + 序号
│   │   ├── ring_test.go            含 property test / race
│   │   └── doc.go
│   │
│   ├── session/                    ★ 两端都用（Agent 持有实体，Server 只持有引用）
│   │   ├── session.go              Session 结构 + 状态机 + 不变量
│   │   ├── manager.go              Create/Attach/Detach/Close/List/Get/Resize/Send
│   │   ├── state.go                ProcState / AttachState / 对外 status 派生
│   │   ├── ref.go                  SessionRef（Server 侧轻量引用，用于鉴权）
│   │   └── manager_test.go         生命周期测试（重点：detach 不杀 PTY）
│   │
│   ├── agent/
│   │   ├── agent.go                Agent 主结构 + Run 循环
│   │   ├── client.go               WSS 客户端 + 读写泵
│   │   ├── connection.go           连接状态机
│   │   ├── heartbeat.go            心跳 ticker
│   │   ├── reconnect.go            指数退避 + jitter
│   │   ├── identity.go             ★ Ed25519 密钥生成/加载/签名
│   │   ├── pairing.go              pair.begin / pair.code / 打印
│   │   ├── dispatch.go             入站消息分发 → session.Manager
│   │   ├── config.go               Agent 配置结构 + 校验
│   │   ├── workspace.go            ★ AllowedRoots 校验（路径穿越防护）
│   │   ├── commands.go             ★ allowed_commands 白名单解析
│   │   └── *_test.go
│   │
│   ├── server/
│   │   ├── server.go               Server 结构 + 依赖装配
│   │   ├── http.go                 HTTP server + 中间件链
│   │   ├── routes.go               路由表
│   │   ├── ws_agent.go             Agent WS handler
│   │   ├── ws_client.go            Client WS handler
│   │   ├── relay.go                ★ Router / Relay（唯一跨连接通路）
│   │   ├── registry.go             AgentConn / ClientConn 注册表
│   │   ├── authz.go                ★ authorizeSession / authorizeDevice
│   │   ├── handler_auth.go         登录/刷新/登出
│   │   ├── handler_device.go       设备 CRUD + pair
│   │   ├── handler_wsticket.go     一次性票据
│   │   ├── handler_health.go       /healthz /readyz /version
│   │   ├── middleware.go           auth / origin / ratelimit / recover / logging
│   │   ├── ratelimit.go            令牌桶
│   │   ├── webui/embed.go          //go:embed all:dist
│   │   └── *_test.go
│   │
│   ├── auth/
│   │   ├── password.go             Argon2id
│   │   ├── jwt.go                  签发/校验 access token
│   │   ├── refresh.go              refresh token 生成/轮换/重用检测
│   │   ├── pairing.go              pairing code 生成/校验/限流
│   │   └── *_test.go
│   │
│   ├── device/
│   │   ├── device.go               Device 模型
│   │   ├── service.go              绑定/解绑/重命名/在线状态
│   │   └── service_test.go
│   │
│   ├── storage/
│   │   ├── store.go                ★ Store 接口（唯一的抽象点）
│   │   ├── sqlite.go               SQLite 实现
│   │   ├── postgres.go             (Phase 9，可留空文件)
│   │   ├── migrate.go              自写 migrator
│   │   ├── migrations/
│   │   │   └── 0001_init.sql
│   │   └── *_test.go               用临时文件 SQLite 跑真实 SQL
│   │
│   ├── config/
│   │   ├── server.go               Server 配置 + 环境变量覆盖 + 校验
│   │   ├── agent.go                Agent 配置
│   │   └── paths.go                平台默认路径（APPDATA / XDG / Library）
│   │
│   └── logging/
│       ├── logging.go              slog 初始化（JSON handler）
│       ├── redact.go               ★ 敏感字段脱敏（token/password/key）
│       └── logging_test.go
│
├── web/                            Vue 3 前端源码
│   ├── package.json
│   ├── vite.config.ts              dev 时 proxy /api → localhost:8080
│   ├── tsconfig.json
│   ├── index.html
│   └── src/
│       ├── main.ts
│       ├── App.vue
│       ├── router/index.ts
│       ├── stores/{auth,devices,sessions}.ts
│       ├── api/{client.ts,auth.ts,devices.ts}
│       ├── ws/{client.ts,protocol.ts,backoff.ts}
│       ├── terminal/{xterm.ts,fit.ts,keys.ts}
│       ├── views/{Login,Devices,DeviceDetail,Terminal,Settings}.vue
│       ├── components/{DeviceCard,SessionRow,NewSessionDialog,StatusBadge}.vue
│       └── styles/theme.css
│
├── deploy/
│   └── docker/
│       ├── Dockerfile.server       多阶段：node build web → go build → distroless
│       ├── docker-compose.yml
│       └── .env.example
│
└── docs/
    ├── PHASE0-ARCHITECTURE.md      本文档
    ├── PROTOCOL.md                 Phase 1 产出（协议规范独立成文）
    ├── SECURITY.md
    ├── DEPLOYMENT.md
    └── TESTING.md
```

### 12.3 依赖清单（尽量短）

| 依赖                               | 用途                | 备注                                                                       |
| -------------------------------- | ----------------- | ------------------------------------------------------------------------ |
| `github.com/gorilla/websocket`   | WS 服务端/客户端        | 成熟稳定，API 简单。**不用** `nhooyr.io/websocket`（需要自己处理更多底层细节，且我们不需要它的 `wsjson`） |
| `github.com/go-chi/chi/v5`       | HTTP 路由           | 轻量，标准库风格。比 gin 小，比裸 `net/http` 好用                                        |
| `github.com/google/uuid`         | UUIDv7            |                                                                          |
| `github.com/aymanbagabas/go-pty` | ConPTY + Unix PTY | ★ 见 D6。是唯一一个"战略级"依赖，必须评估                                                 |
| `github.com/golang-jwt/jwt/v5`   | JWT               |                                                                          |
| `golang.org/x/crypto`            | Argon2id          |                                                                          |
| `modernc.org/sqlite`             | SQLite（纯 Go）      | 无 cgo，交叉编译友好                                                             |
| `github.com/oklog/ulid/v2`       | request_id        | 可选，也可用 uuidv7 代替                                                         |

**明确不引入**：Redis、Kafka、NATS、gRPC、K8s client、任何 ORM、任何 DI 框架、任何 mock 框架（手写 fake）。

---

## 13. Database Schema

### 13.1 DDL（SQLite 方言，PG 兼容性已标注）

```sql
-- ============ users ============
CREATE TABLE users (
    id            TEXT PRIMARY KEY,              -- UUIDv7 (PG: uuid)
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,                 -- argon2id 编码串
    role          TEXT NOT NULL DEFAULT 'user',  -- user | admin（MVP 只用来控制注册开关）
    disabled      INTEGER NOT NULL DEFAULT 0,    -- PG: boolean
    created_at    INTEGER NOT NULL,              -- Unix ms（PG: timestamptz）
    updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_users_email ON users(lower(email));

-- ============ refresh_tokens ============
CREATE TABLE refresh_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  BLOB NOT NULL,                   -- sha256(token)，不存明文
    family_id   TEXT NOT NULL,                   -- 轮换家族，用于重用检测后整族吊销
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,                         -- 非空 = 已轮换过
    revoked_at  INTEGER,
    user_agent  TEXT,
    ip          TEXT,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_rt_user   ON refresh_tokens(user_id);
CREATE INDEX idx_rt_family ON refresh_tokens(family_id);
CREATE INDEX idx_rt_exp    ON refresh_tokens(expires_at);

-- ============ devices ============
CREATE TABLE devices (
    id            TEXT PRIMARY KEY,              -- Agent 生成的 UUIDv7
    user_id       TEXT REFERENCES users(id) ON DELETE SET NULL,  -- NULL = 未绑定
    name          TEXT NOT NULL,                 -- JOJO-PC
    platform      TEXT NOT NULL,                 -- windows | linux | darwin
    arch          TEXT NOT NULL,                 -- amd64 | arm64
    agent_version TEXT NOT NULL,
    public_key    BLOB NOT NULL,                 -- Ed25519 公钥（32B）
    last_seen_at  INTEGER,
    paired_at     INTEGER,
    revoked_at    INTEGER,
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_dev_user ON devices(user_id);

-- ============ pairing_codes ============
CREATE TABLE pairing_codes (
    id          TEXT PRIMARY KEY,
    code_hash   BLOB NOT NULL,                   -- sha256(code)，不存明文
    device_id   TEXT NOT NULL,                   -- 待绑定的 Agent 设备
    public_key  BLOB NOT NULL,                   -- 从 agent.pair.begin 拿到的公钥
    name        TEXT NOT NULL,
    platform    TEXT NOT NULL,
    arch        TEXT NOT NULL,
    agent_version TEXT NOT NULL,
    agent_ip    TEXT,                            -- 展示给用户确认用
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,
    used_by     TEXT REFERENCES users(id),
    created_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_pc_hash ON pairing_codes(code_hash);
CREATE INDEX idx_pc_exp ON pairing_codes(expires_at);

-- ============ sessions（Server 侧元数据缓存，非真相源）============
CREATE TABLE sessions (
    id               TEXT PRIMARY KEY,           -- UUIDv7
    device_id        TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,              -- "Claude Code" / 用户改名
    command          TEXT NOT NULL,              -- 实际可执行文件
    args_json        TEXT NOT NULL DEFAULT '[]', -- PG: jsonb
    cwd              TEXT NOT NULL,
    status           TEXT NOT NULL,              -- starting|running|detached|exited|failed|terminated
    pid              INTEGER,
    exit_code        INTEGER,
    cols             INTEGER NOT NULL,
    rows             INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    started_at       INTEGER,
    ended_at         INTEGER,
    last_attached_at INTEGER,
    created_by       TEXT REFERENCES users(id)
);
CREATE INDEX idx_sess_device ON sessions(device_id, status);
CREATE INDEX idx_sess_user   ON sessions(user_id, created_at DESC);

-- ============ audit_logs ============
CREATE TABLE audit_logs (
    id         TEXT PRIMARY KEY,
    user_id    TEXT,
    device_id  TEXT,
    session_id TEXT,
    action     TEXT NOT NULL,   -- login.ok|login.fail|device.pair|session.create|session.close|...
    result     TEXT NOT NULL,   -- ok | denied | error
    ip         TEXT,
    user_agent TEXT,
    meta_json  TEXT,            -- 绝不放 token / 终端内容
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_audit_user ON audit_logs(user_id, created_at DESC);
CREATE INDEX idx_audit_act  ON audit_logs(action, created_at DESC);

-- ============ schema_migrations ============
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);
```


### 13.2 三条设计说明

1. **`sessions` 表不是真相源**。真相源永远是 Agent 内存。Server 这张表的作用是：给 Web 列表页一个不用等 Agent 的快速视图 + 留一份"曾经跑过什么"的历史。Agent 重连时通过 `session.sync` 覆盖它。所以**表里的 status 允许短暂过期**，UI 上要能体现"以 Agent 上报为准"。

2. **时间统一存 Unix 毫秒整数**。不用 `DATETIME`/`TIMESTAMP`：SQLite 没有原生时间类型，PG 有——用整数可以在两库之间零转换。展示层负责格式化。

3. **索引只建在真实查询路径上**。MVP 的查询就四种：按 email 查用户、按 user 查设备、按 device 查活跃 session、按 user+时间查审计。其余不建。

### 13.3 SQLite 运行参数

```sql
PRAGMA journal_mode = WAL;       -- 读写并发
PRAGMA busy_timeout = 5000;      -- 写锁等待 5s，避免 "database is locked"
PRAGMA foreign_keys = ON;        -- 默认是关的！
PRAGMA synchronous = NORMAL;     -- WAL 下足够安全且快
```

**连接池约束**：SQLite 单写者。`db.SetMaxOpenConns(1)` 用于写，或干脆全库单连接（MVP 流量下完全够）。**不要**设成 20 然后祈祷——WAL 下多写会 `SQLITE_BUSY`。

---

## 14. MVP API Design

### 14.1 为什么 session 全走 WebSocket（重要设计论证）

| 方案 | 优点 | 缺点 |
|---|---|---|
| **A. session 也走 REST**（`POST /devices/:id/sessions`） | 好测（curl）、语义清晰、可缓存 | ① 请求要经过 REST 认证链 → 再转 WS → 到 Agent，链路更长<br>② Agent 的事件（`session.exit`、`terminal.buffer`）只能从 WS 来 → **前端要同时维护 REST 拉取和 WS 事件两套状态更新**，必然出现状态分歧<br>③ `session.attach` 本身就是"把这条 WS 连接绑到某个 session"，用 REST 表达很别扭 |
| **B. session 全走 WS**（推荐） | 单一状态源；attach/detach 天然与连接绑定；request/response 复用 `request_id` 机制；`session.exit` 事件到达时前端已经在同一条通道上 | 测试要写 WS 客户端（但集成测试本来就要写） |
| **C. 混合** | — | 最差：两套都要维护 |

**结论：选 B。** REST 只保留"登录前/登录态管理"和"设备管理"这类**不依赖 Agent 在线**的操作。

### 14.2 REST 端点

| Method | Path | 认证 | 说明 |
|---|---|---|---|
| POST | `/api/v1/auth/register` | 无 | 仅当无用户存在，或 `allow_signup: true` |
| POST | `/api/v1/auth/login` | 无 | → `{access_token, expires_in}` + Set-Cookie(refresh) |
| POST | `/api/v1/auth/refresh` | Cookie | 轮换 refresh → 新 access + 新 cookie |
| POST | `/api/v1/auth/logout` | Bearer | 吊销 refresh |
| GET | `/api/v1/me` | Bearer | 当前用户 + 权限摘要 |
| POST | `/api/v1/auth/password` | Bearer | 改密码（吊销所有 refresh） |
| GET | `/api/v1/devices` | Bearer | 设备列表（含 `online` 实时字段） |
| GET | `/api/v1/devices/{id}` | Bearer | 设备详情 |
| PATCH | `/api/v1/devices/{id}` | Bearer | 改名 |
| DELETE | `/api/v1/devices/{id}` | Bearer | 解绑（同时踢掉 Agent 连接） |
| POST | `/api/v1/devices/pair` | Bearer | 提交 pairing code → 返回待确认设备信息 |
| POST | `/api/v1/devices/pair/confirm` | Bearer | 二次确认完成绑定 |
| GET | `/api/v1/devices/{id}/sessions` | Bearer | **DB 缓存视图**（快速首屏），前端随后用 WS `session.list` 校正 |
| POST | `/api/v1/ws-ticket` | Bearer | → `{ticket, expires_in:30}` |
| GET | `/api/v1/audit` | Bearer | 分页审计日志 |
| GET | `/healthz` | 无 | 存活探针 |
| GET | `/readyz` | 无 | 就绪（含 DB ping） |
| GET | `/api/v1/version` | 无 | Server 版本 + 协议版本范围 |

**REST 约定**：
- 错误统一 `{"error":{"code":"...","message":"..."}}`
- 分页统一 `?limit=&cursor=`（cursor 是 opaque 字符串，不用 offset）
- 所有时间字段是 RFC3339 字符串（DB 里存整数，出参转换）
- `Authorization: Bearer <access_token>`
- ★ **「不存在」与「存在但不属于你」必须返回完全相同的响应**：
  HTTP **404** + `code: "not_found"` + 同一句 message。
  绝不用 403 —— 403 的语义是「这东西存在，但你不能看」，这句话本身就确认了存在性。
  攻击者拿一批 UUID 逐个请求，按状态码差异就能枚举出哪些 device_id / session_id 真实存在。

  > **这条是 P4 落地时踩出来的，不是设计出来的。**
  >
  > 原实现里 `GET /devices/{id}` 走 `device.Service` → 404，
  > 而 `GET /devices/{id}/sessions` 走 `authorizeDevice` → 403。
  > 同一台「别人的设备」，两个端点给出不同状态码 ——
  > 攻击者只要换一个端点就能把存在性读出来。单看任何一个端点都发现不了。
  >
  > 修法：新增 `protocol.CodeNotFound`（→ 404），把 `authz.errNoAccess` 与
  > `writeDeviceError` 收敛到同一个出口（同状态码 + 同错误码 + 同文案）。
  > 回归测试 `TestDeviceEndpointsAgreeOnForeignDevice` 遍历**所有**设备端点，
  > 断言「别人的设备」与「不存在的设备」逐字节一致 —— 新增端点会自动被覆盖。
  >
  > 教训：要测的是**端点之间的一致性**，不是单个端点的正确性。

### 14.3 WS 端点

| Path | 认证方式 | 用途 |
|---|---|---|
| `/api/v1/ws/agent` | Ed25519 挑战-应答（在连接内完成） | Agent 长连接 |
| `/api/v1/ws/client` | `?ticket=`（一次性） | 浏览器长连接 |

**WS 关闭码约定**（自定义范围 4000–4999）：

| Code | 含义 | 客户端是否重连 |
|---|---|---|
| 1000 | 正常关闭 | 否 |
| 1001 | 服务端要关了（`server_shutdown`） | **是**（常规退避） |
| 1008 | 协议违规（方向非法的消息、帧头损坏、消息类型异常） | 否（对端逻辑错了，重连只会一模一样地失败） |
| 1009 | frame 过大 | 否（是 bug） |
| 1011 | 服务端内部错误（如写库失败） | **是**（对端无过错，重连往往就好） |
| 1013 | 服务端过载 / 慢消费者被踢 | **是**（退避重连） |
| 4401 | 认证失败 | 否（Agent 需重新 pair；Browser 需重新登录） |
| 4403 | 无权限 | 否 |
| 4409 | 重复连接（被新连接顶掉） | 否 |
| 4426 | 协议版本不兼容 | 否 |
| 4429 | 限流 | 是（退避） |

> **这张表是协议的一部分，不是实现细节。** 客户端按它决定「要不要重连」，
> 所以改一个码值等于改了一个客户端的重试策略 —— 而且改错了的表现是
> 「服务端重启后所有 Agent 疯狂重连」或「认证失败的 Agent 永远不重连」，
> 两种都很难从日志里一眼看出来。
>
> **1008 vs 4401 的分工**（P4 落地时明确）：协议层的越界行为断连（1008），
> 业务层的失败回错误消息（`error` 消息 + 连接继续）。前者说明对端在试探或已损坏，
> 继续对话没有意义；后者只是这一次请求不成立。
>
> **关闭帧用 `WriteControl` 而不是走发送队列**：队列满时（正是「慢消费者被踢」
> 这个场景）恰恰最需要能把关闭帧立刻发出去。走队列的话它会排在几百个待发数据帧后面。
> 另外 `closeWithCode` **刻意不调用 `conn.Close()`** —— 主动掐 TCP 会让对端
> 看到 1006（异常关闭），把「服务端主动踢我」误判成「网络断了」，触发不必要的重连。

---

## 15. WebSocket Message Types

### 15.1 消息全表

方向：`A→S` Agent 到 Server，`S→A` 反之，`B→S` 浏览器到 Server，`S→B` 反之。

| type | 方向 | 传输 | payload 要点 | 响应 |
|---|---|---|---|---|
| `agent.hello` | A→S | Text | `protocol, device_id, name, platform, arch, agent_version, caps{max_sessions}` | `agent.challenge` |
| `agent.challenge` | S→A | Text | `nonce`(base64 32B), `server_time` | — |
| `agent.auth` | A→S | Text | `signature`(base64 64B) | `agent.ready` \| `error` |
| `agent.ready` | S→A | Text | `heartbeat_interval, server_time, protocol, limits{...}` | — |
| `agent.heartbeat` | A→S | Text | `sessions:[{session_id, status, attached, cols, rows, pid}]` | — |
| `agent.pair.begin` | A→S | Text | `device_id, public_key, name, platform, arch, agent_version` | `agent.pair.code` |
| `agent.pair.code` | S→A | Text | `code, expires_in` | — |
| `agent.pair.completed` | S→A | Text | `device_id, user_email` | — |
| `device.info` | S→A | Text | 请求上报设备状态 | `device.info` |
| `device.info` | A→S | Text | `os_version, cpu_count, mem_total, hostname, shells:[...]` | — |
| `session.sync` | A→S | Text | `sessions:[SessionSummary]`（全量，用于对账） | — |
| `session.create` | B→S→A | Text | `device_id, name?, command_id \| (command,args), cwd, cols, rows` | `session.created` |
| `session.created` | A→S→B | Text | `session_id, pid, status, cols, rows, created_at` | — |
| `session.list` | B→S→A | Text | `device_id?` | `session.list.result` |
| `session.list.result` | A→S→B | Text | `sessions:[SessionSummary]` | — |
| `session.get` | B→S→A | Text | `session_id` | `session.info` |
| `session.info` | A→S→B | Text | `SessionSummary` | — |
| `session.attach` | B→S→A | Text | `session_id, since`(序号，0=要全量 buffer), `cols, rows` | `session.attached` |
| `session.attached` | A→S→B | Text | `session_id, seq_from, seq_to, status` | — |
| `session.detach` | B→S→A | Text | `session_id, reason` | `session.detached` |
| `session.detached` | A→S→B | Text | `session_id, reason`(`client_close`\|`superseded`\|`idle`) | — |
| `session.close` | B→S→A | Text | `session_id, force?` | `session.closed` |
| `session.closed` | A→S→B | Text | `session_id, reason` | — |
| `session.resize` | B→S→A | Text | `session_id, cols, rows` | 无（fire-and-forget） |
| `session.signal` | B→S→A | Text | `session_id, signal`(`int`\|`term`\|`kill`) | 无 |
| `session.exit` | A→S→B | Text | `session_id, exit_code, signal?, reason` | — |
| `terminal.stdin` | B→S→A | **Binary** | 见 §16 | — |
| `terminal.stdout` | A→S→B | **Binary** | 见 §16 | — |
| `terminal.buffer` | A→S→B | **Binary** | 见 §16（flags 标记 buffer 结束） | — |
| `error` | 双向 | Text | `code, message, retryable, reply_to` | — |
| `ping` / `pong` | 双向 | Text | `{}`（应用层心跳；WS 协议层 Ping/Pong 另有） | — |

### 15.2 核心结构体

```go
type SessionSummary struct {
    SessionID      uuid.UUID `json:"session_id"`
    DeviceID       uuid.UUID `json:"device_id"`
    Name           string    `json:"name"`
    Command        string    `json:"command"`
    Args           []string  `json:"args,omitempty"`
    Cwd            string    `json:"cwd"`
    Status         string    `json:"status"`      // 扁平 6 值
    PID            int       `json:"pid,omitempty"`
    ExitCode       *int      `json:"exit_code,omitempty"`
    Cols           uint16    `json:"cols"`
    Rows           uint16    `json:"rows"`
    CreatedAt      int64     `json:"created_at"`
    StartedAt      *int64    `json:"started_at,omitempty"`
    EndedAt        *int64    `json:"ended_at,omitempty"`
    LastAttachedAt *int64    `json:"last_attached_at,omitempty"`
    BufferSeqFrom  uint64    `json:"buffer_seq_from"`
    BufferSeqTo    uint64    `json:"buffer_seq_to"`
}
```

`BufferSeqFrom/To` 很重要：前端 attach 前就知道"buffer 里还有多少可用"，`session.attached` 里返回的 `seq_from/seq_to` 让前端能判断自己拿到的重放是否完整。

### 15.3 `session.attach` 的 `since` 语义

```
since = 0        → 客户端没有历史，请把 ring buffer 里能给的都给（受 max_replay 限制）
since = lastSeq  → 客户端已有到 lastSeq 的输出，只要它之后的
since < BufferSeqFrom → 客户端落后太多，Agent 只给能给的，并在 session.attached 里
                        告知 seq_from > since（前端应 clear 后重放）
```

这条设计让"手机断网 10 秒后重连"只补差量，而不是每次重放 1 MB。

---

## 16. Binary Terminal Frame Format

### 16.1 帧布局

```
┌─────────┬────────┬────────┬──────────────────────┬───────────────────┐
│ Version │  Type  │ Flags  │     Session ID       │      Payload      │
│  1 byte │ 1 byte │ 2 byte │      16 bytes        │      N bytes      │
│   0x01  │        │        │   (RFC4122 binary)   │  原始字节，不解释   │
└─────────┴────────┴────────┴──────────────────────┴───────────────────┘
 offset 0    1        2-3            4-19                 20..
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `Version` | `uint8` | 当前 `0x01`。不匹配 → 关闭连接（协议不兼容） |
| `Type` | `uint8` | `0x01`=STDIN(C→S→A)、`0x02`=STDOUT(S→C)、`0x03`=BUFFER(S→C 重放)、`0x04`–`0x7F` 保留、`0x80`+ 预留扩展 |
| `Flags` | `uint16` BE | bit0=`BUFFER_END`（重放结束，后续是实时流）；bit1=`DROPPED`（本 session 有字节被丢弃，客户端应做全量重放）；其余保留必须为 0 |
| `SessionID` | 16 bytes | UUID 原始字节（**不是字符串**）。Agent/Browser 发送时必须是已 attach 的 session |
| `Payload` | N bytes | 原始字节。**允许任意二进制，包括无效 UTF-8** |

**总头长 = 20 字节。** 选择 20 而不是 24：16 字节 UUID + 4 字节头，整体 4 字节对齐，`SessionID` 落在 offset 4（对齐位置），便于用 `unsafe` 或直接切片读取（虽然我们不这么做，但心智负担低）。

### 16.2 关于规范里的 `PayloadLength`

规范建议头里带 `PayloadLength`（4 字节）。**建议省略**：

- WebSocket 帧本身已经携带了长度（`frame.Payload` 的长度就是 payload 长度）。再带一个长度字段是冗余，且引入了"两个长度不一致怎么办"的问题（必须校验，又是一处可被攻击的解析点）。
- 如果将来要做**多路复用**（一个 WS 帧里塞多个逻辑帧），那时再加长度字段，并且要同时递增 `Version`。

这是"为未来可能需要而提前增加复杂度"的典型例子，规范第 40 条明确禁止。

### 16.3 为什么必须二进制透明

终端字节流**不是文本**。具体地：

| 情况 | 如果强行当文本处理会怎样 |
|---|---|
| 一个中文字符被 PTY 切成两帧（3 字节 UTF-8 拆成 1+2） | 每帧单独解码 → 两个 U+FFFD 乱码 |
| 4 字节 emoji 被切开 | 同上 |
| Base64 编码 | 体积 +33%，额外 CPU，且完全没有必要 |
| JSON 包裹 | 每个字节要转义检查，高频输出下是灾难；且无法表达任意字节 |
| `string(b)` 转换后再 `[]byte(s)` | 多一次分配 + 复制，且不保证字节不变（Go 里是安全的，但心智上是错的） |

**实现纪律**：从 PTY 读出的 `[]byte` 到 WS `WriteMessage`，中间**只允许**发生：一次头拼接（`append` 到预分配缓冲）+ 入队。不允许出现 `string()`。

### 16.4 编解码实现要点

```go
const (
    HeaderLen  = 20
    Version1   = 0x01
)

const (
    TypeStdin  uint8 = 0x01
    TypeStdout uint8 = 0x02
    TypeBuffer uint8 = 0x03
)

const (
    FlagBufferEnd uint16 = 1 << 0
    FlagDropped   uint16 = 1 << 1
)

// Encode 写入 dst（要求 cap(dst) >= HeaderLen+len(payload)）。
// 返回完整帧。调用方应复用缓冲（sync.Pool），避免高频分配。
func Encode(dst []byte, typ uint8, flags uint16, sid uuid.UUID, payload []byte) []byte

// Decode 只做边界检查，不复制 payload（返回的是 src 的子切片）。
// 调用方在使用期间不得复用 src。
func Decode(src []byte) (typ uint8, flags uint16, sid uuid.UUID, payload []byte, err error)
```

**必须的校验**（防资源耗尽 / 崩溃）：
1. `len(src) < HeaderLen` → `ErrShortFrame`
2. `src[0] != Version1` → `ErrVersionMismatch`
3. `typ` 不在已知集合 → `ErrUnknownType`
4. `flags & ^knownFlags != 0` → `ErrBadFlags`（严格拒绝未知 flag，避免未来语义歧义）
5. 入站方向校验：Browser 只能发 `TypeStdin`；Agent 只能发 `TypeStdout`/`TypeBuffer`。**方向错误直接断连**（这是防"客户端伪造 stdout 注入"的关键）

### 16.5 Fuzz 测试

```go
func FuzzDecode(f *testing.F) {
    // seed: 正常帧、短帧、错版本、错类型、超长 payload、全 0、全 0xFF
    f.Fuzz(func(t *testing.T, b []byte) {
        _, _, _, _, err := protocol.Decode(b)
        // 要求：不 panic、不 OOM、err 或成功二选一
    })
}
```

这是 Phase 1 的必交付项。



---

## 17. Major Technical Risks

按"会不会让项目失败"排序。

### R1 — ConPTY 能不能承载这些 TUI（**已实测，风险解除**）

> **状态更新 2026-09-28：已在真机上实测完成，结论是「能」。完整报告见 `docs/PHASE0-R1-CONPTY-FINDINGS.md`。**
> 方法：用 Python ctypes 直接调 ConPTY（不需要 Go），在本机实际安装的 Claude Code 2.1.235 / Codex 0.157.1 / OpenCode 1.18.32 上捕获原始字节并用自写 VT 解析器还原屏幕。

**实测结论：**

| 项 | 结果 |
|---|---|
| Codex 全屏 TUI（ratatui） | ✅ 完整渲染，box-drawing / 选择符 / Unicode 省略号逐字正确 |
| OpenCode 全屏 TUI（opentui，备用屏幕） | ✅ 完整渲染，ASCII logo / `█▓░▀▄╹┃` / 输入框 / 状态栏全对 |
| Claude Code（inline 模式） | ✅ 完整渲染（本轮因代理 502 未进入完整 TUI，待补） |
| `ResizePseudoConsole` | ✅ 子进程实测看到 `100x30` → `60x20` |
| 宽字符 / 中文 / emoji | ✅ 未被破坏 |
| ConPTY 是否改写字节流 | ✅ 未见改写（但**不保证**，见下） |

**但实测挖出 3 个必须处理的坑（这才是真正的收获）：**

1. 🔴 **父进程 std 句柄会被子进程继承，输出绕开伪控制台。** 父进程 stdout 一旦被重定向（管道/服务环境），子进程 stdout 就跟着走，ConPTY 只吐自己的初始化序列。**Agent 作为 Windows Service 运行时必然踩到。** 修复：`CreateProcessW` 前后把父进程三个 std 句柄临时置 NULL（已实测有效）。
2. 🔴 **不设 `TERM`，Codex 直接拒绝进入 TUI**（`TERM=dumb` → 只输出 411 字节并弹确认框；设为 `xterm-256color` 后 3,976 字节完整 TUI）。Agent 必须白名单构造子进程环境，不能整体继承。
3. 🟠 **备用屏幕与模式状态在 attach 时丢失** → 新增 `ModeTracker` 组件（§7.7），attach 时先重放模式前导序列。附带解决了 `?2026h` 同步输出未配对导致的**终端画面冻死**问题。

**风险等级调整：从「最高风险」降为「已验证」。** Phase 2 的任务从「探索可行性」变成「把 Python 探针的能力搬进 Go，并补齐剩余分支」：

- T1：在网络正常的机器上验证 Claude Code 的完整 TUI（备用屏幕路径）
- T2：用真实 xterm.js + 浏览器确认 7 条终端能力查询哪些有应答（Phase 6）
- T3：用 PowerShell / cmd 确认 `0x03` 在 shell 场景的语义

**仍未证伪的残留风险**：ConPTY 内部确实会重绘，本次样本里没有观察到改写，但这不等于「永不改写」。**测试矩阵仍要保留**（vim / htop / `git log` 分页 / 超大输出），Phase 2 逐项过。

**为什么最初会把它当最高风险**：这个判断是对的——只是现在有证据了，而不是靠猜。

### R2 — Attach 重放的保真度

**问题**：ring buffer 里存的是原始字节流，重放给一个新的 xterm.js 实例时，如果起点落在一条 escape sequence 的中间（或某个全屏重绘的中间），渲染结果可能与真实状态不同。全屏 TUI 尤其明显。

**缓解**：
1. 重放前先发 `\x1b[2J\x1b[H`（清屏 + 归位），减少残留
2. **对齐到"安全边界"**：扫描 buffer 尾部，从最后一个 `\x1b[` 之前的完整字节位置开始重放（避免从转义序列中间切）
3. 文档明确：**重放是 best-effort，不保证 100% 还原 TUI 状态**。要精确还原就得在 Agent 侧跑 VT 屏幕模拟器（Phase 7 的备选方案）
4. 前端 attach 后提供 "Clear & Resize" 按钮，让用户手动触发一次全屏重绘（大多数 TUI 收到 resize 会重绘，这是个实用的小技巧：**发一个 cols-1 再发回 cols 的 resize，可强制 TUI 重绘全屏**）

### R3 — 慢消费者拖死 Agent

**问题**：用户跑 `make -j8`，PTY 每秒吐几 MB；浏览器在手机弱网。如果 Agent 的 PTY 读循环因为写不出去而阻塞，**CLI 本身会被阻塞**（PTY 缓冲写满 → 子进程 write 阻塞）。这是最危险的失败模式。

**缓解**（§25 详述）：PTY 读循环**永不阻塞**。ring buffer 永远写（内存定长，无阻塞），实时流走有界 channel，满了就丢 + 标记 + 通知客户端重放。

### R4 — Ctrl+C / 信号语义

**问题**：Windows 没有 POSIX 信号，ConPTY 也没有公开的"注入 Ctrl+C"API。

**缓解**：统一用"写 0x03 到 PTY 输入"表达中断。这在控制台程序上前台等价于 Ctrl+C；在 raw mode TUI 里它就是一个字节（由应用自己处理，这正是透明传输的正确行为）。Phase 2 用 ttyprobe + 真实 Ctrl+C 场景验证。**Unix 侧不受影响**（真实 `kill(-pgid, SIGINT)`）。

### R5 — Agent 重启必然丢 Session

**问题**：Agent 进程退出 → PTY/ConPTY 句柄关闭 → 子进程被回收。没有可靠的跨进程 PTY 接管方案。

**缓解**：
1. **接受它，并说清楚**。UI 上明确提示"Agent 重启会终止所有会话"
2. 用 Job Object（Windows）/ 进程组（Unix）保证**不泄漏孤儿进程**——宁可干净地杀掉，也不要留下一堆管不到的 conhost
3. Agent 启动时对账：把所有 `running`/`detached` 的旧 session 标记为 `terminated(reason=agent_restart)`，并保留最后 buffer（如果做了持久化——MVP 不做）
4. Phase 2+ 可选：把 ring buffer 落盘（`session_buffer` 表或文件），让重启后至少能 attach 看到最后的输出（进程已死，状态为 exited）

### R6 — 中继规模与内存

**问题**：数千条 WS 长连接，每条连接 1 个读 goroutine + 1 个写 goroutine + 有界 channel。Go 的 goroutine 很便宜（初始 2-8 KB），但 channel + buffer 是实打实的内存。

**粗算**：Agent 连接：2 goroutine × 4KB + send chan 1024 × 平均帧 1KB = **约 1 MB/连接**（buffer 是主要开销）。1000 个 Agent 连接 ≈ 1 GB。**这不可接受**。

**缓解**：
- send channel **不存 `[]byte`，存引用**；或者改用**环形队列 + 复用缓冲**（`sync.Pool`）
- 降低队列深度：1024 → 256，同时把 `max_frame_size` 控制在 64 KB（终端输出单帧通常 < 4 KB）
- 重新估算：256 × 4KB = 1 MB 仍是上限，但实际占用取决于瞬时排队量（空闲时为 0）
- **Phase 4 结束后必须做一次实测**：用脚本开 500 条假 Agent 连接，`pprof` 看 RSS。目标：**< 100 KB/空闲连接**

### R7 — 企业网络 / TLS 拦截

**问题**：Agent 在受管企业机器上，出站 HTTPS 被 TLS 中间人拦截，Go 默认校验失败。

**缓解**：支持 `SSL_CERT_FILE` / `SSL_CERT_DIR`（Go 原生支持）+ Agent 配置 `extra_ca_file`。`codegate-agent doctor` 里加一项连通性诊断，明确报出是 TLS 校验失败还是 DNS 失败还是代理失败。**不做** `InsecureSkipVerify` 开关（除非显式配置且日志警告）。

### R8 — resize 风暴 / TUI 抖动

**问题**：拖窗口时 ResizeObserver 高频触发 → `ResizePseudoConsole` 高频调用 → ConPTY 全屏重绘 → 大量输出 → 网络拥塞。

**缓解**：前端 debounce 120ms + 值变化判断；Agent 侧对同一 session 的 resize 做 **50ms 节流 + 只保留最后一次**（合并中间态）。

### R9 — SQLite 写并发

**问题**：`database is locked`。

**缓解**：WAL + `busy_timeout=5000` + **单写连接**。所有写操作走一个串行化路径。MVP 的写频率很低（登录、pair、session 元数据），完全够。

### R10 — 超大输出导致 ring buffer 频繁覆盖

**问题**：`make -j8` 一次输出 50 MB，1 MB 的 ring buffer 被覆盖 50 次。用户断线回来只看到最后 1 MB。

**缓解**：这是设计取舍，不是 bug。但要让用户**可感知**：`Flags.DROPPED` + UI 提示"部分输出已被丢弃"。同时 `session_buffer_size` 可配到 10 MB。

### R11 — 前端 xterm.js 内存泄漏

**问题**：路由切换/反复 attach 导致 terminal 实例、ResizeObserver、WebSocket 未释放，长时间使用后浏览器卡死。

**缓解**：`onUnmounted` 严格清理（`dispose()` + `observer.disconnect()` + `ws.close()`）；`scrollback` 设为 10000 行上限（不要无限）。Phase 5 用 DevTools Memory 面板验证。

### R12 — 协议演进的兼容性

**问题**：Server 升级后旧 Agent 连不上。

**缓解**：`v` 字段 + Server 同时支持多个版本至少一个发布周期 + `agent.hello` 时就拒绝不兼容版本并给出**人类可读的升级提示**（不是默默断开）。

---

## 18. Development Milestones

| Phase | 目标 | 主要交付物 | 退出标准（必须全绿） |
|---|---|---|---|
| **P0** | 架构设计 | 本文档 | 你确认 5 个待决问题 + R1 的验证计划 |
| **P1** | 骨架 + 协议 + 核心数据结构 | `go.mod`、`internal/protocol`（含 fuzz）、`internal/buffer`、`internal/session`（纯逻辑，无 PTY）、CI 脚本 | `go test ./...` + `go test -race ./...` 全绿；协议编解码有 fuzz 覆盖；ring buffer 有 property test |
| **P2** | **Windows ConPTY PoC（关键风险关卡）** | `internal/terminal` + `ttyprobe` + `cmd/conpty-poc` 演示程序 | ★ 用 ttyprobe 验证：创建/读/写/resize/Ctrl+C/退出码；★ **人工验证矩阵**：PowerShell、`vim`、Claude Code、`git log` 分页、中文/emoji、连续 resize 无错乱 |
| **P3** | Agent | `internal/agent` 全套 + `cmd/codegate-agent` | 连上本地假 Server（P1 里写的测试用 WS server）→ 认证 → 心跳 → 自动重连（kill 掉 Server 再起，Agent 30s 内恢复） |
| **P4** | Server | `internal/server` + `internal/auth` + `internal/device` + `internal/storage` + `cmd/codegate-server` | REST 全端点有测试；`/ws/agent` 与 `/ws/client` 可连；pairing 全流程跑通；IDOR 测试（S1/S2）通过；R6 的内存实测 |
| **P5** | Web | `web/` 完整前端 | `npm run build` + `npm run lint` 通过；能登录、看设备、看会话列表；xterm.js 能渲染本地 mock 数据 |
| **P6** | 端到端打通 | 联调修复 | ★ **Browser → Server → Agent → PowerShell，能输入能输出能 resize 能 Ctrl+C** |
| **P7** | Detach / Attach / Buffer | `session.attach` 的 `since` 语义、`FlagBufferEnd`、DROPPED 处理 | ★ 关浏览器 → CLI 继续跑 → 重开 → attach 恢复最近输出；手机锁屏 10 分钟场景手工验证 |
| **P8** | **Workspace 文件传输**（2026-09-28 新增） | 浏览 / 预览 / 下载 / 上传。见 `docs/PHASE0-FILE-TRANSPORT.md` | FS1–FS10 全绿；Server 内存不随文件大小增长；下载 100 MB 时终端交互延迟 < 200ms |
| **P9** | 安全加固 | §11.5 的 S1–S16 + 文件传输的 FS1–FS10 + `SECURITY.md` | S1–S16 与 FS1–FS10 全绿；日志脱敏测试通过；golangci-lint 无高危项 |
| **P10** | Docker 部署 | `deploy/docker/*` + `DEPLOYMENT.md` | `docker compose up` 一条命令起服务；SQLite 数据卷持久化；HTTPS 反代示例可用 |
| **P11** | 文档 + Release | `README.md`、`PROTOCOL.md`、`TESTING.md`、GitHub Release + 交叉编译产物 | Windows/Linux/macOS × amd64/arm64 六个 Agent 二进制可下载；README 的 Quick Start 能照做跑通 |

> **里程碑调整记录（2026-09-28）**：JOJO 提出需要"看工作区文件 + 下载 + 上传"，插入为新 Phase 8；原 P8/P9/P10 顺延为 P9/P10/P11。这样它刚好排在安全加固之前，能被安全测试覆盖。

**跨 Phase 的持续要求**（规范第 39 条）：
- 每个 Phase 开工前：说明目标 / 要改哪些文件 / 设计方案 / 潜在风险
- 每个 Phase 收尾：`go test ./...`，必要时 `-race`；前端 `npm run build` + `npm run lint`；然后报告完成内容 / 测试情况 / 已知问题 / 下一阶段

---

## 19. MVP Acceptance Criteria

### 19.1 主场景（规范第 42 条，逐条可验证）

| # | 步骤 | 通过标准 |
|---|---|---|
| A1 | 在 Windows PC 上运行 `codegate-agent run` | 打印 pairing code，随后显示 `Connected to <server>` |
| A2 | 浏览器打开 CodeGate，登录 | 能看到设备列表 |
| A3 | 设备列表显示 `JOJO-PC ● Online Windows 11 Agent 0.1.0` | 状态实时（Agent 断开 60s 内变 Offline） |
| A4 | 点击设备 → New Session → 选 workspace + 命令 → Start | 5 秒内出现完整原生 CLI 界面 |
| A5 | 在终端里输入 `dir` / `ls` | 输出正确，颜色保留 |
| A6 | 运行 Claude Code，正常交互 | TUI 布局正确，无错乱，无重影 |
| A7 | 中文 + emoji 输出 | 无乱码，宽度正确（Unicode11） |
| A8 | 按 Ctrl+C | 中断当前命令，CLI 不退出。**⚠️ 仅限 TUI 会话**，见下方说明 |
| A9 | 拖窗口改变大小 | TUI 重新布局，无错乱 |
| A10 | 在 vim 里编辑、`:wq` | 全屏 TUI 正常，方向键/Home/End/PageUp 正常 |
| A11 | 手机锁屏 10 分钟 → 重新进入 → Attach | **Claude Code 仍在运行，且能继续交互** |
| A12 | 关闭浏览器标签页 | Session 变 `Detached`，CLI 继续运行 |
| A13 | 重启 Server 容器 | Agent 60 秒内自动重连，Session 不受影响 |
| A14 | 拔网线 30 秒再插上 | Agent 自动重连，无需人工干预 |
| A15 | 关闭 Session | PTY 进程被终止，状态变 `terminated` |

> **A8 的实测修正（2026-09-28）**：Windows 上 Ctrl+C 的效果**取决于会话类型** ——
>
> - **TUI 会话**（Claude Code / Codex / vim / htop）：符合原描述。这些程序启动时自己设了 raw mode，
>   0x03 作为普通字节送达，由应用自己解释。
> - **shell 会话**（cmd / PowerShell / bash）：**无法接收中断信号**。conhost 会拦截 0x03，
>   结果是子进程 stdin EOF 而命令并不中断。此时前端把 Ctrl+C 按钮置灰，改提供「终止会话」。
>
> 完整原因、三条实测路径与前端兜底方案见 §6.5；实测证据见 `docs/PHASE2-CONPTY-VERIFICATION.md` §4。

### 19.2 稳定性 / 性能门槛

| # | 项 | 门槛 |
|---|---|---|
| B1 | 空闲连接内存 | < 100 KB / 连接（500 条并发实测） |
| B2 | 单次按键到回显延迟 | 同城公网 < 150 ms（p95） |
| B3 | 单 Agent 并发 Session | 20 个稳定运行（不 OOM、不错乱） |
| B4 | 大输出 | `yes | head -c 50000000`（50 MB）→ 浏览器不崩、Server 不 OOM、Agent 不阻塞 CLI |
| B5 | 连续 resize | 快速拖窗口 30 秒 → 无错乱、无连接断开、无内存增长 |
| B6 | 长时间运行 | 连续挂 24 小时 → 内存无持续增长（< 10% 漂移） |
| B7 | 竞态 | `go test -race ./...` 全绿 |
| B8 | 二进制体积 | Agent < 15 MB（strip 后） |

### 19.3 安全门槛

| # | 项 | 门槛 |
|---|---|---|
| C1 | §11.5 的 S1–S16 | 全部通过 |
| C2 | 日志审计 | `grep -riE "(password\|token\|private_key\|stdin)"` 在日志里无敏感值命中 |
| C3 | 越权 | 两个账号交叉测试，任何跨账号访问都返回 `forbidden` |
| C4 | 路径 | `../`、symlink、`\\?\`、UNC、8.3 短名 全部被拒 |
| C5 | TLS | 强制 HTTPS/WSS，HTTP 请求 301 到 HTTPS，HSTS 头存在 |

### 19.4 明确不在 MVP 验收范围内的

文件管理、文件编辑、代码编辑器、Git GUI、AI Chat UI、Desktop GUI、移动原生 App、P2P/WebRTC/QUIC、Wake-on-LAN、多人协作、Session 分享、终端录制、E2EE、插件系统、复杂 RBAC、Linux/macOS 的完整验证（Phase 2 之后补齐）、Windows Service / systemd / launchd 后台化。

---

## 20. 移动端 UX 设计（Q5 确认后新增，MVP 必做）

JOJO 的使用场景：**手机为主，上班时才用电脑操控家里机器。** 所以「手机上能不能顺畅操作 Claude Code」直接决定这个项目对主用户有没有价值 —— 这不是"以后再说"的优化项。

### 20.1 桌面终端在手机上为什么不可用

| 问题 | 具体表现 |
|---|---|
| 软键盘没有 Ctrl / Esc / Tab / 方向键 | Claude Code 的 `/` 命令、`Esc` 中断、`Tab` 补全全部用不了 |
| 软键盘遮住半个屏幕 | 100x30 的 TUI 在 6 寸屏上被键盘压到只剩 10 行 |
| 没有鼠标滚轮 | 无法翻阅输出 |
| 组合键无处输入 | `Ctrl+C` / `Ctrl+D` / `Ctrl+L` 这些是终端日常操作 |
| 触摸没有 hover | TUI 的选中态、菜单高亮全部失效 |

**结论：手机端不能是"缩小版的桌面终端"，必须是"为触摸重新设计的外壳 + 终端内核"。**

### 20.2 移动端必备的四个设计

**① 专用按键条（最关键的单项）**

终端下方固定一行可横向滚动的按键，点击即注入对应字节：

| 按键 | 注入 | 说明 |
|---|---|---|
| `Esc` | `0x1B` | TUI 里的"返回/取消"，手机上最缺 |
| `Tab` | `0x09` | 补全 |
| `Ctrl` | 粘滞修饰键 | 点亮后按下一个字母 → 发 `Ctrl+字母`。解决组合键问题 |
| `↑` `↓` `←` `→` | `ESC[A/B/C/D` | 命令历史 / 菜单导航 |
| `Enter` | `0x0D` | 软键盘的回车有时不触发，需要独立按键兜底 |
| `^C` | `0x03` | 中断（**长按 600ms 才生效**，防误触——对应 Q2 的误触顾虑） |
| `Ctrl+L` | `0x0C` | 清屏 |

按键条可折叠（`.mobile-keys` 组件），横屏时自动收起。

**② 键盘弹出时重算终端尺寸**

软键盘弹出/收起会改变可视高度 → `ResizeObserver` 触发 → `fit()` → resize。
但**手机上这个 resize 是高频且剧烈的**，必须复用 §5.4 的 debounce，并且：

> ⚠️ **手机上 resize 与主控客户端制（§7.6）会打架**：手机上键盘一弹一收就 resize 两次，如果手机是 controller，桌面端的会话布局会跟着抖动。
> **解决**：手机端默认以 **viewer** 身份 attach，不自动成为 controller。要接管得显式点「接管控制」。这样桌面端（真正在跑 Claude Code 的那台）保持稳定。

**③ 全屏接管 + 安全区适配**

- 终端页进入后 `position: fixed` 占满视口（**注意：这是 Web 应用内的布局，不是 CSS 的 fixed 定位限制，前端实现时用 flex 全高即可**）
- `viewport-fit=cover` + `env(safe-area-inset-bottom)` 给 iPhone 底部横条留位
- 禁用双击缩放（`touch-action: manipulation`），否则点两下会放大
- 横屏时按键条收起、字号自动缩小

**④ 断线重连要无感**

手机锁屏 = 浏览器进程被挂起 = WebSocket 必然断。
- 回到前台（`visibilitychange` → visible）立刻重连 + 自动 `session.attach`（带 `since: lastSeq`）
- 不要弹"连接已断开，是否重连"的对话框 —— 直接静默恢复
- 恢复期间终端顶部显示一条细的状态条（`Reconnecting…` / `Reconnected`），2 秒后淡出

### 20.3 明确不做

移动原生 App、手势驱动的 TUI 交互（如滑动选择菜单）、移动端专属的 AI Chat UI。**MVP 的手机端就是「能用的终端 + 好用的按键条」。**

---

## 21. CLI 启动、Workspace 与对话恢复（Q2 确认后新增）

### 21.1 命令白名单与预置

Agent 配置里维护 `allowed_commands`，Web 端只传 `command_id`：

```yaml
allowed_commands:
  - id: powershell
    name: "PowerShell"
    command: "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe"
    args: ["-NoLogo"]
  - id: pwsh
    name: "PowerShell 7"
    command: "pwsh.exe"
  - id: cmd
    name: "Command Prompt"
    command: "cmd.exe"
  - id: wsl
    name: "WSL"
    command: "wsl.exe"
  - id: claude
    name: "Claude Code"
    command: "claude.exe"
    resume_args: ["--continue"]        # ★ 见 21.3
    resume_pick_args: ["--resume"]
  - id: codex
    name: "Codex"
    command: "codex.exe"
    resume_args: ["resume", "--last"]
  - id: opencode
    name: "OpenCode"
    command: "opencode.exe"
    resume_args: ["--continue"]
allow_custom_commands: false           # 默认关；要开得改本机配置 = 本机用户自己授权
```

**预置原则**：装了什么就显示什么。Agent 启动时探测命令是否存在（`exec.LookPath`），只把**真实可用的**推给 Web 端 —— 不要让用户点了才发现没装。

### 21.2 关闭 Session 必须二次确认（Q2 明确要求）

JOJO 的原话是担心"界面不小心给 Ctrl+C 关掉啥的"。这里要分清两件完全不同的事：

| 操作 | 危险度 | 交互 |
|---|---|---|
| **Ctrl+C**（`0x03`） | **低** —— 只是中断当前命令，CLI 本身不退出 | 按键条上直接可点，但**长按 600ms 生效**，防误触 |
| **关闭 Session** | **高** —— 进程被终止，工作丢失 | **两次点击**：点「关闭」→ 弹出确认（显示 session 名 / cwd / 命令 / 运行时长）→ 再点「确认关闭」。确认按钮默认焦点**不在**确认上 |
| **关闭全部 / 设备解绑** | **极高** | 二次确认 + 要求**输入 session 名**才能确认 |

前端不要用 `window.confirm()`（丑、在移动端体验差、且无法展示上下文），做一个内联的确认卡片。

### 21.3 对话恢复：这是两个不同的「恢复」

JOJO 要的"恢复之前的对话有个恢复按钮"，**不是**终端 buffer 重放。必须把两件事分开：

| | A. 画面恢复 | B. 对话/上下文恢复 |
|---|---|---|
| 恢复的是什么 | 终端里最后那段**输出画面** | CLI 自己的**会话上下文**（聊天历史、文件状态） |
| 靠什么实现 | Agent 的 ring buffer 重放（§7.7 的 6 步） | **CLI 自身的 resume 机制** |
| 进程要求 | 进程还活着 | 进程可以已经退出 |
| CodeGate 的角色 | 传输 + 缓存字节 | **只是往命令行里加一个参数** |

**B 的实现（MVP 就要做）**：

```
New Session 对话框：
  命令: [Claude Code ▾]
  Workspace: [D:\Projects\CodeGate ▾]
  ☑ 恢复上次对话        ← 勾上则 args += resume_args
  [启动]
```

对应命令：`claude --continue` / `codex resume --last` / `opencode --continue`。

**这完全符合「不解析 CLI 业务内容」的原则** —— CodeGate 不知道对话里有什么，它只知道「这个 CLI 支持一个 resume 标志」这一条**配置元数据**（写在 `allowed_commands` 里，由用户/Agent 配置提供）。

**两个细节**：
1. **`--continue` 和 `--resume` 不一样**：前者续最近一次，后者弹出选择列表。所以配置里区分 `resume_args`（一键续）和 `resume_pick_args`（列出历史让用户选）。
2. **cwd 必须一致**：这些 CLI 的会话是按项目目录索引的。在 `D:\Projects\A` 里 resume 只能拿到 A 的对话。所以**恢复按钮必须和 Workspace 绑定**，UI 上要显示"将在 `D:\Projects\CodeGate` 恢复上次对话"。

> **待验证（Phase 2）**：这三个 CLI 的 resume 参数与行为需要各自实测确认。上面写的是常见形式，不能当成既成事实直接写死。

---

## 22. 低内存部署模式（Q4 确认后新增）

JOJO 的服务器内存小。所以 Server 必须能在一台小机器上跑得住。

| 项 | 措施 |
|---|---|
| 数据库 | **只用 SQLite**（纯 Go，无 cgo）。不上 PostgreSQL —— PG 一个实例的基础内存开销就超过整个 Server |
| 单进程 | 一个 Go binary 干完 HTTP + WS + 内嵌前端，没有 Nginx 常驻（可选） |
| 连接内存 | ★ 见 R6：把每连接的有界队列从 1024 降到 256，帧上限 64 KB；用 `sync.Pool` 复用缓冲。**目标 < 100 KB / 空闲连接** |
| 缓冲复用 | PTY 读缓冲、WS 写缓冲都用 `sync.Pool`，避免高频输出时产生 GC 压力 |
| SQLite 调优 | WAL + `busy_timeout=5000` + **单写连接**（§13.3） |
| 内存上限 | 支持 `GOMEMLIMIT`（Go 1.19+ 的软内存上限）配置项，让 Server 在小机器上不 OOM |
| 日志 | 滚动文件 + 大小上限，不要无限增长（小服务器磁盘也小） |
| Docker | 多阶段构建 → `distroless/static`，最终镜像 ~20 MB（Go 静态二进制 + 内嵌前端） |
| 无中间件 | 不引 Redis / Kafka / NATS。限流用进程内令牌桶；一次性 ticket 用进程内 map + 定时清理 |

**Phase 4 必须做的实测**：开 500 条假 Agent 连接，用 `pprof` 看 RSS。**不达标就先优化再往下走** —— 这是 R6 的验收门槛，不是"以后再优化"。

---

## 附录 A — 推荐 / 备选 决策汇总（规范要求的"为什么选推荐方案"）

| 决策 | 推荐 | 备选 | 核心理由 |
|---|---|---|---|
| 数据库 | SQLite(纯 Go) | PostgreSQL | 零运维 + 无 cgo + 表少查询简单；迁移成本可控 |
| DB 访问 | 手写 SQL + 单一 Store 接口 | sqlc / ORM | 6 张表不值得引入代码生成或 ORM |
| Windows PTY | go-pty（ConPTY） | 手写 CreatePseudoConsole | 省掉 ConPTY 句柄/属性列表/Close 顺序的坑；接口是自己的，可随时替换 |
| 控制消息通道 | session 全走 WS | REST + WS 混合 | 避免双写状态分歧；attach 语义天然属于连接 |
| WS 鉴权 | 一次性 ticket | JWT 放 query / Cookie | 长期凭证不进 URL |
| Agent 鉴权 | Ed25519 挑战-应答 | 长期 bearer token | 规范要求 keypair；Go 里成本极低；DB 泄露无法伪造 |
| 二进制帧头 | 20 字节（无 length 字段） | 24 字节（带 length） | WS 已提供长度，冗余字段是多余的解析攻击面 |
| Ring buffer | 定长环形数组 + 序号 | 分块链表 | 一次分配、无 GC 抖动；序号支持差量重放 |
| Buffer 重放 | 原始字节尾部重放 | Agent 侧 VT 屏幕模拟器 | MVP 简单优先；代价（TUI 还原不精确）已记录为 R2 |
| 多客户端 attach | 同账号 fan-out + 主控客户端制 | 单 attach 新顶旧 / 全员可 resize | JOJO 要求多端同看；resize 必须有唯一权威，否则布局横跳 |
| 前端渲染 | DOM/canvas（默认） | WebGLAddon | 避免 context lost 与泄漏 |
| 前端分发 | go:embed 进 Server | 独立 Nginx / 容器 | 部署 = 一个二进制 |
| 对话恢复 | 用 CLI 自身的 resume 机制（`claude -c` 等） | 靠终端 buffer 重放还原 | 见 §21.3：buffer 重放只能还原**画面**，还原不了**对话上下文**；两者是两件事 |
| 移动端 | 提升为 MVP 必做（专用按键条 + 全屏接管） | 只做响应式 | 见 §20：JOJO 手机使用频率高，桌面式终端在手机上不可用 |
| 迁移工具 | 自写 migrator | golang-migrate / goose | 少一个依赖 |
| 状态模型 | 内部两轴 + 对外扁平 | 纯扁平枚举 | 状态机自洽，同时保持 API 兼容 |
| `Wait()` 返回 | `ExitResult` | `error` | 必须区分"正常非 0 退出"与"PTY 故障" |
| 路由 | chi | gin / 裸 net/http | 标准库风格、依赖小 |

---

## 附录 B — 参考资料 / 待验证清单

Phase 1–2 需要实测确认的技术点（**不要凭记忆下结论**）：

| # | 待验证 | 验证方式 |
|---|---|---|
| V1 | ConPTY 承载 Claude Code TUI 是否无错乱 | P2 人工矩阵 |
| V2 | ConPTY 是否存在可用的 passthrough 模式 | 查 MS 文档 + 实测注册表/标志 |
| V3 | 写 0x03 在 PowerShell / TUI 中的实际行为 | ttyprobe + 真实 CLI |
| V4 | `go-pty` 的 resize 在 ConPTY 上是否真正生效 | ttyprobe 打印 cols/rows |
| V5 | `go-pty` 是否暴露退出码 | 读源码 |
| V6 | ConPTY 下中文/emoji 的宽度计算 | 实测 + xterm Unicode11 对比 |
| V7 | `modernc.org/sqlite` 在 Windows 上的性能与并发表现 | P4 压测 |
| V8 | 500 条 WS 连接的实测 RSS | P4 pprof |
| V9 | 浏览器 WebSocket 在移动端锁屏后的存活行为（iOS Safari 会主动断开） | P7 真机测试 |

---

## 附录 C — 需要你确认的事项

1. **Q1–Q5**（见 §0.3）：注册策略、自定义命令、多客户端 attach、部署环境、移动端优先级。
2. **R1 的处理方式**：我建议 Phase 2 单独作为"风险关卡"，PoC 不通过就停下来重新评估。你同意这个节奏吗？
3. **Go 环境**：当前机器上没有 Go。Phase 1 开始前需要装 Go 1.24+（我可以代劳）。
4. **模块路径**：`go.mod` 的 module path 用什么？（例如 `github.com/jojo/codegate`）—— 影响所有 import 路径，越早定越好。
5. **本文档是否需要产出英文版**？开源项目通常需要，但可以等 P10 再补。
