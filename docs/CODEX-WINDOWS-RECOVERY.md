# Windows Codex 执行环境与续跑恢复

2026-10-18 本机诊断确认了两个独立状态，不能只凭网页顶部的“已连接”判断执行能力正常。

## 沙箱在命令启动前失败

Codex CLI 0.162.1 的沙箱日志记录：对正在使用的 `cua_node/.../node_repl.exe` 执行运行时权限校验时，发生 `open ACL target for root-only update` 与 Windows `os error 32`，最终返回 `helper_unknown_error: setup refresh had errors`。PowerShell 和 CMD 都没有启动，修改项目依赖或反复发送继续指令不能修复它。

本机 CLI 已升级为 0.163.0，保留原有配置和正在运行的对话，没有重置目录权限或删除运行时。使用新版 CLI 实际运行以下受限工作区探针成功：

```powershell
codex sandbox -P :workspace -C D:/mengfanliuFile/dev/codegate -- powershell.exe -NoProfile -Command "Write-Output CODEX_SANDBOX_OK"
```

升级后原来已经运行的 CLI 进程仍是旧版本。需要新开或恢复原生对话才会使用新进程；不能通过结束所有 Codex/Agent 进程的方式中断无关任务。npm 在更新时可能保留仍被旧进程使用的旧包目录，这不表示新安装失败，使用 `codex --version` 核对新入口。

## 控制路径存在但守护进程不可连接

本机控制路径存在，`codex app-server daemon version` 却返回 Windows `10061`（连接被拒绝）。因此路径存在不能作为服务健康证明。

本次启动本机默认守护进程，先以 CodeGate 的真实 WebSocket 握手与只读 RPC 检查没有已加载或活跃回合，再同步守护进程至 0.163.0。升级后再次通过 CodeGate 的原生控制适配器验证连接。没有恢复、提交或中断任何模型回合。

恢复守护进程不会把原来独立进程内的会话自动加载进去。额度墙监测仍要求原生对话身份、目录、当前回合、控制服务加载状态一致；不满足时需要用户从原终端恢复相应原生对话并重新开启监测。

## CodeGate 的反馈和恢复

- 首次连接或尚未提交自动回合时的短暂重连失败，会保存本机回执，显示等待重试，30 秒后核对；连续四次失败停止自动派发并显示“需要确认”。整个过程不提交继续指令，不自动启动或更换 CLI/模型。已经提交自动回合后的控制连接异常仍要求核对，不推测其执行结果。
- 原生连接恢复后先验证原会话、目录和回合，再开启监测。提交回执不明仍进入“需要确认”，不会利用重连去重放提示词。手动输入优先，已关闭的安排保持关闭。
- 没有已派发自动回合且不存在模糊提交时，关闭监测无需控制服务在线；原终端保留。已有自动回合需要确认停止，不能把请求发出当作取消成功。
- 普通后台执行收到结构化的失败命令结果，并报告上述沙箱初始化错误时，进入“需要确认”。仅模型回复里的文字不能更改执行状态；实际成功的后续命令可以证明执行能力已恢复。CLI 退出码为零不等于用户目标已验收。

已暂停或已结束的历史安排不会在修复环境后擅自重跑。新逻辑需要部署对应 Agent 后生效。

## 检查入口

`TestLiveQuotaControlReconnectArmsWithoutSubmittingAndCanCancelOffline`、`TestLiveQuotaControlReconnectIsBounded` 和 `TestContinuationSeparatesReportedSandboxFailureFromNormalExit` 覆盖恢复和反馈边界。

设置 `CODEGATE_CODEX_CONTROL_SMOKE=1` 后运行 `TestCodexNativeControlSmoke`，可以用本机真实服务验证握手、加载状态读取；测试不调用模型、不打印对话正文、身份或凭据。
