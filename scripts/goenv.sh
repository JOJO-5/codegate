#!/usr/bin/env bash
# CodeGate 开发环境设置
#
# 用法：
#   source scripts/goenv.sh
#
# 这台机器上跑 Go 有六个坑，都在这里一次性解决（详见 .workbuddy-ai/memory）：
#
#  1. Go 是绿色解压安装的（~/.workbuddy-ai/binaries/go/），不在系统 PATH 里。
#  2. 环境变量里的 http_proxy 指向 127.0.0.1:<坏端口>，一律 502 CONNECT tunnel failed。
#     真正可用的是 192.168.9.163:10808。Go 拉模块会读 http_proxy，不覆盖就拉不到。
#  3. 本机 DNS 只能通过代理解析 —— 国内镜像（阿里云、goproxy.cn）
#     直连时是 Could not resolve host，反而不可用。别想着换镜像绕开。
#  4. PATH 里的 Go 目录必须写成 /c/... 形式；写成 C:/... 会被环境拦掉
#     （但 GOROOT/GOPATH 必须用 C:/... 形式，Go 本身读不懂 /c/...）。
#  5. ★ Go SDK 必须用【非白名单】工具解压，否则整个标准库是密文。
#  6. ★ 本机 go vet 不可用（vet.exe 崩在 0xc0000005），go test 必须加 -vet=off。

# ---- Go 工具链 ----
#
# ★ GOROOT 指向 1.27.1-plain 而不是 1.27.1，这不是笔误。
#
#   亿赛通透明加密的规则是「读要白名单，写要非白名单」：
#   python 在白名单里，所以用它解压出来的 Go 源码会被驱动【加密】，
#   而 go 工具链（asm.exe / compile.exe）不在白名单，读到的是密文，
#   报错长这样：
#       src/sync/atomic/asm.s:1:1: invalid UTF-8 encoding
#       src/sync/atomic/asm.s:1: expected identifier, found "\u0a11"
#
#   修法：用 Windows 自带的 tar.exe（不在白名单）解压，落盘即明文：
#       C:/Windows/System32/tar.exe -xf go1.27.1.windows-amd64.zip -C <目标目录>
#   1.27.1/ 目录是第一次用 python 解压的，整个标准库是密文，已废弃。
export GOROOT="C:/Users/JOJO/.workbuddy-ai/binaries/go/versions/1.27.1-plain/go"
export GOPATH="C:/Users/JOJO/.workbuddy-ai/binaries/go/gopath"

# PATH 用 /c/ 形式
export PATH="/c/Users/JOJO/.workbuddy-ai/binaries/go/versions/1.27.1-plain/go/bin:$PATH"
export PATH="/c/Users/JOJO/.workbuddy-ai/binaries/go/gopath/bin:$PATH"

# ---- 网络 ----
export HTTP_PROXY="http://192.168.9.163:10808"
export HTTPS_PROXY="http://192.168.9.163:10808"
export http_proxy="$HTTP_PROXY"
export https_proxy="$HTTPS_PROXY"
export NO_PROXY="localhost,127.0.0.1,::1"
export no_proxy="$NO_PROXY"

# ---- C 编译器（go test -race 需要 cgo） ----
#
# 本机原本没有任何 C 编译器，-race 直接不可用：
#   go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
#   gcc: executable file not found in %PATH%
#
# w64devkit 是一个自带 mingw-w64 gcc 的绿色包（解压即用，无需安装）。
# 注意它也是 SFX 自解压 exe —— 用 7z 解，不要运行它：
#   "/c/Program Files/7-Zip/7z.exe" x -y -o<目标> w64devkit-x64-2.10.0.7z.exe
export CGO_ENABLED=1
export CC="C:/Users/JOJO/.workbuddy-ai/binaries/w64devkit/2.10.0/w64devkit/bin/gcc.exe"

# ★ 追加到 PATH **末尾**，不要放最前面。
#
# w64devkit 自带一个 sh.exe。放最前面会让它抢走 PATH 里第一个 sh ——
# 而 GNU make 的默认 SHELL 就是「PATH 里的 sh」，于是 make 改用
# w64devkit 的 sh 执行 recipe，在那个环境里 `go env` 会失败。
# 现象极具误导性：`make agent` 报 `Error 1` 但**没有任何错误输出**，
# 而同一行命令手动跑完全正常。
#
# 放末尾既保证 gcc 可被找到（cgo/-race 需要），又不会劫持 make 的 shell。
export PATH="$PATH:/c/Users/JOJO/.workbuddy-ai/binaries/w64devkit/2.10.0/w64devkit/bin"

# ---- 本机专属 workaround ----
#
# go vet 在这台机器上会崩：
#   vet.exe -flags failed: exit status 0xc0000005   (ACCESS_VIOLATION)
# 标准库源码确认是明文，所以不是加密问题 —— 更像 DLP 驱动 hook 进程创建导致的。
# 而 go test 默认会跑 vet 的一个子集，于是所有测试都会 setup failed。
# 因此在环境层直接关掉它。
#
# ⚠️ 这是【本机 workaround】，不要写进 CI 配置、不要提交到仓库的 Makefile 默认值里。
#    换一台干净的机器应该把这一行去掉，让 vet 正常跑。
export GOFLAGS="${GOFLAGS:--vet=off}"

echo "CodeGate env ready: $(go version 2>/dev/null || echo 'go 未找到，检查 GOROOT')"
echo "  CGO_ENABLED=$CGO_ENABLED  race=$(gcc --version >/dev/null 2>&1 && echo 可用 || echo 不可用)"
