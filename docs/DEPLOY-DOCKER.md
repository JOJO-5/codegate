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
