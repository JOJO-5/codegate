# Agent 自动更新与故障恢复（v0.1.10）

升级前先更新 Server，并在目标电脑重新安装 v0.1.10 Agent 启动器、重启 Agent 服务。自动下载只替换守护进程启动的子 Agent；正在运行的旧 `supervise` 不会自动变成新版，旧启动器需要这一次手动更新。新版仍写旧的就绪标记，允许旧启动器先启动新版子 Agent。

## 切换与回滚

- 存活/保留会话、DSH Web、Git 操作及 worktree 创建都会阻止更新。新操作和最终切换共用原子互斥边界；繁忙时延后，不中断正在提交或推送的 Git。
- 下载验证 HTTPS、同源、平台、版本、大小和 SHA-256；私有票据记录大小/摘要，执行与重启前再次验证。符号链接候选和越界状态路径被拒绝。
- 新版本须在 90 秒内通过真实 Server 鉴权。失败则继续旧版。
- 已接受版本持续报告本机进度；连续 3 分钟无进度会恢复旧版。Server 离线时重连/退避也报告本机进度，不把网络故障当作进程卡死。
- 连续异常退出累计 3 次会回滚；只有单次运行稳定满 10 分钟才重置预算，35 秒后再崩溃也会计数。
- 失败版本首轮退避 15 分钟，随后 30 分钟、1 小时，最高 24 小时。新版本号不受上一版本退避影响。状态保存在 `updates/last-failure.json`；排查根因后可在停止服务期间删除此文件再重启，重新尝试。
- 退出时先请求子 Agent 正常停止，3 秒后仍不退出才强制终止。健康回滚可能结束故障 Agent 的会话，不能保证恢复故障进程内的历史缓存。

私有 `state_dir` 从远程工作区 API 排除，即使它位于较大的 allowed_roots 中，也不能通过网页文件、Git 和扫描 API 访问密钥、更新票据或二进制。授权 CLI 本身不是系统沙箱，请用普通用户运行 Agent，并保护本机配置/状态目录权限；Windows 应使用服务用户独享的 ACL。

## 独立发布签名（可选）

默认兼容现有部署，信任配置的 HTTPS Server。配置 `update_public_key` 后，Agent 会强制要求发布方的 Ed25519 签名；缺失、错误签名和篡改的版本/平台/大小/摘要均拒绝。Server 无需签名私钥，只读取产物同目录的 `signature.txt`。

在可信发布机器构建离线签名工具并生成密钥：

```sh
go build -o codegate-update-sign ./cmd/codegate-update-sign
./codegate-update-sign -generate-key /secure/codegate-release.key
```

输出为 base64 公钥，私钥文件以 0600 创建且不覆盖已有文件。将公钥加入目标电脑 agent.json：

```json
{"update_public_key": "生成命令输出的 base64 Ed25519 公钥"}
```

在发布机器对已经构建的更新目录签名：

```sh
./codegate-update-sign -key /secure/codegate-release.key -dir /release/agent-updates
```

目录结构为 `linux|darwin|windows` / `amd64|arm64`，每个平台含 codegate-agent（Windows 为 .exe）和 version.txt。部署已签名的目录，设置 Server 的 `CODEGATE_AGENT_UPDATES_DIR`；Docker 可用只读挂载覆盖 `/opt/codegate/agent-updates`。私钥始终留在独立发布机器，不放入仓库、Server 或运行镜像。当前默认镜像不包含签名；配置公钥前必须先部署签名产物。公钥轮换需先更新目标电脑配置。

本次验证包含真实守护子进程的切换/故障恢复、并发 Git 提交与更新屏障、签名拒绝/退避、原子状态替换，以及真实 Agent/HTTPS Server 的签名下载和故障版本恢复。
