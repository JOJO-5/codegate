#!/usr/bin/env bash
#
# Makefile 的中文提示出口。
#
# # 为什么需要这个脚本（不是过度设计）
#
# Windows 上的 GNU make（实测 w64devkit 自带的 4.4.1）**把 Makefile 当成
# ANSI 代码页（本机是 GBK）解析**，于是 recipe 里的中文在输出时变成乱码：
#
#     $ make server
#     go build -trimpath ... -o bin/codegate-server.exe ./cmd/codegate-server
#     宸叉瀯寤? bin/codegate-server        ← 这就是「已构建」
#     codegate-server dev
#
# 原因很确定：`已构建` 的 UTF-8 字节是 E5 B7 B2 E6 9E 84 E5 BB BA，
# 按 GBK 逐对读出来正好是 `宸叉瀯寤` + 半个字符 —— 一个字节都没丢，只是读错了码表。
# 同理 `echo` 和 `printf` 都躲不过，因为**乱在 make 解析 makefile 的那一刻**，
# 不在执行时。
#
# 给 Makefile 加 UTF-8 BOM **无效**（实测：GNU make 4.4.1 不认 BOM，
# 加了照样乱码）。也查不到可用的环境变量开关。
#
# 所以唯一可行的办法是：**让中文不经过 make 的解析器**。这个脚本由 bash 读取
# （bash 处理 UTF-8 正常），make 只负责把 key 传进来。
#
# 代价是提示文案与 recipe 分居两处。为了让这件事不失控，约定：
#   - 每个 key 只在这里出现一次；
#   - recipe 里紧挨着写一行注释指明 key 的用途；
#   - 新增提示时同步更新本文件的 case 分支。
#
# # 用法
#
#     bash scripts/make-msg.sh <key> [参数...]
#
# 未知 key 会明确报错并列出可用 key —— 不要静默什么都不打印，
# 那样 recipe 看起来会像"跑成功了但没输出"。

set -uo pipefail

key="${1:-}"
shift || true

case "$key" in
  # make server / make agent
  server-built)  printf '已构建: bin/codegate-server%s\n' "$*" ;;
  agent-built)   printf '已构建: bin/codegate-agent%s\n' "$*" ;;

  # make ttyprobe
  probe-built)   printf '探针已构建: bin/ttyprobe%s\n' "$*" ;;
  probe-usage)   printf '手动用法（在真实终端里）: bin/ttyprobe%s\n' "$*" ;;

  # make web
  web-synced)    printf '前端产物已拷入 internal/server/webui/dist/。\n重新编译 Server 才会生效: make server\n' ;;

  # make web-clean
  web-cleaned)   printf 'dist/ 已恢复为未构建状态（只剩 .gitkeep）。\n记得 make server 重编 —— 产物是在编译期嵌进二进制的。\n' ;;

  # make race
  race-needs-cgo)
    printf '!! 本机 CGO_ENABLED=0，-race 不可用。请在 Linux/CI 上跑，或先装 C 工具链。\n' >&2
    ;;

  # make lint
  lint-no-golangci)
    printf '（未安装 golangci-lint，已跳过；go vet 已经跑过）\n'
    ;;

  # Makefile 头部在 Windows 上找不到 bash 时的警告
  no-bash)
    printf '找不到 bash.exe —— 回退到 make 的默认 shell。\n' >&2
    printf '本 Makefile 用的是 Unix 风格写法（管道 / $() / test），多半会失败。\n' >&2
    printf '装一个 Git for Windows，或用 make SHELL=<bash 绝对路径> 覆盖。\n' >&2
    ;;

  ""|-h|--help)
    printf '用法: bash scripts/make-msg.sh <key>\n\n可用 key:\n' >&2
    grep -oE '^  [a-z-]+\)' "$0" | tr -d ' )' | sed 's/^/  /' >&2
    exit 2
    ;;

  *)
    printf 'make-msg.sh: 未知的 key「%s」。可用 key:\n' "$key" >&2
    grep -oE '^  [a-z-]+\)' "$0" | tr -d ' )' | sed 's/^/  /' >&2
    exit 2
    ;;
esac
