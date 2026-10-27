# 本机编程 CLI 统一调度：调研与实施规划

2026-10-26 补充：最新只读版本核对、原生 Chat 与会话启动权限排期见[工作空间计划](UX-WORKSPACE-CHAT-PLAN-2026-10-26.md)。本文件保留原调度契约和当时版本证据；不得由新 Chat 规划推断其他 CLI 已支持主脑或工作角色。

日期：2026-10-18。状态：规划，尚未实现。基线为 CodeGate v0.1.36；本文不表示 Claude Code、DSH 或通用 ACP 已接入，也不授权发布、升级 CLI 或调用付费模型。

## 1. 目标与结论

用户选择一个主 Agent、项目和目标，由主 Agent 在本机已授权的工作配置中选择工具、拆分任务、检查产物和返工。沿用 Go Server、SQLite、Agent 出站 WebSocket 和 Vue，不新增云端模型代理。

本机模型和凭据配置好是接入前提，还需要机器可读的执行接口，以及可靠的权限、状态、取消和身份绑定。将“安装了 CLI”“普通终端可用”“可被派发工作”“可担任主脑”分别表达。模型兼容 OpenAI API，不代表对应编程 CLI 的生命周期协议兼容。

实施顺序：统一适配契约并迁移现有 Codex/OpenCode → Claude Code → DSH ACP → 其他通过契约验收的 ACP/非交互 CLI。DSH 优先 ACP，headless 作为受限备选；SDK 暂不作为首选。先完成单机串行，跨机和并行仍按[总体设计](MULTI-MACHINE-ORCHESTRATION.md)另行推进。

## 2. 调研证据与版本边界

查阅一手官方文档，并只读检查本机包版本、`dsh --help`、`claude --help/--version`。本轮没有发起模型请求、协议握手、安装插件或修改工具配置。上游 master 文档会变化，实施时需记录所验证的包版本、协议版本及源码提交，不能用最新文档推断旧版支持。

| 工具 | 本机观察 | 官方接口依据 | 接入判断 |
|---|---|---|---|
| Codex | 0.163.0 | `exec --json`、`--output-schema`、指定 Session ID 的 `exec resume` [S1] | 保留现有执行路径，先迁移适配契约；主脑与工作角色分别验收 |
| OpenCode | `@opencode/cli` 2.0.32 | V2 HTTP API：模型、会话、输入、等待、取消与权限接口；API 标注 experimental [S2] | 保留 V2 原生接口，避免套用 V1 SDK/路由 |
| Claude Code | 2.1.240，帮助列出 print、JSON/流式输出、schema、resume、工具及权限选项 | `-p`、`--output-format`、`--json-schema`、`--resume` [S3][S4] | 下一个新增适配器，先工作 Agent，后主脑 |
| DSH | `@deepseek-ai/dsh` 0.2.0-rc.2；本地包包含 acp、sdk、headless 组件 | ACP 支持语义更新、权限请求、取消、恢复与单会话关闭 [S5] | 优先评估 ACP，包存在不等于实际握手/模型已通过 |
| 其他 CLI | 未逐个验证 | 必须提供可探测的原生自动化入口 | 暂不承诺“全部支持”，仅列入候选适配器 |

### Codex 与 OpenCode

Codex 官方提供 JSONL 事件和 schema 输出，也支持按确切会话 ID 继续执行；权限仍由原生沙箱控制 [S1]。沿用当前独立 exec 进程和本机登录，不改为启动或接管用户共享守护进程。`--no-daemon` 等参数以本机帮助和回归为准，不作为所有版本共有接口。

OpenCode V2 的模型、Session、输入和中断由原生服务提供 [S2]。现有实现见 `internal/agent/workflow_runner.go`、`workflow_profiles.go` 及 OpenCode V2 相关文件；迁移时保持已绑定 Session/Input 身份和取消范围。[兼容说明](OPENCODE-V2-COMPATIBILITY.md)仍有效。

### Claude Code：可以接入，权限策略不能照抄最新版

官方 print 模式支持机器可读结果与流式事件，也可请求 schema 输出 [S3]。新建工作任务用新会话；主脑继续决策时只恢复已绑定的 Session ID，不使用“最近一次”或名称模糊匹配。

2.1.240 的本机帮助已确认核心参数，但官方当前文档中的 `--restricted` 要求至少 2.1.248，`--permission-prompts none` 要求至少 2.1.259 [S4]。首期在当前版本上探测实际支持的工具限制和权限行为；不能直接下发新参数，也不能为了无人值守启用 `bypassPermissions` / `--dangerously-skip-permissions`。

只读主脑需要证明不能编辑、运行 Shell、调用能修改外部状态的 MCP 或通过子 Agent 间接写入；仅设置 plan、仅靠提示词或结束后的指纹检查均不足。若原生工具/配置无法可靠收紧，先不开放该角色，工作角色也必须满足既定授权。新增权限请求若无法安全接到界面，转“需要处理”，不默认批准、不悄悄重跑。

`--max-budget-usd` 的计费覆盖范围和实际登录方式需核实，不把它作为订阅额度或全工作流费用的硬保证；不设置跨模型自动 fallback。

### DSH：ACP 优先的原因

| 入口 | 已查明的特点 | 决策 |
|---|---|---|
| `dsh --profile acp` | 提供会话级操作、权限请求、语义事件、取消和关闭；恢复不回放历史 [S5] | 首选。实现 Go stdio JSON-RPC 客户端，先探测能力再调用 |
| `dsh --profile sdk` | SDK 提供事件与等待，但官方明确缺少单会话 close / prompt-cancel；prompt 接纳不是最终结果 [S6] | 暂缓；不能因共用一个 SDK 进程而在取消时结束其他任务 |
| `dsh --profile headless --json` | 一次一任务，返回原生身份与最终事件，支持确切身份续接；最终事件仍可能表示失败 [S7] | ACP 不可用时可单独验证，不自动切换 transport 或重发已有任务 |
| Web / TUI | 面向交互使用，已有转发入口 | 保留给用户；不模拟点击、按键或解析 ANSI 画面进行后台调度 |

本机 DSH ACP 包 README 也列出 `session/new/prompt/resume/cancel/close` 等操作，与首选方案相符；本轮没有启动它的 ACP profile。握手成功只能证明控制面，不代表模型可用、权限可靠或取消已验收。

DSH 的 profile 组合、插件、权限和认证必须纳入配置指纹。上游 headless 文档与本机包存在内容差异：当前上游另有凭据来源说明，本机 README 未见相同段落；不能假定 Web 登录必然能用于 headless [S7]。实施时在目标版本核实，禁止自动修改用户 Web/TUI profile、复制登录文件或扩大权限以绕过认证问题。

DSH ACP 的 `authenticate` 成功不代表模型凭据有效。协议 `session/new` 的 MCP 声明是受信任控制输入 [S5]：只允许 Agent 本机明确配置的服务器，网页和主模型不能传任意命令、环境变量或地址。首期一个工作流拥有一个独立 ACP 进程，不连接用户正在用的 Web runtime。

ACP 协议会演进，取消及恢复必须按实际能力协商 [S8]。DSH 当前声明 `session/resume`，不应替换成未支持的 `session/load`；发送取消通知也不能直接当作已停止。取消后的迟到活动与子任务须进一步关闭、核对，不能以一次取消回执推断所有未来活动都已消失。

## 3. 统一适配契约

主调度循环仅依赖 Agent 内部接口，不再按工具名分支。第一期可以仍放在 `internal/agent`，不为接口拆分引入新服务。以下名称是设计草案，尚未成为协议承诺。

| 操作 | 输入/输出与约束 |
|---|---|
| Probe | 返回 CLI/协议版本、运行模式和能力、不可用原因；默认仅本地控制面检查，不调用模型 |
| Profiles | 返回本机已授权配置、安全展示名称、确切 provider/model、角色能力、费用状态及配置指纹 |
| StartTurn | 接收受校验的运行契约；在发送输入前保存 Attempt，返回确切原生身份及接纳证据 |
| Observe / Inspect | 事件流或受控轮询；区分正在运行、等待权限、等待模型、结果不明和已结束 |
| Resume | 验证目录、身份、配置与独占权；恢复观察和提交新一轮输入分开，不自动重发旧 prompt |
| Cancel | 幂等；返回取消已请求、停止已确认或结果不明，并说明作用范围和证据 |
| Close | 仅释放本运行拥有的会话/进程；不删除代码和原生历史，不退出共享后台服务 |

能力至少拆分：工作角色、主脑角色、结构化决策、原生续接、活动流、取消确认、工作区限制、权限处理、费用可观测。协议能返回 JSON 仅说明传输可解析，不能证明模型会正确规划或遵守 schema。

主脑准入：能可靠生成并校验 `delegate/inspect/check/finish/need_help`，保持必要上下文，限制对工作区和外部工具的直接副作用；工作准入：能在授权工作区执行，报告真实终态并可靠停止。缺少续接的 CLI 可评估新会话工作任务，不能假装支持原对话恢复；缺少可靠取消/权限约束的暂只提供普通终端。

### 身份、事件与配置

- 保留 Workflow、工作步骤、Attempt、原生会话、原生输入/请求和本机进程身份的映射；跨协议不混用 ID。相同 nonce 的相同契约不重复调用模型。
- 通用事件包含工作流/步骤/Attempt、递增序号、事件时间和观察时间、当前阶段及安全摘要。乱序/重传不覆盖较新状态；旧 Attempt 的事件不能覆盖新 Attempt。
- 事件可包括接纳、模型文本、工具活动、权限等待、用量、终态与错误；只上传界面需要的脱敏摘要，不上传完整原生工具参数、环境、认证头或 stderr。默认不展示 thinking。
- 记录可执行文件身份、工具/适配器版本、模型配置、权限与插件策略的指纹。模型目录、有效默认模型或权限变化时重新核对；旧任务不静默切换配置或 transport。
- 凭据仅本机使用，指纹不包含密钥内容，不把可离线猜测的密钥散列上报。未知费用和认证状态保留 unknown，不用 Probe 发模型消息验证登录。

### 费用与权限交集

可用工作配置 = 本机白名单 ∩ 用户候选池 ∩ 角色能力 ∩ 当前费用政策 ∩ 可用工作区。主模型不能扩大交集。默认免费工作池继续执行当前逐次报价检查；本地部署或使用订阅均不自动等于免费。价格未知的配置不能进入“仅免费”。主脑费用与工作费用分开显示，无法核对实际模型路由时不满足“仅免费”。

CLI 的 hooks、插件、子 Agent 和 MCP 可能产生额外操作或费用，必须纳入有效权限与模型策略检查。首期无法核实其路由的扩展应禁用或使 profile 不满足免费/只读准入，不能只检查顶层模型名称。

工作区限制由真实原生策略或外部执行边界支撑，不能用 worktree 代替沙箱。验证命令仍由 Agent 固定 check 入口执行；CLI 无法约束自身 Shell/工具时不开放自动工作角色。

### 失败、取消和恢复

1. 本机先持久化 Attempt 和配置快照，再创建原生执行并保存回执。发生“远端已接收、本机没存下身份”等窗口时进入需要核对，不重新 StartTurn。
2. 终端退出码、原生终态、模型最后一句话、验证通过、人工验收分别记录。CLI 退出 0 但缺少终态或有失败事件，不能视为成功。
3. 停止顺序为封闭后续派发、请求原生取消、核对终态；需要时关闭本任务拥有的 scope。超时仅允许清理有确切归属的独立进程树；清理结果未确认就保持“停止未确认”。
4. Agent 崩溃或断网后先恢复观察。无法证明前一轮已结束则需要核对；原生 resume 不意味着可以自动继续执行。现有单工作流串行限制继续生效。
5. 模型、工作目录和原生会话必须绑定；续接拒绝目录不符或被终端/其他任务占用的对话。路径比较沿用双侧物理路径规范化。
6. 活跃工作流、权限等待、取消未确认和产物收集均阻止工作区清理及 Agent 自动更新；前端关闭不改变执行所有权。

## 4. 界面与设计心理学

入口保持“项目 → 新建任务 → 主 Agent 自主调度”。主界面只展示主 Agent、目标、可用工作池摘要和费用政策，高级选项逐步展开。示例展示：`Claude Code · 所选模型 · 可执行工作 / 主脑待验证`，不用安装成功的绿色图标代替调度能力。

- 设备状态独立写“设备在线”；工作流显示排队、执行中、等待权限、停止中、停止未确认、需要核对、已交付待验收。
- 不能选择的配置有文字原因和恢复入口，例如“CLI 版本缺少受限执行能力”“本机未登录”“价格未知，不符合仅免费”“需更新 Agent”。保留用户目标草稿。
- 执行前展示设备、项目/分支、主模型与候选池、修改范围、费用和检查项；执行后对应真实步骤、diff、测试和产物，不伪造百分比进度。
- 权限等待应显示申请内容的安全摘要、所属任务、允许范围和拒绝入口；主脑不能替用户批准扩大授权。第一期没有安全审批通道时仅能拒绝并等待处理。
- 停止入口始终可发现；等回执后更新状态，刷新仍保留快照和停止请求；允许重查而非盲目重跑。历史整理与删除原生会话/代码分开。
- 键盘可操作，错误/状态由读屏播报，焦点可恢复；390px 窄屏显示工具名、原因和停止按钮。

## 5. 分期与退出条件

以下是建议里程碑，不预占版本号，不代表固定发布日期。按用户要求先本地积累修复，发布需另行明确执行。

| 阶段 | 交付范围 | 退出条件 |
|---|---|---|
| A：统一契约 | 迁移现有 Codex/OpenCode；新增能力/配置快照/通用事件；保留旧协议读取 | 原有免费模型链、身份隔离、取消、断线与旧 Agent 门控无回归；持久化前后崩溃窗口测试通过 |
| B：Claude 工作 Agent | print/stream-json 适配、原生身份、工具约束、取消和用量；不默认付费调用 | 当前 Windows 版本控制面与回归通过；经用户费用授权后真实编辑/验证/取消；无旁路副作用与相邻会话干扰 |
| C：Claude 主脑 | schema 决策、只读约束、按身份续接、同池派发 | 只读越权测试、格式纠正上限、返工和完整真实交付通过；不继承任意本机工具权限 |
| D：DSH ACP | Go ACP stdio、能力协商、语义事件、单会话 cancel/close、权限边界 | 0.2.0-rc.2 实际握手、身份恢复、暂停/取消与隔离通过；模型调用需另计真实验收；可先工作角色再开放主脑 |
| E：可扩展接入 | 通用 ACP transport 与受信任的本机 profile 注册；其他非交互 CLI 独立适配 | 每个 CLI/版本单独通过同一准入测试；未知 ACP 不自动获得 DSH 的能力或信任 |

阶段 A 的候选改动点：`internal/agent/workflow_runner.go`、`workflow_profiles.go`、`workflows.go`、`workflow_engine.go`；协议 `internal/protocol/workflows.go` 与 Agent capability；Server profile/派发校验、SQLite workflow 状态；Web `WorkflowPanel.vue` 与配置/进度展示。实施前确认存储结构与迁移需要，不在本次规划中预先修改数据库。

阶段 B/C 不必强制引入 Node sidecar：先用 Go 驱动原生 CLI。只有需要官方 SDK 提供的可证明权限/控制能力时，另行评估固定版本 sidecar 的分发、签名校验与升级成本。阶段 D 优先原生 Go stdio，避免为 DSH Web 再增加代理和端口。

旧 Agent 缺失能力应返回明确升级要求；已存工作流沿用其旧适配器版本或进入人工核对。回滚应用不能把未知新状态当作可重新派发；涉及存储迁移按停机数据库备份/恢复方案实施。

## 6. 验收矩阵与待确认项

每个适配器先写协议/行为回归，再实现。假服务覆盖错误与竞态，真实 CLI 覆盖生命周期，真实模型覆盖实际产物，界面验收单独记录。

| 路径 | 必须证明 |
|---|---|
| 正常 | 主脑选择跨 CLI 工作配置、生成实际代码、失败后返工、最新文件通过固定检查、产物与原生身份一致 |
| 输出 | 空结果、畸形/超长 JSON、未知事件、截断输出、stdout 插件污染、最后文本与失败终态矛盾均不误判成功 |
| 权限/费用 | 未授权工具、目录越权、配置变更、失效登录、限流、未知价格、嵌套工具路由均不能扩大范围或付费兜底 |
| 等待 | 没有首 token、权限等待、进程存活但无活动分别表达；超时不显示完成、不无限重试 |
| 取消 | 接纳前/刚接纳/工具执行中/完成竞态/已取消再次取消；只停止本任务，模型请求与子进程归属可核对 |
| 恢复 | Server/Agent/CLI 在发送与回执窗口分别崩溃；刷新、断网、迟到事件、旧 Attempt、目录别名和被占用对话不重复执行 |
| 平台/交互 | Windows 进程树与 stdin EOF、Linux/macOS 信号；键盘、焦点、读屏、390px 布局和禁用原因 |

待实施确认：Claude 当前版本有效权限是否足以支持角色限制；原生插件/hooks 的策略隔离；Claude/DSH 真实取消延迟与子活动收敛；DSH 当前版本模型目录/实际路由/认证来源与 ACP 字段；通用 ACP 版本协商和 Go 依赖选型；各 CLI 对实际费用的报告能力。未解决项不得标记完成，也不通过放宽权限或调用付费模型试错来跳过。

本轮完成的是官方接口调研、当前代码和安装版本核对、方案及验收设计。Claude/DSH 的真实编程调用、运行中取消、权限边界、跨 CLI 主脑完整链路，以及新增界面均尚未实现/验收。

## 7. 一手资料

资料查阅于 2026-10-18；master 和在线 CLI 文档可变。以下链接用于事实依据，路线、阶段与准入条件属于 CodeGate 的设计判断。

- [S1：OpenAI Codex 非交互模式](https://learn.chatgpt.com/docs/non-interactive-mode) — JSONL、schema、原生身份续接与沙箱。
- [S2：OpenCode V2 HTTP API](https://opencode.ai/v2/docs/api) 与 [V2 权限](https://opencode.ai/v2/docs/permissions/) — 不使用 V1 路由推断 V2。
- [S3：Claude Code programmatic/headless](https://code.claude.com/docs/en/headless) — print、机器可读结果和结构化输出。
- [S4：Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference) — 参数语义、权限模式及版本要求。
- [S5：DSH ACP 官方契约](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/acp/acp/README.md) 与 [ACP profile](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/bundle/acp-app/README.md)。
- [S6：DSH SDK server](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/sdk/server/README.md) 与 [SDK profile](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/bundle/sdk-app/README.md) — 特别注意取消与结果归属限制。
- [S7：DSH headless](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/bundle/headless/README.md) — 身份、事件、失败和认证行为需结合目标版本核对。
- [S8：ACP Session setup](https://agentclientprotocol.com/protocol/v1/session-setup)、[Prompt turn](https://agentclientprotocol.com/protocol/v1/prompt-turn)、[Cancellation](https://agentclientprotocol.com/protocol/v1/cancellation) — 可选能力与取消回执。
