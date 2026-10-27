# CLI 原生启动模式与 Chat 接入调查

日期：2026-10-27。依据用户本轮澄清：权限预选是把 CLI 自己的 TUI 模式通过原生启动参数提前选好，不是 CodeGate 新建一套权限策略。本批改动纳入本地 v0.1.46 标签，按用户要求暂不推送或部署升级。

## 原生模式预选

项目“开始工作”和设备“新建对话”使用同一选择器，默认“继承 CLI 本机设置”。选择只作用于本次新建终端，切换工具或重新打开面板会清空；启动失败保留名称、目录和选择。既有会话的恢复及其原生权限切换保持原样，后台调度没有复用此字段。

| 工具 | 当前提供的原生预选 | 实际传入方式 |
|---|---|---|
| Codex 0.164+ | 只读、工作区读写、自动审批、完全权限 | `-c default_permissions=:read-only/:workspace/:danger-full-access`，配合 `-a on-request/never`；自动审批使用 `--approve-for-me` |
| Claude Code | 手动、Plan、自动接受编辑、Auto、不询问、完全权限 | `--permission-mode manual/plan/acceptEdits/auto/dontAsk/bypassPermissions` |
| OpenCode 2 | 自动批准 | `--auto`；明确拒绝的原生规则仍保留。没有用自定义规则伪装只读、读写或完全权限 |
| DSH TUI | 继承本机设置，说明限制 | 当前未核实到对应模式的原生启动参数，不注入审批插件或新规则；独立 DSH Web 仍由自身界面控制 |

浏览器只发 `launch_mode` 枚举 ID，不发送任意参数。Server 校验设备归属、Agent 能力及其命令清单；Agent 将 ID 映射为固定原生参数，再由原有白名单及目录校验启动。带本机固定附加参数的命令不开放覆盖入口。旧 Agent 不会静默忽略非空选择；不支持的模式保留输入并要求重新选择。

“完全权限”由用户明确选择，仅本次启动传入，不记为通用默认；界面显示原生行为及可展开核对的启动参数。CodeGate 没有新增文件规则、MCP 规则、沙箱代理、审批插件、原生对话预创建或权限回写；原生 CLI 的权限语义、管理策略和限制仍由 CLI 决定。Codex 通过选择时的版本检查避免旧版忽略新的配置项。

## Chat 的接入结论

首版使用本机 Codex App Server 的结构化协议，沿用本机凭据与模型；不解析 ANSI 输出来拼聊天消息，也不在 Server 保存 CLI 密钥。初期只创建明确归 CodeGate 管理的原生对话，终端作为独立入口保留。

本机 Codex 0.164.0 配合隔离 `CODEX_HOME`、临时项目和本地模拟 Responses 服务，已完成真实协议实验：

- 通过 `thread/start`、`turn/start` 获取原生 thread/turn ID；消息增量携带原生 item ID。
- 收到流式文本、回合结束事件；重新连接后用原 ID 分页读取 turn/item 历史，没有重发模型请求。
- 停止运行中的回合，等待 `turn/completed` 返回 `interrupted` 后确认停止。
- 收到绑定 thread/turn/item 的原生命令审批，明确拒绝后收到 `serverRequest/resolved` 和原生 declined 状态。
- Plan 模式中的原生提问可以结构化回答，并收到请求解除与回合结束事件。

这些实验使用真实 CLI 和模拟模型响应，没有调用真实模型或接入用户当前会话；不能替代真实模型、浏览器全链路或线上验收。

## 开发前固定的边界

现有 `live_quota.go` 查询连接会跳过异步通知和服务端请求，只用于额度/续跑核对，不能直接作为 Chat 客户端。Chat 需要长期读取循环、串行写入、响应与事件分流、未决审批表和连接 epoch。

接入顺序：

1. Agent 原生适配层：持续连接、thread/turn/item 身份、历史分页、停止及审批路由；不重启共享 daemon。
2. Server：设备与对话授权、单个写入者、派发回执及结果不明状态。发送前保存 attempt；断线不能盲目重放。
3. 浏览器：消息与工具状态、审批/提问卡片、停止、草稿、刷新后的对账。只在收到原生回执后显示已执行或已停止。
4. 真实模型隔离验收通过，再开放 Chat 入口；后续逐一接入 OpenCode、Claude 和 DSH。

浏览器刷新与 Agent 到原生服务断线是两种故障。浏览器刷新优先接回 Agent 仍持有的连接；原生连接丢失时重新核对运行状态、历史和审批，不假设旧审批会自动重放。未决审批跨原生断线恢复、派发回包丢失、同时从外部 TUI 写入、附件及真实模型费用策略尚未闭环验证；相应生产能力不提前开放。

## 设计心理学与验收

用“CLI 原生权限模式”表达用户目标，按当前 CLI 显示能力，不把所有工具硬套进同一套权限。默认继承、完全权限明确选择、失败保留输入、参数可核对；不将终端创建、模型运行完成和用户验收混为一谈。

本地验收：Go `./internal/...` 全量通过，前端构建及 UX/会话/上传回归通过；固定参数映射、Server/Agent 门控与旧 Agent 拒绝静默忽略通过。Codex 四种模式在真实隔离 TUI 启动后核对原生状态通过，模型调用为零。Claude 用隔离配置与本地模拟 Anthropic 响应核对手动、Plan、自动接受编辑、不询问、完全权限五种原生 init 回执通过；Auto 的模型/账号可用性尚未验收，不能把帮助中的可选参数当成所有账号可用。

隔离浏览器 48 项检查通过：320/390/1440px、原生参数可查看、键盘选模式、失败保留名称及选择、只传模式 ID、关闭重开及切换工具清空权限、继承时不传覆盖字段、设备旧入口复用选择器。检查使用模拟 HTTP/WebSocket，没有接入生产 Agent；真实手机触摸和软键盘、实际 OpenCode `--auto` 模型执行、DSH 对应 TUI 启动参数、真实模型 Chat 和线上验收尚未完成。

## 一手资料

- [OpenAI Docs：Codex App Server](https://learn.chatgpt.com/docs/app-server)、[原生权限配置](https://learn.chatgpt.com/docs/permissions)，与本机 0.164.0 的 experimental schema、CLI 帮助和隔离实验交叉核对。
- [Claude Code 原生权限](https://code.claude.com/docs/en/permissions)，并以本机 `claude --help` 与结构化 init 回执核对。
- [OpenCode V2 权限说明](https://opencode.ai/v2/docs/permissions/)，本机 2.0.32 的 `opencode --help` 仅核实到 `--auto` 启动选择。
- DSH 本机 0.2.1-rc.2：启动器帮助、已安装模块文档与 Web profile 帮助。直接调用本机启动器时未找到 `tui` profile，未据此推断生产 Agent 的 `DSH_HOME` 或 DSH Web 状态，也未安装或修改用户 profile 来制造“支持”结果。
