# CodeGate

CodeGate 让你从手机或电脑浏览器访问另一台机器上的原生命令行。目标机器运行 Agent，Agent 主动连接 Server；浏览器连接 Server，终端输入与输出由 Server 中继。目标机器不需要开放入站端口。

项目目前处于开发阶段。它传输 PTY 字节流，不解析 CLI 的业务内容，也不替代 Shell、IDE 或 AI Agent。

## 当前能力与边界

- 浏览器里创建、查看和关闭独立的终端会话；返回设备列表、打开文件页或短暂断网时，Agent 上的会话继续运行，再进入原会话会重放仍在环形缓冲区中的输出。Agent 重启会终止其持有的 PTY，无法恢复已终止的进程。
- 同一账号可在多个客户端查看会话，只有主控客户端能输入和调整尺寸。
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

## 开发与验证

```powershell
go test ./...
cd web
npm run typecheck
npm run build
```

Windows 上的 ConPTY 集成测试位于 `internal/terminal`。完整端到端终端链路可以在配置好 GNU Make 与 Bash 的开发环境运行 `make e2e`；它使用临时 Server 和数据库，不触碰已有数据。

详细设计与实现记录见 [架构文档](docs/PHASE0-ARCHITECTURE.md)、[ConPTY 验证](docs/PHASE2-CONPTY-VERIFICATION.md)、[Server](docs/PHASE4-SERVER.md) 和 [Web](docs/PHASE5-WEB.md)。
