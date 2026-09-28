# CodeGate 构建入口
#
# 目标：
#   make build        构建 server + agent（当前平台）
#   make test         跑全部测试
#   make race         跑竞态检测
#   make fuzz         跑协议编解码的模糊测试（限时）
#   make cover        生成覆盖率报告
#   make cross        交叉编译六个平台的 Agent
#   make web          构建前端并 embed 到 Server
#   make web-clean    把前端产物清出 dist/，恢复成「未构建」的干净状态
#   make lint         静态检查
#   make probe        ConPTY 探针（R1 验证工具，需要 Python）
#   make server-smoke 服务端端到端冒烟（临时实例，不碰已有数据）
#   make e2e          端到端终端验证（真 ConPTY → Server → 客户端）

SHELL := bash
GO    ?= go
PY    ?= python

# ⚠️ VERSION / LDFLAGS 刻意放在下面「shell 解析」块**之后**，原因见那一节末尾。

# ---- shell 解析（Windows 上必须给绝对路径）----
#
# ⚠️ 上面那行 `SHELL := bash` 在 **Windows 上是静默无效的**。
#
# GNU make 在 Windows 上会忽略 SHELL 的裸名字，强制使用它自己发现的那个
# sh.exe（本机是 w64devkit 自带的）。后果不是报错，而是：
#
#     $ make build
#     go build -trimpath ... -o bin/codegate-agent ./cmd/codegate-agent
#     make: *** [Makefile:54: build] Error 1
#
# **零错误信息、零线索。** 实测根因：那个 sh.exe 跑 go.exe 会 exit 1，
# 且 stdout / stderr 全是空的（`sh.exe -c 'go version'` → exit 1，无输出）。
#
# 而 SHELL 写成**绝对路径**就一切正常。所以 Windows 上显式解析 bash 的绝对路径。
#
# 用 `command -v bash` 而不是硬编码 `C:/Program Files/Git/bin/bash.exe`：
# 硬编码在非默认安装位置（本机就有 scoop 与 PortableGit 两条）会失效。
ifeq ($(OS),Windows_NT)
  SHELL := $(shell command -v bash 2>/dev/null)
  ifeq ($(SHELL),)
    # 退路：PATH 里找不到就按已知安装位置逐个试。
    SHELL := $(shell for c in "$${ProgramFiles}/Git/bin/bash.exe" \
        "$${ProgramFiles}/Git/usr/bin/bash.exe" \
        "$${LOCALAPPDATA}/Programs/Git/bin/bash.exe" \
        "$${USERPROFILE}/scoop/shims/bash.exe" \
        "C:/msys64/usr/bin/bash.exe" \
      ; do if [ -x "$$c" ]; then echo "$$c"; break; fi; done)
  endif
  ifeq ($(SHELL),)
    # ⚠️ 这里的文案必须是 **ASCII**。
    #
    # 原因见下面「中文提示」一节：make 解析 makefile 时按 ANSI 码页读，
    # 中文会变乱码。`$(warning)` / `$(error)` 的文本同样经过解析器，
    # 所以这里没法用中文（recipe 里的中文有 scripts/make-msg.sh 兜底，
    # 解析期的提示没有）。
    $(error No bash.exe found. This Makefile uses Unix-style recipes \
(pipe, $$(), test) and will not work with make's default shell. \
Install Git for Windows, or run: make SHELL=<absolute path to bash>)
  endif
endif

# ---- 版本号：必须放在 SHELL 解析**之后** ----
#
# ★ 顺序陷阱，实测踩过（项目刚纳入 git 时暴露）：
#
#   `VERSION ?= $(shell git describe ...)` 里的 `$(shell)` 是**立即展开** ——
#   `?=` / `:=` 都在解析期求值，用的是**那一刻**的 SHELL。
#
#   而上面 `SHELL := bash`（裸名字）在 Windows 上被静默忽略，所以在这一行
#   展开时，make 用的还是它自己发现的 sh.exe（w64devkit 自带），
#   那个 sh 的 PATH 里**没有 git** → `git describe` 失败 →
#   `2>/dev/null` 把错误吞掉 → `|| echo "dev"`。
#
#   症状极具误导性：仓库明明有提交，`codegate-server version` 却一直打印 `dev`，
#   看起来像「ldflags 没注入」或「main.version 没接上」，其实是求值顺序问题。
#
#   把这两行挪到 SHELL 解析块之后，VERSION 立刻变成 `604832e-dirty`。
#
#   `--always` 保证无 tag 时回落到短哈希（否则永远 `dev`）；
#   `--dirty` 在工作区有未提交改动时加后缀 —— 正好提醒
#   「这个二进制不是从干净提交构建出来的」。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

# ---- 中文提示：为什么 recipe 里不直接 echo ----
#
# ⚠️ Windows 上的 GNU make（实测 w64devkit 的 4.4.1）**把 Makefile 当成
#    ANSI 代码页（本机 GBK）解析**，于是 recipe 里的中文输出是乱码：
#
#      $ make server
#      go build ... -o bin/codegate-server.exe ./cmd/codegate-server
#      宸叉瀯寤? bin/codegate-server        ← 「已构建」
#      codegate-server dev
#
#    字节没丢，只是被按错码表读了一遍：`已构建` 的 UTF-8 是
#    E5 B7 B2 E6 9E 84 E5 BB BA，按 GBK 逐对读正好是 `宸叉瀯寤` + 半个字符。
#
#    加 UTF-8 BOM **无效**（实测 GNU make 4.4.1 不认），也没有环境变量开关。
#
#    所以中文提示统一走 `bash scripts/make-msg.sh <key>` —— bash 读 UTF-8 正常，
#    make 只传一个 ASCII 的 key。新增提示时同步改那个脚本。
MSG := bash scripts/make-msg.sh

# Windows 上可执行文件必须有 .exe 后缀。
#
# ⚠️ `go build -o bin/foo` 在 Windows 上**不会**自动补 .exe ——
#    Go 只在 -o 指向一个目录时才补。结果是一个无法直接运行的 `bin/foo`，
#    而 scripts/server-smoke.sh 会优先去找 `bin/foo.exe`，于是它跑去执行
#    上一次构建留下的**旧二进制**，表现得像"改动完全没生效"。
#    实测踩过：webui 已经接进路由，冒烟测试却报 404 page not found。
#
#    用 `go env GOEXE` 拿到平台正确后缀，一次解决两个平台。
#
# 加一层兜底：go 不在 PATH 时 `go env GOEXE` 会失败并返回空串，
# 于是又回到「产出无后缀文件」那个坑。Windows 上直接假定 .exe。
GOEXE := $(shell go env GOEXE 2>/dev/null)
ifeq ($(GOEXE),)
  ifeq ($(OS),Windows_NT)
    GOEXE := .exe
  endif
endif

# ---- 本机 go 环境 ----
#
# 这台机器上 go 不在系统 PATH 里（绿色解压安装），另外还有代理、
# CGO、-vet=off 等一堆本机专属设置，全都在 scripts/goenv.sh。
#
# 每个需要 go 的 recipe 前面 source 一次。写成变量前缀而不是改 SHELL：
# 改 SHELL 会连带影响所有 recipe，包括那些根本不需要 go 的（cp / rm / npm），
# 而 goenv.sh 会设代理和 PATH，波及面没必要那么大。
#
# ★ 文件不存在时展开成空 —— 换一台干净的机器（没有 goenv.sh）
#   就是「什么都不做」，`make build` 用系统 PATH 里的 go 正常跑。
GOENV := $(if $(wildcard scripts/goenv.sh),. ./scripts/goenv.sh >/dev/null; ,)

.PHONY: all build test race fuzz cover cross web web-deps web-check web-dev web-clean lint vet fmt tidy clean probe help \
        ttyprobe ttyprobe-unit ttyprobe-test agent agent-test \
        server server-test server-smoke e2e

all: test build

## ---- Go ----

# bin 作为目录依赖：不存在则创建，存在则跳过。
# 用目录当 target 是安全的 —— make 判断的是「目标是否存在」，
# 而目录一旦建好就不会再触发重建。
bin:
	@mkdir -p bin

# codegate-server 与 codegate-agent 一起构建。
#
# ★ 两者都进 build 而不是分成两个目标：`make build` 应当产出
# 「一套能跑起来的东西」。只建 agent 的话，新克隆仓库的人
# 会拿到一个连不上的客户端，还得自己去翻 Makefile 才知道少了什么。
build: bin
	@$(GOENV)$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/codegate-agent$(GOEXE) ./cmd/codegate-agent
	@$(GOENV)$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/codegate-server$(GOEXE) ./cmd/codegate-server
	@ls -lh bin/

test:
	@$(GOENV)$(GO) test -count=1 ./...

# ⚠️ 关于 -race：它在本机**可以**跑，但需要先 source scripts/goenv.sh。
#
#   历史记录里曾写过「本机没有 C 工具链，-race 跑不了」—— **那是错的**。
#   实测（2026-09-28）在 goenv.sh 下 `make race` 全 13 个包通过，耗时 3 分 42 秒。
#
#   goenv.sh 做的是：CGO_ENABLED=1 + CC 指向 w64devkit 自带的 gcc.exe
#   （w64devkit 同时提供了 make，见 w64devkit 的 bin 目录）。
#   没有它的话 go 会报 `-race requires cgo`，那个报错容易被误读成
#   「这台机器装不了」—— 其实只是环境变量没设。
#
#   代价是编译明显变慢（cgo 会走 gcc 而不是纯 Go 编译链），
#   所以别把它塞进日常循环，交给 CI 或提交前跑一次。
#
#   recipe 里先 source scripts/goenv.sh（存在的话），免得每次都要
#   手动 source 一遍 —— 少了它那行检查就会直接拦住，而报错信息
#   「-race requires cgo」很容易被读成「这台机器装不了」。
race:
	@$(GOENV)if [ "$$(go env CGO_ENABLED)" != "1" ]; then \
	  $(MSG) race-needs-cgo; \
	  exit 1; \
	fi; \
	$(GO) test -race -count=1 ./...

fuzz:
	@$(GOENV)$(GO) test -run=XXX -fuzz=FuzzDecodeFrame -fuzztime=30s ./internal/protocol/
	@$(GOENV)$(GO) test -run=XXX -fuzz=FuzzDecodeEnvelope -fuzztime=30s ./internal/protocol/

cover:
	@$(GOENV)$(GO) test -coverprofile=coverage.txt -covermode=atomic ./...
	@$(GOENV)$(GO) tool cover -func=coverage.txt | tail -20

cross:
	@mkdir -p dist
	@$(GOENV)for os in windows linux darwin; do \
	  for arch in amd64 arm64; do \
	    echo ">> codegate-agent $$os/$$arch"; \
	    GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
	      -o dist/codegate-agent-$$os-$$arch$$( [ $$os = windows ] && echo .exe ) ./cmd/codegate-agent; \
	  done; \
	done
	@ls -lh dist/

## ---- 质量 ----
#
# ⚠️ go vet 在本机（亿赛通 DLP 环境）会崩：
#      vet.exe -flags failed: exit status 0xc0000005  (ACCESS_VIOLATION)
#    标准库源码确认是明文，所以不是加密问题，更像 DLP 驱动 hook 进程创建。
#    本地 workaround 是 GOFLAGS=-vet=off（由 scripts/goenv.sh 设置）。
#    ★ 这是【本机专属】，不要写进 CI —— 干净机器上 vet 应该正常跑。

vet:
	@$(GOENV)$(GO) vet ./...

fmt:
	@$(GOENV)$(GO) fmt ./...

tidy:
	@$(GOENV)$(GO) mod tidy

lint: vet
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || $(MSG) lint-no-golangci

## ---- Agent（P3 交付物）----
#
# codegate-agent 是常驻在本机的那一端：只出站连接，不需要公网 IP。
# 构建后可以直接用：
#   bin/codegate-agent doctor   环境自检（PTY / 目录 / 密钥 / 配置）
#   bin/codegate-agent pair     生成配对码，把本机绑到账号
#   bin/codegate-agent run      启动

agent: bin
	@$(GOENV)$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/codegate-agent$(GOEXE) ./cmd/codegate-agent
	@$(MSG) agent-built $(GOEXE)
	@bin/codegate-agent$(GOEXE) version

agent-test:
	@$(GOENV)$(GO) test -count=1 -vet=off -v ./internal/agent

## ---- Server（P4 交付物）----
#
# codegate-server 是服务端：REST API + 两条 WebSocket 通路 + 中继。
# 构建后可以直接用：
#   bin/codegate-server doctor            环境自检（目录 / 数据库 / 密钥 / 端口）
#   bin/codegate-server user add          创建账号
#   bin/codegate-server serve             启动
#
# ⚠️ 服务端跑在**别的机器**上（有公网地址那台），所以这里只建本机平台。
#    跨平台分发用 `make cross`。

server: bin
	@$(GOENV)$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/codegate-server$(GOEXE) ./cmd/codegate-server
	@$(MSG) server-built $(GOEXE)
	@bin/codegate-server$(GOEXE) version

server-test:
	@$(GOENV)$(GO) test -count=1 -vet=off -v ./internal/server

# 端到端冒烟：起一个临时实例，走完注册 → 登录 → 设备列表 → 票据 → 退出。
# 用临时数据库和随机端口，不碰用户已有的数据。
server-smoke: server
	@bash scripts/server-smoke.sh

## ---- 端到端（终端数据通路）----
#
# e2e 是**唯一**覆盖终端数据通路的验证：
#
#   Agent（真 ConPTY，跑真进程）──二进制帧──▶ Server（中继 + ring buffer）──▶ 客户端
#
# 它补的是另外两个脚本都碰不到的盲区：
#
#   scripts/server-smoke.sh      只到 HTTP 层（状态码 / 认证 / 缓存头）
#   scripts/web-render-check.sh  只验前端能不能挂载
#
# 这条链路断掉时的症状是「网页能打开、能登录、能点新建会话，
# 然后终端永远空白」—— 而上面两个脚本的所有断言都是绿的。
#
# 实测抓出过两个只在多订阅者 / 有 pending 请求时才暴露的 bug：
#   1. session.close 的响应永远回不到客户端（前端卡到 60 秒超时）
#   2. 会话结束时其他观察者收不到通知（订阅关系先被删了才广播）
# 两者的回归测试在 internal/server/ws_session_test.go。
#
# 全程临时目录 + 随机端口，不碰用户已有数据。约 15 秒。

e2e: build
	@bash scripts/e2e-terminal.sh

## ---- 前端 ----
#
# 前端产物会被 go:embed 进 Server（internal/server/webui）。
#
# ⚠️ 关于几条刻意的写法：
#
# 1. 用 `npm install` 而不是 `npm ci`。
#    `npm ci` 的第一步是**清空整个 node_modules** —— 在本机（装了
#    亿赛通 DLP 的环境）这会撞上安全删除守卫直接失败（一次删 1400+ 项），
#    而且它的价值（可复现安装）在这里由 package-lock.json 提供就够了。
#
# 2. 只删 `assets/`，不删整个 dist。
#    dist/ 里有一个必须保留的 .gitkeep —— 它是 `//go:embed all:dist`
#    在「前端未构建」时仍然成立的前提。删掉它、紧接着 vite build 又失败，
#    仓库就处于「整个项目编译不过」的状态，而失败原因看起来
#    和前端毫无关系。只清 assets 既去掉了上一版的哈希文件，
#    又保证 .gitkeep 始终在。
#
# 3. 跑完必须重新编译 Server。
#    产物是在**编译期**嵌进二进制的，不重编的话二进制里还是旧的界面。

web: web-deps
	cd web && npm run build
	@mkdir -p internal/server/webui/dist
	@rm -rf internal/server/webui/dist/assets
	@cp -r web/dist/. internal/server/webui/dist/
	@$(MSG) web-synced

# 把 dist/ 恢复成「未构建」的干净状态。
#
# 用途：提交前跑一次，保证版本库里只有 .gitkeep 而不含构建产物。
# 跑完之后二进制里内嵌的仍是上一次构建的结果（embed 在编译期求值），
# 所以要接着 `make server` 才能让二进制和磁盘状态一致。
web-clean:
	@rm -rf internal/server/webui/dist/assets internal/server/webui/dist/index.html
	@$(MSG) web-cleaned

# node_modules 作为文件依赖：package.json 变了才重装。
web/node_modules: web/package.json
	cd web && npm install

web-deps: web/node_modules

# 只跑类型检查（比 build 快，改完代码先跑这个）
web-check: web-deps
	cd web && npm run typecheck

# 开发模式：Vite 开发服务器（5173），/api 代理到本地 Server（8080）。
# 改前端时用这个，不要反复 make web —— 那每次都要重编 Go 二进制。
web-dev: web-deps
	cd web && npm run dev

## ---- 调试工具 ----
# 这两个工具是 Phase 0 做 R1 风险评估时写的，留着用于日常排查 PTY 问题。
# 不依赖 Go，直接用 Python + ctypes 调 ConPTY。

probe:
	@$(PY) tools/conpty_probe.py --seconds 8 --cols 100 --rows 30 --out captures/probe.bin -- $(CMD)
	@$(PY) tools/ansi_report.py captures/probe.bin

## ---- PTY 测试夹具（P2 交付物）----
#
# ttyprobe 是 PTY 的测试夹具：报告自己的尺寸/控制台模式，回显 stdin，
# 报告 resize 和 0x03。它的集成测试用真实 ConPTY 跑完整链路 ——
# 这是 P2 的验收依据，也是排查 PTY 问题的第一件工具。
#
# 为什么放在 cmd/ 而不是文档 §6.4 建议的 internal/terminal/testdata/：
#   Go 工具链会**完全忽略** testdata 目录，那里的代码不会被编译检查，
#   也就永远不会因为改动而报错。一个不会被编译的测试夹具是维护陷阱。

ttyprobe: bin
	@$(GOENV)$(GO) build -o bin/ttyprobe$(GOEXE) ./cmd/ttyprobe
	@$(MSG) probe-built $(GOEXE)
	@$(MSG) probe-usage $(GOEXE)

# 只跑探针的单元测试（纯逻辑，秒级）
ttyprobe-unit:
	@$(GOENV)$(GO) test -count=1 -vet=off ./cmd/ttyprobe

# 跑完整集成测试（真实 ConPTY，约 60-70 秒）
ttyprobe-test:
	@$(GOENV)$(GO) test -count=1 -vet=off -v ./cmd/ttyprobe

clean:
	rm -rf bin dist coverage.txt captures

# 打印文件头的目标清单。
#
# ⚠️ 原来写的是 `grep -E '^## ' Makefile` —— 它只匹配小节标题
#    （`## ---- Go ----` 这种），于是 help 只输出 7 行 `---- Go ----`，
#    一个可用目标都列不出来。目标清单其实在文件头的注释块里。
help:
	@awk '/^#/ {sub(/^# ?/,""); print; next} {exit}' Makefile
