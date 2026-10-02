# CodeGate

CodeGate 让你从手机或电脑浏览器访问另一台机器上的原生命令行。目标机器运行 Agent，Agent 主动连接 Server；浏览器连接 Server，终端输入与输出由 Server 中继。目标机器不需要开放入站端口。

项目目前处于开发阶段。它传输 PTY 字节流，不解析 CLI 的业务内容，也不替代 Shell、IDE 或 AI Agent。

## 当前能力与边界

- 浏览器里创建、查看和关闭独立的终端会话；返回设备列表、打开文件页或短暂断网时，Agent 上的会话继续运行，再进入原会话会重放仍在环形缓冲区中的输出。Agent 重启会终止其持有的 PTY，无法恢复已终止的进程。
- 同一账号可在多个客户端查看会话，只有主控客户端能输入和调整尺寸。
- 项目页自动发现在线 Agent 的 `allowed_roots` 下的 Git 工作树，可选择已安装且授权的 CLI 新建对话；设备页也可选择仓库填入工作目录。Agent 启动后扫描，每分钟更新，也支持手动重新扫描。支持多层目录、worktree 和子模块，不遍历 `.git` 元数据或符号链接目录；扫描权限错误或超时会显示未完成状态。此功能需要同时更新 Server 和目标电脑上的 Agent，Docker Server 不会扫描宿主机或其他设备的目录。
- Agent 通过工作目录白名单和命令白名单限制远程启动；默认不允许自定义命令。
- Server 使用 SQLite；终端输出缓存在 Agent 的环形缓冲区，Server 不保存终端内容。
- Agent 在 Windows 使用 ConPTY，在 Linux/macOS 使用 Unix PTY；均可运行终端 CLI。Linux/macOS 的安装和配对见 [Unix Agent 指南](docs/DEPLOY-AGENT-UNIX.md)。
- 从会话终端的“文件”入口浏览当前工作目录：列目录、预览 UTF-8 文本和常见图片、下载和上传文件。每次操作都按会话归属与 Agent 工作区白名单校验，上传/下载单文件上限 100 MB。

## Docker Compose 部署 Server

在 Linux 或 macOS 上可通过已登录的 GitHub CLI 和 Docker 一条命令拉取预构建 Server 镜像；也能从源码用 Compose 构建。具体命令与私有仓库权限要求见 [Docker Compose 部署指南](docs/DEPLOY-DOCKER.md)。Compose 运行 Server 与网页，可选 Caddy HTTPS；被控电脑运行对应系统的原生 Agent。

## 快速开始：同一台 Windows 电脑体验

需要 Go **1.26+**、Node.js 和 npm。下面用 PowerShell；首次构建需要联网下载依赖。Server 和 Agent 可以先跑在同一台 Windows 电脑，确认流程后再分开部署。

### 1. 构建前端与两个程序

在仓库根目录运行：

```powershell
cd web
npm install
npm run build
cd ..

New-Item -ItemType Directory -Force internal/server/webui/dist, bin | Out-Null
Remove-Item internal/server/webui/dist/assets -Recurse -Force -ErrorAction SilentlyContinue
Copy-Item web/dist/* internal/server/webui/dist/ -Recurse -Force

go build -o bin/codegate-server.exe ./cmd/codegate-server
go build -o bin/codegate-agent.exe ./cmd/codegate-agent
```

前端资源在编译 Server 时嵌入二进制。改完前端后要重新构建前端、复制产物并重新编译 Server。仓库的 `Makefile` 也提供 `make web`、`make build` 和 `make e2e`；它需要 GNU Make 与 Bash，且当前 `scripts/goenv.sh` 含开发者机器专用路径，因此新机器优先使用上面的直接构建命令。

### 2. 启动 Server 并创建首个账号

在一个 PowerShell 窗口中运行：

```powershell
$env:CODEGATE_LISTEN = '127.0.0.1:8080'
$env:CODEGATE_BASE_URL = 'http://127.0.0.1:8080'
$env:CODEGATE_ALLOWED_ORIGINS = 'http://127.0.0.1:8080'
.\bin\codegate-server.exe serve
```

打开 <http://127.0.0.1:8080>，注册第一个账号并登录。第一个账号成为管理员，之后默认关闭公开注册。首次启动会自动创建 SQLite 数据库；没有设置 JWT 密钥时会生成临时密钥，重启后已有登录状态会失效。

### 3. 配置 Agent

在 `%APPDATA%\CodeGate\agent.json` 创建配置文件（不要把设备私钥或个人配置提交到仓库）。将示例中的工作目录改成**已经存在**的绝对路径：

```json
{
  "server_url": "ws://127.0.0.1:8080/api/v1/ws/agent",
  "insecure": true,
  "device_name": "My Windows PC",
  "allowed_roots": ["C:\\Work"],
  "allowed_commands": [
    {
      "id": "powershell",
      "label": "PowerShell",
      "command": "powershell.exe",
      "args": ["-NoLogo"],
      "kind": "shell"
    }
  ],
  "allow_custom_commands": false
}
```

`insecure: true` 只用于本机明文 `ws://` 测试。跨机器部署时使用 HTTPS/WSS，并将 `server_url` 改为 Server 的 `wss://<域名>/api/v1/ws/agent` 地址。

### 4. 配对并运行 Agent

在第二个 PowerShell 窗口中运行：

```powershell
.\bin\codegate-agent.exe doctor
.\bin\codegate-agent.exe pair
```

在网页的“设备”页面输入 Agent 输出的配对码，核对设备后确认。配对码有效期为 10 分钟。配对完成后运行：

```powershell
.\bin\codegate-agent.exe run
```

回到网页选择设备、工作目录和 PowerShell 命令，即可创建会话。Agent 的 `pair` 会等待网页确认；请保持它运行到配对结束。若需开机自动运行且未登录桌面也能连接，完成配对后参照 [Windows Agent 开机自启](docs/DEPLOY-DOCKER.md#开机自启未解锁也能连接) 安装计划任务。首次排障可分别运行 `codegate-server doctor` 和 `codegate-agent doctor`。

## 部署提示

- Server 可以与 Agent 分开部署；Agent 只发起出站 WebSocket 连接。浏览器与 Agent 都必须能访问 Server。
- 公网部署请配置 HTTPS/WSS、固定的 `CODEGATE_JWT_SECRET_FILE`、`CODEGATE_BASE_URL` 与 `CODEGATE_ALLOWED_ORIGINS`。不要把本机示例中的 `insecure: true` 用于公网。
- Agent 的 `allowed_roots` 和 `allowed_commands` 必须显式配置。浏览器只应获得你愿意开放的目录和程序。
- Server 的数据库默认放在平台数据目录；Agent 的设备私钥放在状态目录。迁移设备身份时应妥善保管私钥。
- Docker 镜像由 main 分支的发布工作流生成；Server 镜像内嵌六个平台的带版本 Agent 包；登录后的设备页可以生成从自托管 Server 下载的十分钟一次性安装命令。Windows 流程和 [Linux/macOS 流程](docs/DEPLOY-AGENT-UNIX.md) 也支持源码构建。

已授权的普通 Shell 和编程 CLI 都可作为终端会话运行。新版 Agent 会报告本机已安装的常见 CLI，但扫描结果不自动授权；DeepSeek Harness 的 TUI 与独立 Web UI 接入见 [部署文档](docs/DEPLOY-DOCKER.md#终端与-dsh)。

## 开发与验证

```powershell
go test ./...
cd web
npm run typecheck
npm run build
```

Windows 上的 ConPTY 集成测试位于 `internal/terminal`。完整端到端终端链路可以在配置好 GNU Make 与 Bash 的开发环境运行 `make e2e`；它使用临时 Server 和数据库，不触碰已有数据。

详细设计与实现记录见 [架构文档](docs/PHASE0-ARCHITECTURE.md)、[ConPTY 验证](docs/PHASE2-CONPTY-VERIFICATION.md)、[Server](docs/PHASE4-SERVER.md) 和 [Web](docs/PHASE5-WEB.md)。

## 远程编程 Git 面板

在会话终端点击 **Git**，可查看当前工作目录所属仓库的分支、改动文件，以及已暂存/未暂存差异。新文件提供有限长度文本预览，二进制文件不展示内容。大清单/差异会标记截断，完整内容可在终端查看。目标 Agent 和 Server 均需升级至 v0.1.7。仓库工作目录与共享 Git 元数据必须位于该 Agent 授权的工作目录内。

分期范围与验收见 [远程编程计划](docs/REMOTE-CODING-ROADMAP.md)。

### 独立 Git 工作区（v0.1.8）

设备页新建会话时勾选 **创建独立 Git 工作区**，Agent 会从当前 HEAD 创建唯一的 `codegate/task-*` 分支与 `.codegate-worktrees/<任务 ID>` 目录。原目录未提交的修改不会复制。关闭会话后工作区保留，通过扫描清单或手动选择该路径可继续工作。新建 CLI 失败会尝试移除干净工作区；已有修改不会强制删除。目录仅通过 Git 本地 exclude 忽略，不修改项目的 .gitignore。清理可在终端使用 `git worktree remove <路径>`，本版本不提供自动合并或自动清理。Server 与 Agent 均需升级。

### 任务模板与 Git 操作（v0.1.9）

编程 CLI 的终端底部可选择检查 bug、修复构建、运行测试和审查改动模板。模板只填入输入框，不自动发送；已有输入时不会覆盖。普通 Shell 不显示编程任务模板。

Git 面板可填写说明并确认 **提交全部改动**。请先停止 CLI 修改并刷新，操作会暂存并提交该仓库全部改动。失败时暂存区可能保留，需刷新后检查。要求已有 HEAD、本机 Git 身份和已解决冲突。

**推送当前分支** 仅推送到当前分支已配置的上游，不强制推送，也不推送其他分支或自动跟随 tag。独立任务的新分支首次发布须在终端执行 `git push -u <remote> <branch>` 配置上游。使用 Agent 服务用户的 Git 凭据；认证失败或超时会显示错误，可在终端核对远端状态。Web 暂不提供 GitHub PR 创建和合并。Server 与 Agent 均需升级至 v0.1.9。
