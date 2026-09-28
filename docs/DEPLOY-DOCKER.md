# Docker Compose 部署

Compose 只运行 **Server**。要控制的电脑仍需运行 Windows 原生 Agent；Linux/macOS Agent 的 PTY 尚未实现，不能靠把 Agent 放进容器来替代。Server 镜像构建时会编译 Web 前端并嵌入二进制。

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

## 运维

```bash
docker compose --profile public logs -f server caddy
docker compose --profile public ps
docker compose --profile public up -d --build
```

备份时同时保管 `server_data` 中的 SQLite 数据与 `secrets/jwt_secret`；若需要迁移 Windows 设备身份，也备份 Agent 的状态目录。不要使用 `down -v` 做普通重启。

如果已有自己的 HTTPS 反向代理，可以只启动 `server` 服务，将它代理到部署机的 `127.0.0.1:8080`，并照样设置 `CODEGATE_BASE_URL=https://你的域名`。
