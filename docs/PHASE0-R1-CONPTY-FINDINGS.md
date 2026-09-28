# CodeGate — R1 实测报告：ConPTY 到底能不能承载这些 TUI

> 日期：2026-09-28 · 状态：**已验证**
> 关联：`docs/PHASE0-ARCHITECTURE.md` §17 R1/R4
> 工具：`tools/conpty_probe.py`、`tools/ansi_report.py`、`tools/scan_tui.py`

---

## 0. 一句话结论

**R1 通过。** ConPTY 能正确承载 Claude Code / Codex / OpenCode 的 TUI，字节流完整、可还原、resize 生效。

但实测暴露出 **4 个必须在 Agent 里显式处理的坑**，其中 2 个是「不处理就完全跑不起来」级别的（F1、F2），1 个会直接决定重连能不能用（F5）。

| # | 发现 | 严重度 | 状态 |
|---|---|---|---|
| F1 | 父进程 std 句柄会被子进程继承，输出**绕开**伪控制台 | 🔴 致命 | 已定位 + 已给出修复 |
| F2 | 不设 `TERM`，Codex 直接拒绝进入 TUI | 🔴 致命 | 已验证 |
| F5 | 备用屏幕 + 模式状态在 attach 时丢失 | 🟠 高 | 已定位，需新增设计 |
| F6 | 目标 CLI 会主动查询终端能力，且**在等回答** | 🟠 高 | 已定位，需在 Phase 6 用真 xterm.js 验证 |
| F3 | resize 正确传递到子进程 | ✅ 通过 | — |
| F4 | `0x03` 的语义**取决于子进程的控制台模式**：raw mode 下作为字符投递，cooked mode 下被 conhost 拦截并导致 stdin EOF | 🟠 高 | ✅ Phase 2 已收尾（2026-09-28），见 §7 |

---

## 1. 方法

没有 Go 也能做——**用 Python ctypes 直接调 ConPTY**（`CreatePseudoConsole` / `ResizePseudoConsole` / `ClosePseudoConsole` + `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE`）。

这么做的额外好处：**拿到的字节流与未来 Go 实现拿到的字节流是同一份东西**，结论可直接迁移，不依赖 `go-pty` 是否可靠。

三件工具：

| 工具 | 作用 |
|---|---|
| `tools/conpty_probe.py` | 起 ConPTY、喂输入、resize、捕获原始字节到 `.bin` |
| `tools/ansi_report.py` | 模式时间线 / 能力查询清单 / 用极简 VT 解析器还原最终屏幕 |
| `tools/scan_tui.py` | 扫二进制，识别 TUI 框架与终端模式协商特征 |

环境：Windows 11，100x30 伪控制台，本机实际安装的 Claude Code 2.1.235 / Codex 0.157.1 / OpenCode 1.18.32。

---

## 2. F1 — 父进程 std 句柄会被继承，输出绕开伪控制台（🔴 最严重）

### 现象

子进程启动正常、退出码 0、输出完全正确 —— **但一个字都没进伪控制台**。ConPTY 只吐出它自己的两条初始化序列：

```
捕获 = 16 字节 = b'\x1b[?9001h\x1b[?1004h'
```

（`?9001h` = Win32 输入模式，`?1004h` = 焦点上报 —— 这是 Windows 11 ConPTY 开机就发的，与子进程无关。）

### 定位

让子进程自报 `GetStdHandle(STD_OUTPUT_HANDLE)` 的状态：

| 对照组 | 子进程 stdout 是控制台？ | 缓冲区尺寸 |
|---|---|---|
| A：原样（父进程 stdout 是管道） | ❌ `False` | `0x0` |
| B：创建前把父进程三个 std 句柄置 NULL | ✅ `True` | **`100x30`**（= 我们设的伪控制台尺寸） |

### 结论

> **`CreateProcessW` 会把父进程的 STD_INPUT/OUTPUT/ERROR 句柄复制给子进程；只要父进程某个槽位不是 NULL，伪控制台就【不会】覆盖它。**
> 结果就是子进程的 stdout 指向父进程的管道/文件，而不是伪控制台。

### 为什么这对 CodeGate 是致命的

Agent 在生产环境会以 **Windows Service** 方式运行（无控制台，std 句柄可能指向 `nul` 或别处），`codegate-agent run` 前台运行时 stdout 又可能是被重定向的日志管道。
**两种情况都会让所有会话的输出去向错误的地方。**

### 修复（已验证）

在 `CreateProcessW` 前后把父进程的三个 std 句柄临时置 NULL：

```go
// Go 实现（对应 tools/conpty_probe.py 里 start() 的 null_parent_std 分支）
const (
    stdInput  = ^uint32(9)   // -10
    stdOutput = ^uint32(10)  // -11
    stdError  = ^uint32(11)  // -12
)

saved := [3]uintptr{}
for i, slot := range []uint32{stdInput, stdOutput, stdError} {
    saved[i], _, _ = procGetStdHandle.Call(uintptr(slot))
    procSetStdHandle.Call(uintptr(slot), 0)
}
created := CreateProcessW(...)
for i, slot := range []uint32{stdInput, stdOutput, stdError} {
    procSetStdHandle.Call(uintptr(slot), saved[i])
}
```

**工程注意**：这段是进程级全局状态修改，必须放在**互斥锁**里；同时 Agent 自己的日志不能走 stdout（写文件/slog 到 stderr 也可能被影响）——建议 Agent 日志一律写文件，stdout 只用于 `--foreground` 的终端展示，且该窗口内不写日志。

---

## 3. F2 — 不设 `TERM`，Codex 直接拒绝渲染（🔴）

### 现象

```
WARNING: TERM is set to "dumb". Codex's interactive TUI may not work in this terminal.
Continue anyway? [y/N]:
```

捕获只有 411 字节 / 9 个 ESC —— **它根本没进 TUI**。

设 `TERM=xterm-256color` 后重跑：**3,976 字节 / 350 个 ESC**，完整 TUI 出来了。

### 结论

> **Agent 必须为子进程显式设置终端环境变量，不能简单继承自身环境。**

Agent 需要设置的（`StartConfig.Env` 里强制覆盖，不给用户/上层覆盖的机会）：

| 变量 | 值 | 说明 |
|---|---|---|
| `TERM` | `xterm-256color` | 不设 → Codex 拒进 TUI；`dumb` → 同上 |
| `COLORTERM` | `truecolor` | 让 CLI 敢用 24 位色 |
| `LINES` / `COLUMNS` | 当前 rows / cols | 部分程序在非 tty 场景下读它 |
| `CODEGATE=1` | 可选 | 给 CLI 一个可探测的标记 |

反向要求：**不能把 Agent 自己的环境整体透传**（安全要求见架构文档 §11.4），要按白名单构造，再强制覆盖上表。

---

## 4. F3 — 三个 CLI 的渲染栈（静态证据 + 动态验证）

### 静态：二进制特征扫描

| CLI | 版本 | 体积 | 运行时 | 渲染栈 | 判定依据 |
|---|---|---|---|---|---|
| **Claude Code** | 2.1.235 | 326 MB | **Bun**（单文件编译） | 自有渲染层（未确认 Ink） | `Bun`/`bun:ffi`/`Bun.version` 命中；JS 明文可读（`require(` ×2164、`use strict` ×1569、`Anthropic` ×1010）。**`react-reconciler` 与 `yoga-layout` 零命中** → 若真用 Ink 不应如此，待确认 |
| **Codex** | 0.157.1 | 322 MB | **Rust** | **ratatui + crossterm** | `ratatui` ×20、`crossterm` ×31、`unicode-width` ×13；无任何 JS 运行时特征 |
| **OpenCode** | 1.18.32 | 180 MB | **Bun** | **opentui**（Zig 核心 + TS 上层） | `opentui` ×415、`@opentui` ×31、`zig` ×292、`YogaNode` ×9 |

> ⚠️ **踩过的坑，记下来免得再犯**：`ink` 作为子串在 `link` / `thinking` / `blinking` 里到处出现，第一次扫描 Claude Code 报了 8752 次「Ink」，纯属噪音。
> **判断框架必须用无歧义标记**（`react-reconciler`、`yoga-layout`、`@opentui`），不能拿短通用词当特征。

### 动态：ConPTY 实测渲染

`tools/ansi_report.py` 的 VT 解析器把字节流还原成了屏幕，**逐字正确**：

**Codex**（备用屏幕内，box-drawing + 选择符）：
```
+----------------------------------------------------------------------------------------------------+
|  Folder access                                                                                     |
|  C:\Users\JOJO\AppData\Local\Temp\cg\work                                                          |
|  Note: You're in a subdirectory of a Git project. Trusting will apply to the                       |
|  repository root:  c:\users\jojo\appdata\local\temp\cg\work                                        |
|  Trust this folder? Codex can read, edit, and run files here, subject to your                      |
|  permission settings. ...                                                                          |
|› 1. Trust and continue                                                                             |
|  2. Back to Agent Command Center                                                                   |
|  enter continue · esc back                                                                         |
+----------------------------------------------------------------------------------------------------+
```

**OpenCode**（备用屏幕 + ASCII logo + 输入框 + 状态栏）：
```
+----------------------------------------------------------------------------------------------------+
|                     █▀▀█ █▀▀█ █▀▀█ █▀▀▄ █▀▀▀ █▀▀█ █▀▀█ █▀▀█                                        |
|                     █  █ █  █ █▀▀▀ █  █ █    █  █ █  █ █▀▀▀                                        |
|                     ▀▀▀▀ █▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀                                        |
|   ┃                                                                                                |
|   ┃  Ask anything… "Fix a TODO in the codebase"                                                    |
|   ┃  Sisyphus - Ultraworker · Nemotron 3 Super (free) OpenRouter                                   |
|   ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀                      |
|                                                   tab agents  ctrl+p commands                      |
|  ~\AppData\Local\Temp\cg\work                                         1.18.32                      |
+----------------------------------------------------------------------------------------------------+
```

**结论**：ConPTY 的字节流是**完整可还原**的，宽字符（`█▓░▀▄╹┃`）、Unicode 省略号、box-drawing 都没被破坏。**R1 的主要风险解除。**

> 补充：Claude Code 这一轮走的是 **inline 模式**（无 `?1049h`），因为它连不上 Anthropic（环境里的 `https_proxy` 返回 502）。它渲染了欢迎界面 + ASCII 图 + 错误提示，同样完整。Claude Code 的完整 TUI 路径需要在网络正常的机器上再补一次验证 —— **这是 R1 唯一没走完的分支**。

### 顺带发现（对 CodeGate 有影响）

Claude Code 用 **`\x1b[1C`（CUF 光标前移）代替空格**来排版：

```
Welcome\x1b[1Cto\x1b[1CClaude\x1b[1CCode\x1b[1Cv2.1.235
```

这不是异常，是终端应用的正常优化。但它再次证明：**任何对字节流的"整理"都会破坏布局** —— 不能把 `\x1b[1C` 归一化成空格，也不能做任何形式的行重建。这条要写进代码评审红线。

---

## 5. F5 — 备用屏幕与模式状态在 attach 时会丢（🟠 需要新增设计）

### 现象

OpenCode 启动时立刻进备用屏幕，**模式设置全部集中在开头的 300 字节内**：

```
@    0  SET  Win32输入模式(9001)          ← ConPTY 发的
@  300  SET  备用屏幕+清屏(1049)          ← 应用发的，之后所有绘制都在备用屏幕里
@  315  SET  字素簇处理(2027)
@  323  SET  括号粘贴(2004)
@  331  SET  鼠标-点击(1000)
@  339  SET  鼠标-拖动(1002)
@  347  SET  鼠标-移动(1003)
@  355  SET  鼠标-SGR扩展(1006)
@  683  SET  同步输出(2026)
```

Codex 同样：`@319 SET 备用屏幕+清屏(1049)`。

### 为什么这是问题

用户断线 10 分钟回来，浏览器里是一个**全新的 xterm.js 实例**，它：
- 在**主缓冲区**，而应用一直在**备用缓冲区**里画
- 没开 `2027` 字素簇、没开 `2004` 括号粘贴、没开鼠标上报
- 我们从 ring buffer 重放的是**尾部字节**，`?1049h` 那条在 300 字节处，早被覆盖掉了

结果：**重放的内容画到主缓冲区，之后应用的实时输出也画到主缓冲区 —— 而应用以为是备用屏幕。画面直接乱掉。**

### 设计补充：Terminal Mode Tracker（新增组件）

Agent 在每个 Session 里维护一个**极小的增量扫描器**，只做一件事：跟踪当前处于 ON 状态的 DEC 私有模式集合。

```go
// internal/terminal/modes.go
type ModeTracker struct {
    // 只在见到 ESC [ ? ... h/l 时更新，其余字节一律跳过（零拷贝，状态机只有 4 个状态）
    on map[uint16]struct{}
}

// Scan 返回本段数据中出现的模式变更（用于实时同步给已连接的客户端，可选）
func (m *ModeTracker) Scan(p []byte) []ModeChange

// Preamble 生成"重建当前终端状态"的前导序列
func (m *ModeTracker) Preamble() []byte   // ESC[?1049h ESC[?2027h ESC[?2004h ESC[?1000h ... 
```

**attach 时的发送顺序**：

```
1. ESC[?1049l  ESC[2J ESC[H     ← 先让客户端回到干净的已知状态
2. Preamble()                    ← 重放当前 ON 的所有模式（含 1049 备用屏幕）
3. ESC[2J ESC[H                  ← 在正确的缓冲区里清屏
4. ring buffer 尾部字节           ← 重放
5. ESC[?2026l（若 2026 处于 ON）  ← 防同步输出卡死（见下）
6. 进入实时流
```

**这不违反"不解析输出"原则**：它只记录模式开关的**存在性**，从不理解内容、不修改字节。写进架构文档 §43 原则的注脚。

### 连带发现：同步输出（2026）卡死风险

Codex 和 OpenCode 都用 `?2026h ... ?2026l` 把整帧包起来防撕裂。**如果 ring buffer 的切片恰好落在一个 `2026` 块中间**：

- 重放出去的是 `2026h` 而没有配对的 `2026l`
- 客户端的 xterm.js 进入同步模式并**永远等不到结束标记**
- 表现：**终端画面完全冻住，什么都不渲染**

这是低概率但后果严重的 bug，而且**只有在重连路径上才复现**，日常开发根本碰不到。上面第 5 步（无条件补一个 `2026l`）就能解决。

---

## 6. F6 — 目标 CLI 会主动查询终端能力，并且在等回答（🟠）

### 实测到的查询序列（OpenCode，全在启动头 500 字节内）

| 序列 | 含义 | 期望的回应 |
|---|---|---|
| `ESC[>0q` | **XTVERSION** 问终端名字/版本 | `ESC[>|名称(版本)` |
| `ESC[?u` | **Kitty 键盘协议**查询 | `ESC[?{flags}u` |
| `ESC]99;i=...;p=?;ESC\` | Kitty 通知协议探测 | OSC 99 应答 |
| `ESC]1337;Capabilities ESC\` | iTerm2 私有能力查询 | OSC 1337 应答 |
| `ESC]66;w=1; ESC\` | Kitty 文本尺寸协议探测 | OSC 66 应答 |
| `ESC[14t` ×2 | 文本区**像素**尺寸 | `ESC[4;{高};{宽}t` |
| `ESC]4;0;?BEL` | 调色板颜色 0 查询 | `OSC 4;0;rgb:...` |

Claude Code 也发 `ESC[>0q`（XTVERSION）。

### 为什么重要

CodeGate 的 Server 和 Agent 是**字节透明中继**，这些查询会原样透传到浏览器，**必须由 xterm.js 来回答**。

- 终端答不上来 → 应用走 fallback：可能降级配色、禁用某些特性、**甚至判定终端能力不足而拒绝渲染**（Codex 的 `TERM=dumb` 就是同类问题的极端例子）。
- 这些序列一旦被中继层吞掉或改写，应用就会卡在等应答上。

### 待办（Phase 6 必须做）

用**真实的 xterm.js + 真实浏览器**重跑同样的 CLI，逐条确认哪些查询有应答、哪些没有。已知需要重点确认：

| 查询 | xterm.js 支持情况 | 应对 |
|---|---|---|
| `CSI c` / `CSI > c`（DA1/DA2） | 支持 | — |
| `CSI 5n` / `CSI 6n`（DSR） | 支持 | — |
| `CSI > 0 q`（XTVERSION） | **待确认** | 若不支持，前端需要挂一层应答钩子（`xterm.parser.registerCsiHandler`） |
| `CSI ? u`（Kitty 键盘） | **待确认** | 若不支持，需评估对按键编码的影响 |
| `CSI 14 t`（像素尺寸） | **待确认** | xterm.js 知道单元格像素尺寸，可以回答 |
| `OSC 4 ; n ; ?`（调色板查询） | **待确认** | 可以从当前 theme 反查 |

> 如果确认 xterm.js 有缺口，实现方式是**在前端加一个极薄的「终端能力应答层」**（`registerCsiHandler` / `registerOscHandler`），**不是**在 Server 或 Agent 里做 —— 保持中继层纯粹。这条要写进架构文档。

---

## 7. F4 — `0x03` 的语义（✅ Phase 2 已收尾，2026-09-28 修正）

> **⚠️ 本节原结论已被 Phase 2 实测推翻一半。** 原文说「0x03 被当作输入字符投递」——
> 那只对 **raw mode** 成立。cooked mode 下子进程收到的**不是字符 3，而是 EOF**。
> 下面保留原始实测记录，并在其后给出修正。

### 原始实测（Phase 0）

往 ConPTY 输入管道写 `0x03`，用 Python 的 `msvcrt.getwch()` 观察：

| 观察 | 结果 |
|---|---|
| 子进程能否收到这个按键 | ✅ `msvcrt.getwch()` 返回 `'\x03'`（`ORD=3`） |
| 是否触发 `CTRL_C_EVENT` → `KeyboardInterrupt` | ❌ **否** |

### ★ Phase 2 修正：语义取决于子进程的控制台模式

用 `cmd/ttyprobe` 复测（探针会报告自己的控制台输入模式），结论如下：

| 子进程模式 | 谁在用 | 写 0x03 的结果 |
|---|---|---|
| **cooked**（默认，`PROCESSED_INPUT` 开着） | `cmd.exe`、PowerShell | ❌ 字节被 conhost 拦截 → 子进程 **stdin EOF**，命令**不中断** |
| **raw**（应用自己关掉 `PROCESSED_INPUT`） | vim、Claude Code、Codex、OpenCode | ✅ 0x03 作为字节送达，进程存活，应用自己解释 |

- ConPTY 默认输入模式实测为 `0x01f7`（含 `PROCESSED_INPUT`）。
- Phase 0 测的全是 TUI —— 它们启动时会自己设 raw mode，所以只看到了「字符投递」这一面。
- cooked mode 下的完整现象：探针根本没读到 0x03，stdin 直接 EOF，进程以 exit code 0 正常退出；
  对真实 `cmd.exe` 则是「ping 不中断 + shell 不再接受任何输入」。

### 备选路径的实测结果（两条都试过了）

| 路径 | 结果 |
|---|---|
| `AttachConsole(pid)` + `GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)` | ❌ **API 全部返回成功但信号无效**。用 `GetConsoleProcessList` 确认 `AttachConsole` 真的生效（列表含 cmd/ping/测试进程），排除了「Agent 不是该控制台成员」这个原猜想 —— 问题不在调用姿势，在 ConPTY 实现本身 |
| `conpty.dll!ConptyGenerateConsoleCtrlEvent`（Windows Terminal 用的私有接口） | ⚠️ 需随包分发 `conpty.dll`；开发机上不存在，无法验证 |

### 结论

> **Windows 上「发 Ctrl+C」= 往 PTY 写一个 0x03 字节。它对 raw mode 程序有效，
> 对 cooked mode（shell）无效且会破坏会话。系统自带的 ConPTY 无法可靠中断 shell 进程。**

**处置（JOJO 2026-09-28 决策）：前端兜底** —— shell 会话的 Ctrl+C 按钮置灰，改提供「终止会话」；
不在 MVP 引入 `conpty.dll`。完整方案见架构文档 §6.5，证据见 `docs/PHASE2-CONPTY-VERIFICATION.md` §4。

架构文档里的结论更新为：`Terminal.Signal(SIGINT)` 在 Windows 上仍实现为「写 0x03」，
但**调用方（前端）必须按会话类型决定是否暴露这个动作**。

---

## 8. 其他观察

| 观察 | 含义 |
|---|---|
| ConPTY 启动即发 `ESC[?9001h`（Win32 输入模式） | 它希望客户端按 `INPUT_RECORD` 格式回传按键。xterm.js **不会**发这种格式 → 需要确认 ConPTY 在收不到 win32 输入模式应答时是否仍接受普通 VT 按键（实测**接受**，因为我们写 `0x03` 生效了） |
| 三个 CLI 都开 `ESC[?1004h`（焦点上报） | 浏览器标签页切换会触发焦点事件。**这其实是好事**：切回标签页时 TUI 会重绘 |
| Claude Code 用 `ESC[?2031h`（主题变更通知） | 配合深色/浅色模式。前端 theme 切换时可以主动通知它 |
| Codex 启动带 `WARNING: failed to clean up stale arg0 temp dirs: 拒绝访问。(os error 5)` | 与 CodeGate 无关，是本机权限问题 |
| Claude Code 报 `Failed to connect to api.anthropic.com: Status 502, A proxy is configured via https_proxy` | 本机 `https_proxy` 指向坏代理（已知问题）。**Agent 若整体继承环境，会把这个坏代理一起传下去** → 印证了 F2 的结论：环境变量必须白名单构造，不能整体透传 |

---

## 9. 对架构文档的影响（已同步）

| 影响 | 落到哪 |
|---|---|
| Agent 创建 PTY 前必须临时置空自身 std 句柄 | 架构 §6.2 Windows 实现要点 |
| Agent 必须白名单构造子进程环境并强制覆盖 `TERM`/`COLORTERM`/`LINES`/`COLUMNS` | 架构 §11.4、§17 |
| 新增 `ModeTracker` 组件 + attach 的 6 步发送顺序 | 架构 §7（Session Lifecycle） |
| 前端可能需要「终端能力应答层」 | 架构 §5.3、§20 |
| R1 状态从「最高风险」降级为「已验证，剩 Claude Code 完整 TUI 分支待补」 | 架构 §17 |
| Phase 2 的验收标准改为「跑通本报告的复现步骤」 | 架构 §18 |
| **（Phase 2 新增）Ctrl+C 对 shell 无效** → 前端按会话类型决定是否暴露该动作 | 架构 **§6.5**（新增章节） |
| **（Phase 2 新增）子进程退出后 `Read` 不 EOF** → 退出顺序改为 Wait → 静默窗口 → Close | 架构 §6.2 第 3 点 |
| **（Phase 2 新增）ttyprobe 移到 `cmd/`** | 架构 §6.4、§12.2 |

---

## 10. 待办清单

| # | 事项 | 何时 |
|---|---|---|
| T1 | 在**网络正常**的机器上验证 Claude Code 的完整 TUI（备用屏幕路径） | Phase 2 |
| T2 | 用真实 xterm.js + 浏览器逐条确认 F6 的 7 个查询 | Phase 6 |
| T3 | ~~用 PowerShell / cmd 确认 `0x03` 在 shell 场景的语义~~ ✅ **已完成（2026-09-28）**：确认对 shell **无效且会破坏会话**，见 §7 | Phase 2 |
| T4 | 确认 Claude Code 的渲染层到底是不是 Ink（`react-reconciler` 零命中，存疑） | 有空再看，不影响开发 |
| T5 | ~~把 `conpty_probe.py` 的能力搬进 Go~~ ✅ **已完成（2026-09-28）**：交付为 `cmd/ttyprobe/` | Phase 2 |
