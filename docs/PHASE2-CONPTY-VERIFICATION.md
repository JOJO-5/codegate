# Phase 2 — ConPTY 验证报告

> 日期：2026-09-28
> 环境：Windows 10.0.26100（Windows 11 24H2）、Go 1.27.1
> 对应里程碑：`docs/PHASE0-ARCHITECTURE.md` §18 的 **P2 — Windows ConPTY PoC（关键风险关卡）**
> 验证代码：`cmd/ttyprobe/`（本次新增）

---

## 0. 结论摘要

**主体通过，但发现 1 个必须处理的缺陷。**

| 项 | 结论 |
|---|---|
| 创建 / 读 / 写 / 退出码 | ✅ 通过 |
| resize（含连续 resize） | ✅ 通过 |
| UTF-8 / 宽字符字节透明 | ✅ 通过 |
| 退出后资源回收 | ✅ 通过（附一条重要平台差异，见 §5.2） |
| **Ctrl+C 中断（shell 场景）** | ❌ **不通过** —— 见 §4 |

**这条缺陷的影响范围有限但真实**：它**不影响** CodeGate 的主用例（Claude Code / Codex 这类全屏 TUI），但**会让 shell 会话的 Ctrl+C 既无效又破坏会话**。验收标准 A8 在 shell 场景下不满足。

**这正是 P2 作为「风险关卡」的价值** —— 它在写 Agent 之前就卡住了一个真问题，而不是等 Phase 6 联调时才发现。

---

## 1. 本次新增的交付物

### 1.1 `cmd/ttyprobe/` —— PTY 测试夹具

架构文档 §6.4 要求的核心交付物，本次补齐。它做四件事：

1. 报告伪控制台尺寸（`SIZE cols=80 rows=24`）
2. **报告 stdin 的控制台输入模式**（`MODE stdin=0x01f7 …`）—— 见 §4.2，这一项是定位 Ctrl+C 问题的钥匙
3. 尺寸变化时报告（`RESIZE cols=60 rows=20`）
4. 回显 stdin（`ECHO …`）、报告 0x03（`CTRL_C byte=0x03`）、支持 `width`/`raw`/`exit` 命令

**为什么不用「跑个 ls 看输出对不对」来测 PTY**：`ls` 的输出随版本、TERM、locale、颜色配置而变，断言它必然脆弱，而且它测的是 `ls` 不是 PTY。探针报告的是 **PTY 自身的属性**（窗口尺寸、控制台模式、字节保真度、信号语义），与任何外部命令的行为无关。

**与文档的一处偏离**：文档建议放在 `internal/terminal/testdata/ttyprobe/`，实际放在 `cmd/ttyprobe/`。理由是 **Go 工具链会完全忽略 `testdata` 目录** —— 那里的代码不会被 `go build`/`go vet`/`go test` 编译检查，也就永远不会因为改动而报错。一个不会被编译的测试夹具是维护陷阱。

### 1.2 测试分层

| 层 | 文件 | 特点 |
|---|---|---|
| 纯逻辑单测 | `main_test.go` | 秒级，测输入解析（`\r` / `\n` / `\r\n`、0x03 丢弃半行、多字节透明） |
| 集成测试 | `integration_test.go` | 真实 ConPTY，约 65 秒 |
| 专项验证 | `interrupt_windows_test.go` | Ctrl+C 三条路径，见 §4 |

---

## 2. 验证方法

**核心原则：不 mock PTY。** mock 会把「我们认为 PTY 会怎么表现」写进断言里，于是它只能验证我们的假设，永远发现不了 ConPTY 的真实行为。本报告里所有出人意料的结论（0x03 语义、退出不 EOF、模式值）**都是 mock 测不出来的**。

**关键手法：对照实验。** 每当观察到异常，先加一个「什么都不做」的对照组，确认异常是由目标操作引起的，而不是测试夹具本身的问题。§4.3 的结论就是靠这个坐实的。

---

## 3. 通过项明细

| # | 验证项 | 方法 | 结果 |
|---|---|---|---|
| P2-1 | 创建 + 读 + 写 + 退出码 | `TestProbeLifecycle` | ✅ SIZE 正确；ECHO 回显正确；exit code 0 |
| P2-2 | resize 生效 | `TestProbeResize` | ✅ 子进程自报 `RESIZE cols=60 rows=20` |
| P2-3 | 连续 resize 不错乱 | `TestProbeResizeRepeatedly` | ✅ 4 次连续变更全部被观察到 |
| P2-4 | UTF-8 / emoji 字节透明 | `TestProbeUTF8Transparent` | ✅ `中文😀` 逐字节原样穿过 |
| P2-5 | 宽字符样本完整 | `TestProbeWidthSamples` | ✅ CJK / emoji / box-drawing 全量通过 |
| P2-6 | 控制台模式可观测 | `TestProbeConsoleMode` | ✅ 首次测得默认值 `0x01f7`（§5.1） |
| P2-7 | 退出后不误判 | `TestProbeReadDoesNotEOFOnExit` | ✅ 见 §5.2 |

**关于 resize 的一条语义说明**：Windows 侧的「窗口变化」不是一个事件，而是需要被发现的**状态变化**（探针用 150ms 轮询，因为 ConPTY 不会通知子进程 `ResizePseudoConsole` 被调用过）。直接后果是**快速连续 resize 时中间尺寸可能被整个跳过**。这不是 bug，但 Phase 6 前端做 resize 防抖时必须知道 —— 否则会把「防抖窗口小于轮询周期导致的丢帧」误判成后端 resize 链路断了。

---

## 4. ★ 未通过项：Ctrl+C 在 shell 场景下不可用

### 4.1 现象

用真实 `cmd.exe` 起一个长时间运行的命令（`ping -n 30`），然后写 `0x03`：

```
对照组（写 0x03 之前）: echo FIRST_MARKER  → ✅ 正常执行
写 0x03 之后:
  · ping 输出增量 = 3        → ❌ 没有被中断，继续输出
  · echo AFTER_CTRL_C        → ❌ 再也没出现，shell 不接受输入了
  · 进程存活，PTY 输出链路正常
```

**即：写 0x03 既不能中断命令，还会把 stdin 弄坏。** 对照实验排除了「输入通道本身有问题」的可能。

### 4.2 根因：0x03 的语义取决于**子进程的控制台模式**

探针实测到 ConPTY 交给子进程的默认输入模式：

```
MODE stdin=0x01f7
  PROCESSED_INPUT | LINE_INPUT | ECHO_INPUT | MOUSE_INPUT
  | INSERT_MODE | QUICK_EDIT_MODE | EXTENDED_FLAGS
```

**`ENABLE_PROCESSED_INPUT` (0x0001) 是开着的。** 这个标志的含义是「Ctrl+C 由系统处理，不作为字节返回给应用」。于是 0x03 被 conhost 拦截，走不到子进程的读取路径。

同一段代码在子进程**自己设了 raw mode** 之后，行为完全不同：

| 子进程模式 | 谁在用 | 写 0x03 的结果 |
|---|---|---|
| **cooked**（默认，`PROCESSED_INPUT` 开） | `cmd.exe`、PowerShell、任何普通控制台程序 | ❌ 字节被 conhost 拦截 → **stdin EOF**，命令不中断 |
| **raw**（应用自己关掉 `PROCESSED_INPUT`） | vim、htop、**Claude Code、Codex、OpenCode** | ✅ 0x03 作为字节送达，进程存活，由应用自己解释 |

两条路径都有测试钉住：`TestProbeCtrlCInCookedMode` / `TestProbeCtrlCInRawMode`。

**这推翻了 R1 的一条结论。** `docs/PHASE0-R1-CONPTY-FINDINGS.md` §7 写的是「0x03 不产生 `CTRL_C_EVENT`，子进程收到的是字符（ORD=3）」，并据此认为「对 raw mode TUI 恰好正确，对 shell 待 Phase 2 确认」。现在确认了：**前半句只对 raw mode 成立**；cooked mode 下子进程收到的不是字符 3，而是 **EOF**。R1 当时测的都是 TUI，所以没暴露这一面。

### 4.3 三条中断路径全部实测

| 路径 | 做法 | 结果 |
|---|---|---|
| ① 写 0x03 | Agent 当前实现 | ❌ cooked mode 下无效 + 破坏 stdin；raw mode 下正确 |
| ② `GenerateConsoleCtrlEvent` | `FreeConsole` → `AttachConsole(子进程)` → `GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)` | ❌ **API 全部返回成功，但信号无效** |
| ③ `conpty.dll` 私有接口 | `ConptyGenerateConsoleCtrlEvent` | ⚠️ **本机没有 `conpty.dll`**（System32 与 Program Files 全盘搜索均无），无法验证 |

**路径 ② 值得单独说明 —— 它不是「API 用错了」。** 我们用 `GetConsoleProcessList` 验证了前置条件：

```
3)  AttachConsole(70588)                  → 成功
3b) GetConsoleProcessList → [测试进程, ping, cmd.exe]
                                          ↑ 三者确实共享同一个伪控制台
4)  GenerateConsoleCtrlEvent(CTRL_C, 0)   → 返回成功
    ping 输出增量 = 3                     → 但没有任何效果
```

文档要求的前置条件（调用者已附加到目标控制台、目标进程共享该控制台）全部满足，调用也返回成功，信号就是送不到。**结论：系统自带的 ConPTY（kernel32 的 `CreatePseudoConsole`）不响应 `GenerateConsoleCtrlEvent`。**

**路径 ③ 是 Windows Terminal 走的路。** 它自带一份 `conpty.dll`，导出 `ConptyGenerateConsoleCtrlEvent`，直接给伪控制台里的进程组发信号，不走「附加控制台」那套机制。这条路在本机无法验证，因为它需要随包分发 `conpty.dll`。

### 4.4 影响评估

| 场景 | 影响 | 严重度 |
|---|---|---|
| **Claude Code / Codex / OpenCode（主用例）** | 无影响 —— 它们设 raw mode，0x03 正确送达 | 无 |
| vim / htop 等全屏 TUI | 无影响 | 无 |
| **cmd.exe / PowerShell 里的长时间命令**（`npm install`、`ping`、构建） | Ctrl+C 按钮既不能中断，还会把会话弄坏（stdin EOF） | **高** |
| 验收标准 **A8**「按 Ctrl+C → 中断当前命令，CLI 不退出」 | shell 场景下**不满足** | 高 |

**一句话**：主用例（JOJO 实际要用的「手机操控家里的 Claude Code」）不受影响；但「在 CodeGate 里开个 shell 跑命令」这个基础场景的 Ctrl+C 是坏的。

### 4.5 候选方案（待决策）

| 方案 | 做法 | 优点 | 缺点 |
|---|---|---|---|
| **A. 按模式分流** | 会话创建时声明是 shell 还是 TUI；或运行时探测（`AttachConsole` + `GetConsoleMode`） | 不需要新交付物 | 探测有进程级副作用（`FreeConsole` 影响整个 Agent），必须全局串行化；声明方式对用户是负担 |
| **B. 分发 `conpty.dll`** | 随 Agent 分发，用 `ConptyGenerateConsoleCtrlEvent` | 唯一被证明可靠的通路（Windows Terminal 在用） | 体积、许可、版本兼容都要单独评估 |
| **C. 接受限制 + UI 兜底** | 对 shell 会话禁用/改写 Ctrl+C 按钮，提供「终止会话」替代；文档写明 | 成本最低，行为诚实 | 功能上确实是缺失 |
| **D. 换 PTY 实现** | 回到 winpty（注入 conhost 的老方案） | 老方案对 Ctrl+C 处理成熟 | 放弃 ConPTY 的现代特性，且 winpty 自身也在维护停滞 |

**我的建议：短期 A + C，中长期评估 B。** 理由：主用例不受影响，所以不值得为 shell 场景立刻引入一个新二进制依赖；但要**先把「Ctrl+C 在 shell 会话下不可靠」这件事写进 UI 和文档**，不能让它以「偶发 bug」的形式存在。

---

## 5. 其他实测发现

### 5.1 ConPTY 默认输入模式（首次测得）

```
0x01f7 = PROCESSED_INPUT | LINE_INPUT | ECHO_INPUT
       | MOUSE_INPUT | INSERT_MODE | QUICK_EDIT_MODE | EXTENDED_FLAGS
```

两点值得注意：

- **`ENABLE_VIRTUAL_TERMINAL_INPUT` (0x0200) 不在其中。** 也就是说默认状态下输入走的是「控制台输入事件」模型，不是 VT 序列。全屏 TUI 会自己打开这个标志。**对 CodeGate 的含义**：Agent 往 PTY 写的字节，会由 conhost 按当前模式翻译成事件或字符 —— 这个翻译层是「透明中继」这个承诺里最容易被忽视的一环，Phase 6 联调时要重点观察方向键之类的特殊按键。
- **`ENABLE_PROCESSED_INPUT` 默认开着**，这是 §4 的全部根因。

### 5.2 ★ 子进程退出后，`Read` 不会 EOF

实测：子进程退出、`Wait()` 正常返回之后，**`Read` 仍然阻塞** —— conhost 还持有管道写端，直到我们调用 `Close()` 才断开。（这与 Unix PTY 不同：Unix 上子进程退出会关闭从端，master 读端随即 EOF。）

**这条直接推翻架构文档 §6.2 第 3 点的写法**：

> 原文：「顺序必须是 `Wait 返回 → 继续 drain 到 EOF → 发 session.exit`」

**在 Windows 上「drain 到 EOF」是个永远等不到的事件，照抄会挂死。** 正确顺序是：

```
Wait() 拿到退出码 → 给一个短的静默窗口收尾（吸收最后几行输出）
                 → 主动 Close() → 发 session.exit
```

测试 `TestProbeReadDoesNotEOFOnExit` 把这个「不 EOF」本身作为断言 —— 如果哪天它开始 EOF 了，说明 ConPTY 行为变了，上述结论要重新验证。

### 5.3 一个环境事实

`conpty.dll` 在本机不存在，也没装 Windows Terminal。这意味着**任何依赖 Windows Terminal 私有接口的方案，都无法在开发机上验证** —— 如果最终选方案 B，需要先解决「在哪测」的问题。

---

## 6. 对设计文档的修订建议

| 文档位置 | 现状 | 建议 |
|---|---|---|
| `PHASE0-R1-CONPTY-FINDINGS.md` §7 | 「0x03 不产生 CTRL_C_EVENT，子进程收到字符」 | 改为「**取决于控制台模式**」：raw mode 下作为字节送达；cooked mode 下被 conhost 拦截并导致 stdin EOF」 |
| `PHASE0-ARCHITECTURE.md` §6.2 第 3 点 | 「Wait 返回 → 继续 drain 到 EOF」 | 改为「Wait → 短静默窗口 → 主动 Close」；并注明这是 Windows/Unix 的**真实差异**，不能共用一套退出逻辑 |
| `PHASE0-ARCHITECTURE.md` §6.1 `Signal` 注释 | 「Windows 只支持 SignalInterrupt，实现为写 0x03」 | 补充「**仅对 raw mode 程序有效**；cooked mode 下会破坏会话」，并链接本报告 §4 |
| `PHASE0-ARCHITECTURE.md` §6.4 | ttyprobe 放 `internal/terminal/testdata/` | 改为 `cmd/ttyprobe/`（理由见 §1.1） |

---

## 7. 待决策

1. **§4.5 的方案选择** —— 短期走 A+C（分流 + UI 兜底），还是直接上 B（分发 conpty.dll）？
2. **A8 验收标准要不要改写** —— 当前表述「按 Ctrl+C → 中断当前命令，CLI 不退出」在 shell 场景下做不到。建议改为分场景表述，或明确标注「TUI 场景」。
3. **是否继续 P3** —— P2 的结论是「主体可用、一处缺陷有明确边界」。如果同意「主用例不受影响」，可以按原计划进 P3（Agent）；如果要求先解决 Ctrl+C，则 P3 顺延。
