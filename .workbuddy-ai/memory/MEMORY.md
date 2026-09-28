# CodeGate —— 项目长期记忆

## 这是什么

自托管工具：从手机控制家里 PC 上的 Claude Code 会话。
`Agent`（家里机器，只出站）→ `Server`（有公网那台，中继 + ring buffer）→ `Web`（手机浏览器）。

技术栈：Go 1.27.1（`internal/` 13 个包）+ Vue 3 + TypeScript + Vite + xterm.js。
规范文档：`docs/PHASE0-ARCHITECTURE.md`（唯一真相源，改实现要同步改它）。

## 构建与验证入口（都用 make）

**本机有 GNU Make 4.4.1**（`~/.workbuddy-ai/binaries/w64devkit/.../bin/make.exe`，
已加进 `~/.bashrc`）。不要再说「本机没有 make」。

```bash
make build      # server + agent
make test       # 全量测试（依赖 goenv.sh 的 GOFLAGS=-vet=off）
make race       # 竞态检测（依赖 goenv.sh 的 CGO_ENABLED=1 + gcc）
make e2e        # ★ 终端数据通路端到端（真 ConPTY → Server → 客户端）
make server-smoke     # HTTP 层冒烟
make web / web-check  # 前端构建 / 类型检查
```

⚠️ 自动化工具若以**非交互**方式起 bash（走 `BASH_ENV`），读不到 `.bashrc`，
需要显式设 PATH。Makefile 里有 `GOENV` 兜底：每个用到 go 的 recipe 前
source `scripts/goenv.sh`。

## 四个验证脚本，各管一层（缺一层就有盲区）

| 脚本 | 覆盖 |
|---|---|
| `go test ./...` | 单元 / 集成（13 包） |
| `scripts/server-smoke.sh` | HTTP：状态码 / 认证 / 缓存头 / 优雅退出 |
| `scripts/web-render-check.sh` | 前端能否挂载（真 Chrome 无头） |
| `scripts/e2e-terminal.sh` | **终端数据通路**（真 Agent → 真 ConPTY → Server → 客户端） |

**前三个全绿也可能「终端永远空白」** —— 这正是 e2e 存在的理由。
改任何与会话生命周期 / 中继 / 订阅相关的代码，必须跑 `make e2e`。

## 代码约定

- 注释写**为什么**，不写**是什么**。踩过的坑要写进注释（含实测数据）。
- 错误信息要能指向修法，不能只描述现象。
- 安全边界（`allowed_roots` / `allowed_commands`）必须显式声明，**不给默认值**。
- 会话关闭路径的顺序铁律：**先广播、再 `ForgetSession`**。
  `ForgetSession` 会删掉订阅者名单，顺序反了广播就是静默失败。
- `session.closed` 有双重身份（响应 / 自发推送），判据是 pending 表里有没有
  那条 `request_id`。

## 本机环境（Windows + 亿赛通 DLP）

- **密文判据只有一条可靠**：`fcli read` 的 `size_bytes` ≠ `fcli stat` 的 `size`。
- **提交前必扫**：`python tools/fcli-plaintext.py --check internal cmd docs scripts tools
  Makefile go.mod web/src web/index.html web/package.json web/package-lock.json
  web/tsconfig.json web/vite.config.ts web/env.d.ts`
- `go vet` 崩 `0xc0000005` → `GOFLAGS=-vet=off`（**本机专属，不要进 CI**）。
- 详细的 Windows make / 命令行参数陷阱见 skill `windows-make-and-argv-traps`。

## 尚未解决

- Go module cache 的 rename 失败：`.mod` 从不落盘（118 个 `.mod*.tmp` 堆积），
  每次构建重新拉 `.mod`。`GOTMPDIR` 无效，可试 `GOMODCACHE` 指到 `%TEMP%`。
- `SessionView.vue`（Vue 层）的帧循环 / attach 重放 / resize 防抖无自动化测试。
- 项目**不在 git 仓库里**（`git status` → not a repository）。
