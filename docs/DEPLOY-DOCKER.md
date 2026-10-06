# Docker Compose 部署

Compose 只运行 **Server**。要控制的电脑需运行对应系统的原生 Agent；Windows、Linux、macOS 均支持终端会话。Linux/macOS Agent 的接入见 [Unix Agent 指南](DEPLOY-AGENT-UNIX.md)。Server 镜像构建时会编译 Web 前端并嵌入二进制。

## 一条命令安装预构建 Server（Linux / macOS）

先安装并启动 Docker Engine（macOS 用 Docker Desktop）和 Compose 插件，安装 [GitHub CLI](https://cli.github.com/) 并执行 `gh auth login`。由于仓库与镜像是私有的，当前 GitHub 账号需要拥有仓库与 GHCR 镜像的读取权限；如果镜像拉取提示拒绝访问，检查令牌的 `read:packages` 权限。镜像在合并到 `main` 后由 GitHub Actions 发布，首次发布成功后才能使用下面的命令。

本机部署，在 Linux 或 macOS 终端执行：

```bash
bash -o pipefail -c "gh api -H 'Accept: application/vnd.github.raw+json' repos/JOJO-5/codegate/contents/deploy/install-compose.sh | sh"
```

公网部署，先设置域名 DNS 指向这台服务器、开放 TCP 80/443，再执行：

```bash
CODEGATE_DOMAIN=codegate.example.com bash -o pipefail -c "gh api -H 'Accept: application/vnd.github.raw+json' repos/JOJO-5/codegate/contents/deploy/install-compose.sh | sh"
```

脚本下载 Compose 配置与 Caddyfile，创建并保留 JWT 密钥，登录 GHCR，拉取匹配宿主架构的 Server 镜像并启动。默认安装目录为 `~/.local/share/codegate`，可用 `CODEGATE_INSTALL_DIR` 修改。重复运行会拉取最新镜像并更新容器，但保留 `.env`、密钥和 SQLite 卷。修改域名时手动编辑安装目录的 `.env` 并重新运行；运行 `docker compose --profile public ps` 可检查状态。

macOS 的本机地址只能在该 Mac 上访问；如需让其他设备连接，请使用公网域名与 HTTPS 或自行配置安全的反向代理。此方法下载的是运行在 Docker 中的 Linux Server 镜像，macOS 由 Docker Desktop 运行它。需要从源码构建时继续使用下方的 Compose 步骤。

## 先在部署机本地验证

安装 Docker Engine 与 Compose 插件，在仓库根目录操作：

```bash
mkdir -p secrets
openssl rand -hex 32 > secrets/jwt_secret
chmod 600 secrets/jwt_secret
docker compose up -d --build server
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
```

浏览器在这台机器打开 `http://127.0.0.1:8080`，注册第一个账号。即使 `CODEGATE_ALLOW_SIGNUP=false`，空数据库允许创建首个账号；此后公开注册保持关闭。

`secrets/jwt_secret` 必须保存好；不要提交到 Git，也不要在重建容器时重新生成。数据库在 Docker 命名卷 `server_data` 中，`docker compose down` 不会删除它；`docker compose down -v` **会删除数据卷**。升级时通常使用 `docker compose up -d --build`。

本地端口只绑定到宿主机 `127.0.0.1`，不会直接暴露到公网。

## 公网域名和 HTTPS

先将域名 A/AAAA 记录指向部署机，并允许入站 TCP 80、443。复制环境示例并修改：

```bash
cp .env.example .env
```

```dotenv
CODEGATE_DOMAIN=codegate.example.com
CODEGATE_BASE_URL=https://codegate.example.com
```

然后启动：

```bash
docker compose --profile public up -d --build
docker compose ps
curl -fsS https://codegate.example.com/healthz
```

Caddy 自动申请和续期证书，转发 HTTP 与 WebSocket 到内部 Server。80、443 是 Caddy 的端口；Server 的 8080 仍只在部署机本地开放。Caddy 的证书和配置缓存在 `caddy_data` / `caddy_config` 卷里。

`CODEGATE_BASE_URL` 与浏览器实际访问的 HTTPS 地址必须一致；Compose 同时用它设置 Server 的 Origin 白名单和安全 Cookie。如果域名改变，修改 `.env` 后重新执行 `docker compose --profile public up -d`。

## Windows Agent 连接

在要控制的 Windows 电脑上，安装 OpenCode（若需使用），并构建原生 Agent：

```powershell
go build -o bin/codegate-agent.exe ./cmd/codegate-agent
```

在 `%APPDATA%\CodeGate\agent.json` 创建配置。把工作目录改为该电脑上真实存在、愿意开放的目录：

```json
{
  "server_url": "wss://codegate.example.com/api/v1/ws/agent",
  "device_name": "JOJO-PC",
  "allowed_roots": ["C:\\Work"],
  "allowed_commands": [
    {
      "id": "opencode",
      "label": "OpenCode",
      "command": "cmd.exe",
      "args": ["/c", "opencode"],
      "kind": "tui"
    }
  ],
  "allow_custom_commands": false
}
```

这里不设置 `insecure`。在 Windows 电脑执行：

```powershell
.\bin\codegate-agent.exe doctor
.\bin\codegate-agent.exe pair
```

打开 `https://codegate.example.com`，在设备页输入配对码并确认。保持 `pair` 窗口运行到配对结束，再运行：

```powershell
.\bin\codegate-agent.exe run
```

Agent 只需能向域名的 443 端口发起出站连接。OpenCode 运行在 Windows 电脑上，使用那台电脑的安装、模型配置和工作目录。

### 开机自启（未解锁也能连接）

先完成 `pair` 并确认网页显示设备，再在目标 Windows 电脑的仓库根目录运行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\agent-autostart.ps1 install
```

脚本会先运行 `doctor`，把 Agent 程序复制到当前用户的 `%LOCALAPPDATA%\CodeGate\bin`，创建系统启动时触发的计划任务，并立即启动。安装时输入**当前 Windows 账号的密码**（不是 PIN）；任务以同一账号的普通权限运行，使用已配对的配置和设备私钥，登录桌面前也可以运行。不要同时留着手动启动的 `agent run`，避免同一身份双重连接。不要把任务改成 SYSTEM 账号，远程 CLI 会获得过高权限且使用不同的用户配置。

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\agent-autostart.ps1 status
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\agent-autostart.ps1 uninstall
```

卸载只移除计划任务，保留配置和设备私钥。重新构建 Agent 后再执行一次 `install` 以更新任务使用的程序。安装后建议实际重启并在**未登录桌面**时查看网页设备状态、创建一条 TUI 会话；本项目 CI 只验证脚本语法，真实的登录状态和 CLI 环境需要在目标 Windows 电脑上确认。任务使用的用户账号必须能在无人登录时访问允许的工作目录、OpenCode 程序及其模型配置；如密码变更，重新运行 `install` 更新计划任务的凭据。


## 运维

```bash
docker compose --profile public logs -f server caddy
docker compose --profile public ps
docker compose --profile public up -d --build
```

备份时同时保管 `server_data` 中的 SQLite 数据与 `secrets/jwt_secret`；若需要迁移 Windows 设备身份，也备份 Agent 的状态目录。不要使用 `down -v` 做普通重启。

如果已有自己的 HTTPS 反向代理，可以只启动 `server` 服务，将它代理到部署机的 `127.0.0.1:8080`，并照样设置 `CODEGATE_BASE_URL=https://你的域名`。

## Agent 更新包

Server 镜像构建时按仓库根目录 `VERSION` 同时交叉编译 Linux、macOS、Windows 的 amd64/arm64 Agent，并将它们放进镜像内只读的 `/opt/codegate/agent-updates`。更新镜像后由 Server 自动提供这六个平台的版本包，无需在宿主机手动复制二进制。发布新版本前递增 `VERSION` 的 `v主.次.修订` 号。Agent 通过已配对设备的签名请求下载。使用 `supervise` 开机服务的 Agent 在没有会话时下载校验，冻结新会话创建后启动新版本；若 90 秒内无法通过 Server 认证则继续旧版本，启动后短时间内连续异常退出也会回滚。首次安装应在网页“设备”页选择系统并生成 10 分钟有效的一次性安装命令。

## Agent 首次安装与空闲更新

在已登录的“设备”页选 Windows、Linux 或 macOS，点击生成安装命令，在目标机器执行。安装命令只使用 10 分钟、仅能下载一次；它从当前 Server 下载对应 CPU 架构的二进制并校验 SHA-256，同时保存开机服务脚本。请先为目标机器创建 `agent.json`、运行 `doctor` 和 `pair`，网页确认配对后关闭前台 `run`，再安装开机服务。

Linux / macOS 的服务脚本保存于 `~/.local/share/codegate/agent-autostart.sh`，执行：

```sh
CODEGATE_AGENT_BINARY="$HOME/.local/bin/codegate-agent" sh "$HOME/.local/share/codegate/agent-autostart.sh" install
```

Windows 的服务脚本保存于 `%LOCALAPPDATA%\\CodeGate\\agent-autostart.ps1`，在 PowerShell 中执行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$env:LOCALAPPDATA\\CodeGate\\agent-autostart.ps1" install -AgentPath "$env:LOCALAPPDATA\\CodeGate\\bin\\codegate-agent.exe"
```

将 `"update_enabled": true` 写入 Agent 配置后，服务运行的 `supervise` 命令会在无会话时安全切换。已有采用 `run` 启动的服务需要先手动关闭所有会话，再重新执行新版服务脚本安装一次以切换到 `supervise`；仅前台手动运行的 Agent 会暂存包而不会自行替换。服务器故障时，更新可能在健康检查阶段退回旧版并于下次检查重新尝试。

## 终端与 DSH

设备上的 Agent 可以启动授权的普通 Shell 和交互式 CLI，浏览器中使用的是该机器的真实 PTY。Agent 会扫描 Claude Code、Codex、OpenCode、DSH；扫描本身不会授权。在设备页的“本机工具”中点击“授权使用”，Agent 仅接受这四个固定工具 ID，并将选择保存在本机状态目录的 `approved-tools.json`，无需手改 JSON 或重启。取消授权后不能再新建该工具的会话，已运行会话不受影响。自定义程序仍需在目标机器的 `agent.json` 中添加 `allowed_commands`。旧版 Agent 不支持网页授权，需要先更新 Agent。

DeepSeek Harness 的 `dsh` 命令本身是 profile 启动器；CodeGate 检测到它不代表 TUI 插件已就绪。在目标机器以运行 Agent 的同一用户安装官方文档给出的示例插件，并确认能启动交互界面：

```sh
dsh plugin --profile tui add github:deepseek-harness/turtle-ui
dsh --profile tui
```

插件管理需要 `pnpm`；安装 Git 源码插件时，可能还需按 DSH 提示在该 profile 的 `pnpm-workspace.yaml` 允许构建后重试。验证成功后可在设备页授权 DSH。需要特殊参数或独立 Web 地址时，仍可手动在 `allowed_commands` 添加：

```json
{
  "id": "dsh",
  "label": "DeepSeek Harness",
  "command": "dsh",
  "args": ["--profile", "tui"],
  "kind": "tui",
  "web_url": "https://dsh.example.com"
}
```

`web_url` 可选，仍表示你**另外部署并完成 HTTPS 与登录保护**的外部 DSH Web 地址。它与下方内建转发互不影响。

### 内建 DSH Web 转发（可选）

Agent 在电脑上启动 `dsh web --no-open`，通过现有出站连接把本机 `127.0.0.1:3080` 的 HTTP 和 WebSocket 转发到 Server。电脑不需要配置域名、证书或开放端口。DSH 的登录令牌和原生 Cookie 只留在 Agent/Server 内存中。

**简化方式：同域名、独立 HTTPS 端口。** Compose 的 `public` 配置自动将 DSH 入口设为 `https://你的现有域名:8443`，复用 Caddy 已有证书，不需要另配子域名或通配证书。更新 `compose.yaml` 和 `deploy/Caddyfile`，保持 `CODEGATE_BASE_URL=https://你的域名`，开放服务器/云防火墙的 TCP 8443，再启动 `docker compose --profile public up -d`。`CODEGATE_DSH_PROXY_URL=auto` 是 Compose 默认值；本地 HTTP 部署不会启用公网代理。

使用自己的 Nginx/Caddy 也可以：添加同域名的 HTTPS 8443 监听，使用现有证书，代理到 CodeGate Server 的 8080，保留完整 Host（含端口），支持 WebSocket 升级。Server 设置 `CODEGATE_DSH_PROXY_URL=https://你的域名:8443` 并重启。DSH Web 必须使用与 CodeGate 主站不同的浏览器来源；原生应用从根路径加载 `/api`、资源和 WebSocket，不能直接挂到 `/dsh` 子路径。

电脑上安装 DSH 并确保 Agent 服务账户能找到 `dsh`；在设备页点击“允许 Agent 启动 DSH Web”，然后“启动并打开 Web”。授权保存在本机状态目录，无需编辑 `agent.json` 或重启（Agent v0.1.14 起支持）。已有 `"dsh_web_enabled": true` 仍兼容。如果端口 3080 被其他 DSH 占用，先关闭那个实例。原生 Web profile 的安装与模型登录按 DSH 本身的提示完成。

简化入口一次服务一台设备，切换电脑前在设备页停止 DSH Web，以免多个页面混用设备。需要多设备同时使用时，可保留独立通配域名模式：设置 `CODEGATE_DSH_PROXY_DOMAIN=dsh.example.com`，为 `*.dsh.example.com` 配置 DNS 和通配证书，将证书/私钥放到 `secrets/dsh-tls/fullchain.pem`、`privkey.pem`，使用 `docker compose -f compose.yaml -f deploy/compose.dsh.yaml --profile public up -d`。`auto` 会让已有通配域名配置优先；显式 URL 与域名配置不能同时启用。

内建浏览器授权有效一小时，过期或 Agent 重连后从设备页重新打开。DSH Web 运行时会阻止 Agent 自动更新；点击“停止”后恢复空闲更新检查。外部 `web_url` 仍可独立使用。

### 结束终端后恢复原对话

刷新或离开页面只会重新连接现有进程；“关闭会话 / 确定结束进程”会结束进程。设备页已结束的 CLI 记录提供“恢复原对话”，以原生对话 ID 启动同一目录的 CLI，保留它的历史和上下文。OpenCode、DSH 自动记录原生 ID；Codex 仅在原生存储证实回调 ID 时自动关联。旧记录、尚未关联的 Codex 或 Windows CLI 首次恢复需要选择同工作目录的原生历史，之后保存选择，按精确 ID 恢复，不使用“最近一次”猜测。原生历史被删除或工作目录不再授权时无法恢复。

### 传输压缩

支持的 Agent/浏览器与 Server 协商 WebSocket `permessage-deflate`。512 字节以上的终端输出/回放和文件读写使用轻量压缩，小按键、登录控制消息和已压缩的 DSH 隧道不会重复压缩。发送端压缩、接收端解压，Server 作为两段连接的中继也会处理；并非所有工作都在手机端。旧客户端可继续无压缩连接。解压后的消息仍有长度上限。

内嵌网页资源缓存 gzip 结果；Caddy 可协商 zstd/gzip。压缩率取决于数据，重复终端文本收益较大，图片本身通常已压缩。算法使用低压缩级别降低 CPU 和交互延迟。
