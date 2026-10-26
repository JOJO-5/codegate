# Ctrl+C：编程 TUI 防误退，普通终端中断当前命令

v0.1.45 的实现与验收记录。按用户要求，Codex、Claude Code、OpenCode、DSH 会话禁用工具条 Ctrl+C，并在按钮内显示“防误退”；键盘输入的 Ctrl+C 同样拦截并说明原因，其他输入保持可用。普通 Shell 启用 Ctrl+C，将中断当前命令与“关闭会话”明确区分；实际部署状态以发布记录为准。

根据实际命令、已知 npm shim 参数及原生绑定识别编程 CLI，不根据用户自定义会话名称决定。普通终端即使叫“Codex”也可中断；Codex 即使叫“Shell”也保持保护。未接回、断线、只读及已结束状态不发送中断；迟到的失败回执不能覆盖重连后新连接的错误状态，草稿不受影响。

## 为什么修订旧的 Windows Shell 禁用规则

旧报告记录过 cooked-input 探针及 cmd 无法继续输入的现象。本轮在当前系统上未复现真实交互式 Shell 失效，不能继续将旧报告作为统一禁用依据；历史差异尚未定位到具体 Windows 版本或其他因素。这次使用真正的交互式 cmd 和 Windows PowerShell 5.1，分别验证：

1. cmd 启动持续 ping → `Terminal.Signal(SignalInterrupt)` → 返回提示符 → 再执行新命令。
2. PowerShell 启动持续 ping → 中断 → 返回提示符 → 再执行新命令。
3. PowerShell 运行 `Start-Sleep -Seconds 60` → 中断 → 返回提示符 → 再执行新命令。

三项均在当前 Windows 11 开发机（10.0.26200.9711）通过，原有发送 0x03 的后端路径无需修改。探针只管理它创建的进程树，不停止用户会话。未将这些结果扩大为所有 Windows 版本或所有控制台程序都已验证；本机没有 pwsh 7，本轮未验证它的实际行为。

发布前 Windows CI 出现命令已回显但普通 VT 回车未提交的情况；卸载 PSReadLine 或等待回显后单独提交仍复现，不能将其归因于该模块或时序。自动探针保留默认行编辑，使用 ConPTY 请求的 [Win32 输入按键格式](https://github.com/microsoft/terminal/blob/main/src/terminal/input/terminalInput.cpp) 明确发送 VK_RETURN 按下/抬起，仅用于驱动中断与 Shell 生命周期测试；不修改产品输入或用户配置。普通 VT 回车的跨 Windows 环境兼容性未据此验收。

微软说明 processed-input 模式决定系统如何处理 Ctrl+C，不能仅凭类别推断最终程序行为：[Low-Level Console Modes](https://learn.microsoft.com/en-us/windows/console/low-level-console-modes)。历史调查保留在原文并标注修订，当前前端策略独立于旧的 `CommandKind.Interruptible` 辅助值。

## 设计与验收

通过“防误退”和“中断当前命令，不关闭终端”的文字区分两种目标，按钮和键盘执行同一保护策略。先观察 Codex 仍可发送 Ctrl+C 的回归测试失败，再实现策略。

- Web 会话测试覆盖四个编程 CLI、cmd shim、原生绑定、带空格的 Shell 路径、会话改名、断线/只读/未接回/结束状态，以及普通字符输入和草稿保留。
- 隔离浏览器沿真实 SessionView 与 HTTP/WebSocket 路径测试，57 项通过：新增 TUI 禁用原因、物理键保护、普通终端的 `signal=int` 请求与 0x03 输入，以及原有手机布局、Shift+←、草稿和重连检查。
- 未调用真实模型、未操作线上任务；真实 CLI 当前任务取消语义、其他 Windows 版本、手机实机软键盘和读屏全流程未验收。

重跑本机终端验证：`go test ./internal/terminal -run TestConPTYShellInterruptKeepsSession -count=1 -v`。浏览器 fixture 的运行方式见 [手机布局验收记录](MOBILE-TERMINAL-2026-10-26.md)。
