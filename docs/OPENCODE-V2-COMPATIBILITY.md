# OpenCode V2 兼容性核对

核对日期：2026-10-17。本机已从 `opencode-ai@1.19.0` 升级为官方 `@opencode/cli@2.0.32`，命令输出 `opencode v2.0.32`。已实现默认本机后台服务的 CodeGate 适配；修改尚未发布到在线 Agent。

升级前已备份全局配置、凭据文件及 SQLite 历史数据库，备份数据库 `quick_check=ok`。本机备份位于 `%LOCALAPPDATA%/CodeGate/release-tools/opencode-v1-backup-20261017T125546`。npm 安装脚本被禁用，未更改全局脚本策略；安装时将 npm 已下载且验证能运行的同版本官方 Windows 二进制复制到包的入口位置。

## 已确认的架构变化

官方 V2 默认入口通过 `ServerConnection.resolve` 发现或启动受管后台服务，TUI 作为客户端连接。默认 TUI 启动时可以因版本不匹配替换后台服务；显式指定 `--server` 则警告后继续连接。`--standalone` 是另一种启动方式，不能与 `--server` 同用。

- [默认入口](https://github.com/anomalyco/opencode/blob/dev/packages/cli/src/commands/handlers/default.ts)
- [后台服务连接与版本策略](https://github.com/anomalyco/opencode/blob/dev/packages/cli/src/services/server-connection.ts)

非交互 `run` 也通过该连接层访问服务。`--session` 和 JSON 输出仍存在，但这只证明参数入口存在，不能证明 CodeGate 原有绑定、取消和结果判定兼容。官方 V2 规范把 HTTP 协议、领域事件与运行时分别交给 Protocol、Schema、Core；客户端应跟随公共 HTTP 接口。

- [run 入口](https://github.com/anomalyco/opencode/blob/dev/packages/cli/src/commands/handlers/run.ts)
- [非交互执行](https://github.com/anomalyco/opencode/blob/dev/packages/cli/src/run/run.ts)
- [V2 规范](https://github.com/anomalyco/opencode/blob/dev/specs/v2/README.md)

## CodeGate 实现边界

| 能力 | V1 路径 | V2 路径 |
| --- | --- | --- |
| 打开/恢复终端 | 白名单 CLI，附加 `--session` | 通过本机 CLI 发现服务、核验版本，再显式指定 `--server` 和已核验的 `--session`，避免默认 TUI 自动替换共享服务 |
| 原生历史 | 只读 V1 SQLite 表 | 读取原生 HTTP 历史，跟随分页，再核对实际目录、父会话、归档状态和明确 ID；不读写 V2 数据库内部表 |
| 新对话绑定和事件 | 注入旧版插件，写本地 marker | 创建空会话并保存服务返回的原生 ID；使用原生 Session、active、message、inbox 接口，不注入 V1 插件，不猜测最近会话 |
| 后台执行取消 | 取消 CLI 进程上下文 | 原生 HTTP 提交固定输入 ID，查询结果；取消该 ID 的待执行输入或中断对应执行并复查。无法确认时标记 `needs_review`，不重发，不中断其他输入 |
| 关闭终端 | 结束对应进程 | 清理该原生会话的待执行输入并核实执行停止后再关闭客户端；不停止共享服务 |
| 断线恢复 | WebSocket 重连后重新 attach 原终端 | 同样复用原终端、原生 Session 和提交回执；不因重新连接重发提示词 |
| Agent 更新 | 检查会话及任务活动 | TUI 异常退出及任务结果不明时，继续保留活动屏障，直到原生执行和待执行输入确认空闲；共享服务单纯常驻不阻塞更新 |

适配器通过本机获准的 CLI 获取服务地址和私有认证信息，认证信息不上传 Server、不保存到会话记录。只允许回环地址，不跟随 HTTP 重定向。原生提示词以 JSON 请求体发送，不经过 Shell 参数。现阶段支持默认本机共享服务；自定义 `--server`、`--standalone` 配置会明确拒绝原生集成，避免控制错误的服务。V2 后台续跑沿用原生会话保存的模型设置。

## 验证结果

- 自动化原生接口夹具：历史分页和跨目录过滤、结构化额度错误、任务完成、取消执行、取消抢先于提交回执、丢失回执进入 `needs_review`、原生任务阻止更新、相邻输入不串取消。
- 本机 `2.0.32` 原生实测：创建明确 ID、历史查询、恢复同一会话、仅入队不执行的输入取消、空闲中断，以及实际 CodeGate ConPTY 终端中的 TUI 启动和关闭。关闭客户端后共享服务仍可访问。测试只清理自己创建的会话和缓存 location。
- 使用本机 `opencode/mimo-v2.6-flash-free` 完成真实推理和运行中取消，取消另一会话后已完成会话的输出保持，两个原生会话记录的费用均为零。执行前核实模型当前所有价格档均为零，固定 provider/model，不进行付费回退。实测发现自造的 UUID 形式 Session ID 能被本机 API 接受，却可能导致免费模型 `provider.auth`；已改用服务生成的原生 Session ID，并增加回归。
- 未向付费模型提交提示词；真实供应商额度耗尽、运行中服务崩溃恢复和手机界面的完整人工验收尚未覆盖。第三方 V1 插件的迁移由各插件维护者负责，CodeGate 的 V2 适配不依赖它们。

运行本机实测：设置 `CODEGATE_OPENCODE_V2_SMOKE` 为已安装原生 `opencode.exe` 的绝对路径，然后执行 `go test ./internal/agent -run '^TestOpenCodeV2NativeSmoke$' -v -count=1`。普通回归不会启动本机 OpenCode 或触发模型请求。

运行免费模型实测：另设 `CODEGATE_OPENCODE_V2_FREE_MODEL=opencode/mimo-v2.6-flash-free`，执行 `go test ./internal/agent -run '^TestOpenCodeV2FreeModelExecution$' -v -count=1`。仅操作独立临时目录和测试会话；清理时只删除自己的原生会话、卸载对应缓存 location。

此适配不替代主模型调度 CLI 的设计与实现。

## Windows 命令入口升级

V1 npm 包 `opencode-ai/bin/opencode.exe` 与 V2 `@opencode/cli/bin/opencode.exe` 路径不同。配置规整只对已消失的、精确匹配 V1 npm 包布局的 OpenCode 路径检查同一个 `node_modules` 下的 V2 文件；已有文件和自定义路径不会被替换。命令清单中的“已安装”与实际启动使用同一份规整结果。

2026-10-18 本机已修正持久配置指向 V2，并给仍使用旧内存配置的活动 Agent 恢复旧路径的硬链接；两处是同一份官方 2.0.32 二进制，没有重启活动 Agent。旧路径仅用于本次运行兼容，后续启动使用新配置。此次修改没有修复或替换服务商凭据；模型认证与命令可用性是两个状态。
