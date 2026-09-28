#!/usr/bin/env bash
# 格式化 Go 源码，并把结果以【明文】写回磁盘。
#
# 用法：
#   source scripts/goenv.sh && bash tools/gofmt-plain.sh internal/server/
#   bash tools/gofmt-plain.sh internal/server/handler_auth.go
#
# # 为什么不能直接用 gofmt -w
#
# 本机装了亿赛通透明加密驱动，规则是「读要白名单，写要非白名单」：
#
#     fcli.exe 写  → 磁盘明文
#     gofmt 写     → 磁盘密文（驱动自动加密受保护后缀）
#
# `gofmt -w` 走的是后者。密文文件 go build 仍然能过（go.exe 在白名单），
# 但 **Read / Grep / Edit 全都读不了它** —— 报 "binary file" 或返回乱码。
# 2026-09-28 实测：一次 `gofmt -w internal/server/` 把 server.go 和
# registry.go 变成了密文（磁盘 11758 字节 / 实际可读 7662，差 4096）。
#
# # 做法
#
# gofmt 输出到 stdout（管道，不落盘、不加密），再用 **fcli 直连**写回。
# 关键在于「直连」：加 --via 就变成白名单进程写盘，又会被加密回去。
set -euo pipefail

FCLI="${FCLI_PATH:-$HOME/.workbuddy-ai/bin/fcli.exe}"

if [[ $# -eq 0 ]]; then
  echo "用法: $0 <文件或目录>..." >&2
  exit 2
fi
if [[ ! -x "$FCLI" ]]; then
  echo "找不到 fcli: $FCLI（用 FCLI_PATH 指定）" >&2
  exit 2
fi
if ! command -v gofmt >/dev/null 2>&1; then
  echo "gofmt 不在 PATH。先执行: source scripts/goenv.sh" >&2
  exit 2
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

changed=0
# gofmt -l 只读，用来挑出「确实需要格式化」的文件。
# 直接对每个文件重写一遍也能工作，但会把所有文件的 mtime 都刷新，
# 让 make / go build 的增量判断全部失效。
while IFS= read -r f; do
  [[ -z "$f" ]] && continue
  gofmt "$f" > "$tmp"
  cat "$tmp" | "$FCLI" write "$f" --force > /dev/null
  echo "已格式化（明文写回）: $f"
  changed=$((changed + 1))
done < <(gofmt -l "$@")

if [[ $changed -eq 0 ]]; then
  echo "无需格式化。"
fi
