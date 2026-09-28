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
- **`GOMODCACHE` 必须设在 `~/.workbuddy-ai/` 之外**（已设 `C:/Users/JOJO/go/pkg/mod`）。
  在 `.workbuddy-ai` 树里，go 的 `.mod` rename 会被拦，`.mod` 永不落盘 →
  每次构建重新拉模块。`goenv.sh` 里已修，含实测对照表。
- 详细的 Windows make / 命令行参数陷阱见 skill `windows-make-and-argv-traps`。

## 尚未解决

- `SessionView.vue`（Vue 层）的帧循环 / attach 重放 / resize 防抖无自动化测试。

## 版本控制

**已纳入 git 并推到 GitHub**：`https://github.com/JOJO-5/codegate`（**private**）。
分支 `main`（**不带 `/`**，本机 git 有「报成功但引用不落盘」的老毛病，
见 skill `git-ops-locked-windows`；权威校验只用 `git ls-remote`）。

- 首次提交 `604832e`（154 文件），第二次 `07cb060`（VERSION 求值顺序修复）。
- **推 GitHub 必须走代理**：`~/.gitconfig` 里已给 `https://github.com/` 配
  `proxy = http://192.168.9.163:10808`（直连超时，代理 0.36 秒 200）。
- **凭据走 gh 而不是 GCM**：`~/.gitconfig` 的 `[credential "https://github.com"]`
  段用 `gh auth git-credential`。GCM 对 github.com 会拉 GUI 授权、静默卡住。
  ⚠️ `gh` 连不上网时会报 **`The token in keyring is invalid`** —— 这是**假警报**，
  真因是没走代理。设上 `HTTPS_PROXY` 后同一个 token 完全可用。

`.gitattributes` 钉死 **仓库内一律 LF**，这不是洁癖：
本机 `core.autocrlf=true`，靠它的话**下次 checkout** 会把 `scripts/*.sh`
和 `Makefile` 写成 CRLF → bash 报 `$'\r': command not found`，
而 `make e2e` / `make test` 是唯一验收入口，报错完全指不到换行符。
`.gitattributes` 随仓库走，换机器不漂移。例外：`*.bat/*.cmd/*.ps1` 保持 CRLF。

提交前自检（三条，都跑一遍）：
```bash
git ls-files --eol | awk '$1 != "i/lf" {print}'   # 应只输出 .bat/.cmd/.ps1
git status --porcelain                             # 应为空
git ls-files | grep -cE '\.exe$|node_modules'      # 应为 0
```
