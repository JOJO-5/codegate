# Linux / macOS Agent 接入

在被控机器上运行 Agent，Server 可以部署在另一台机器。Linux 使用 Unix PTY，macOS 使用系统伪终端；两边都能运行 shell 和全屏 TUI。需要 Go 1.26+ 从私有仓库构建 Agent。安装 OpenCode 等 CLI 时，请先在目标机器上完成模型和凭据配置。

## 构建与配置

在仓库根目录运行：

```sh
go build -o bin/codegate-agent ./cmd/codegate-agent
```

创建 Agent 配置文件。Linux 路径为 `~/.config/codegate/agent.json`；macOS 路径为 `~/Library/Application Support/codegate/agent.json`。将下面的工作目录和 CLI 路径换成目标机器上的**绝对路径**（运行 `command -v opencode` 可查看程序路径）：

```json
{
  "server_url": "wss://codegate.example.com/api/v1/ws/agent",
  "device_name": "My Unix PC",
  "allowed_roots": ["/home/yourname/Projects"],
  "allowed_commands": [
    {
      "id": "opencode",
      "label": "OpenCode",
      "command": "/usr/local/bin/opencode",
      "args": [],
      "kind": "tui"
    },
    {
      "id": "shell",
      "label": "Shell",
      "command": "/bin/sh",
      "args": [],
      "kind": "shell"
    }
  ],
  "allow_custom_commands": false
}
```

macOS 的工作目录一般是 `/Users/yourname/Projects`，OpenCode 可能位于 `/opt/homebrew/bin/opencode`。不要直接照抄示例路径。Agent 只允许浏览器从 `allowed_roots` 选择工作目录、从 `allowed_commands` 选择命令。远程 shell 仍能以当前用户权限读取该用户可访问的文件，请用专门的低权限用户和明确的工作目录配置生产机器。

## 配对与启动

```sh
./bin/codegate-agent doctor
./bin/codegate-agent pair
```

在网页“设备”页选择 Linux 或 macOS，输入配对码并确认；保持 `pair` 运行到绑定完成。先在前台试运行：

```sh
./bin/codegate-agent run
```

从网页打开一条 shell 或 TUI 会话，确认输入输出、窗口尺寸和 Ctrl+C。Agent 只向 Server 发起出站 WSS 连接，无需在被控机器上开放入站端口。

## 开机自启

完成配对并关闭手动运行的 Agent 后，在仓库根目录执行对应脚本。

Linux（需要 systemd 用户服务和 `loginctl`，启用 linger 时可能需要 sudo）：

```sh
sh scripts/agent-autostart-linux.sh install
sh scripts/agent-autostart-linux.sh status
sh scripts/agent-autostart-linux.sh uninstall
```

脚本把二进制复制到 `~/.local/bin/codegate-agent`，创建用户级 systemd 服务并启用 linger，使用户管理器在未登录时也能启动。卸载不会替你关闭 linger，以免影响其他用户服务。

macOS（需要 sudo 安装系统 LaunchDaemon）：

```sh
sh scripts/agent-autostart-macos.sh install
sh scripts/agent-autostart-macos.sh status
sh scripts/agent-autostart-macos.sh uninstall
```

LaunchDaemon 以当前用户身份运行 Agent，二进制位于 `/usr/local/libexec/codegate/codegate-agent`，设备私钥仍留在该用户目录。macOS 的 FileVault 冷启动需要先解锁磁盘，系统才能启动任何守护进程；模型凭据如果只在登录钥匙串里，登录前的 CLI 也可能无法读取。请在目标机器上完成一次重启、未登录状态的设备上线与 TUI 操作验证。

更新 Agent 时重新构建并执行 `install`。卸载脚本仅移除自启服务，保留配置和设备密钥；迁移机器或重装系统时请单独备份状态目录。Linux/macOS 的 CI 已验证 PTY 读写、尺寸、Ctrl+C、关闭与 Go 包测试，但无法替代目标机器的开机和模型凭据验证。

## 空闲时检查更新包（可选）

Agent 构建时须注入版本号，例如 `go build -ldflags '-X main.version=v1.2.3' -o bin/codegate-agent ./cmd/codegate-agent`。在 `agent.json` 中加入 `"update_manifest_url": "https://updates.example.com/linux-amd64.json"`，可再加入 `"update_interval": "1h"`。Windows 和 macOS 使用各自 OS/架构的清单 URL；`dev` 构建不会检查更新。更新源由部署者控制，不要使用不可信的地址。

清单示例：`{"version":"v1.2.4","os":"linux","arch":"amd64","url":"https://updates.example.com/codegate-agent-v1.2.4-linux-amd64","sha256":"<64 位十六进制 SHA-256>","size":12345678}`。清单与二进制须为同一 HTTPS 主机，不能重定向；版本必须为更高的 `v主.次.修订`，大小不超过 100 MiB。Agent 只在会话管理器没有任何会话时开始下载；下载中若新建会话就取消，完整校验大小及 SHA-256 后暂存于状态目录的 `updates/`。当前版本**只暂存，不自动替换运行中的 Agent**；安装与重启仍需在确认无会话后按上面的安装流程操作。私有 GitHub Release 需要认证，不能直接作为无凭据的下载源，也不要把仓库令牌写入 Agent 配置。
