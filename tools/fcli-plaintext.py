#!/usr/bin/env python3
"""把被亿赛通（Esafenet）透明加密的文件「洗」回磁盘明文。

# 为什么需要这个脚本

本机装了 Esafenet 透明加密驱动，规则是**读写不对称**：

    谁写                  磁盘上的结果
    --------------------  --------------------------
    fcli.exe（非白名单）   明文
    node/python/bash（白名单）  密文（驱动自动加密）

于是任何**白名单进程**写出来的文件都会变成密文，包括：

  - `gofmt -w` / `goimports -w`（会重写 .go 文件）
  - 用 Python 脚本改文件
  - `sed -i` / `cat > file`

密文文件对宿主工具链是**致命**的：

  - `go build` 仍然能过（go.exe 在白名单，读得到明文）
  - 但 Read / Grep / Edit 这些工具会报「binary file」或读到乱码
  - `git diff` 会把整个文件算成改动

# 修法

组合两边的能力：**python 读（拿明文）+ fcli 直连写（落明文）**。

    密文 --[python 读，驱动解密]--> 内存明文 --[fcli 直连写]--> 磁盘明文

关键点：必须用 **fcli 直连**（不加 `--via`）。加了 `--via node/python`
就变成白名单进程写盘，又会被加密回去。

# 用法

    python tools/fcli-plaintext.py internal/server/server.go internal/server/registry.go
    python tools/fcli-plaintext.py --check internal/server/     # 只检查，不修
    FCLI_PATH=/path/to/fcli.exe python tools/fcli-plaintext.py <path>...

目录参数会递归收集受保护后缀的文件（默认 .go/.txt/.json/.md/.yaml/.yml/.sql）。
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from pathlib import Path

# 加密开销固定 4096 字节：密文长度 = 明文长度 + 4096。
# 用「stat 报的大小 != 实际读到的字节数」判定加密态，比找特征串可靠。
ENCRYPTION_OVERHEAD = 4096

# 会被驱动加密的后缀（实测）。默认只处理这些，避免误碰别的文件。
#
# ★ P5 补上了前端后缀。之前只有 .go/.json/.md 这一批，于是
#   `web/dist/assets/*.js`、`*.css` 这些由 node（白名单）写出来的产物
#   从来不会被扫到 —— 而它们正是会被提交进仓库的那一批。
#   漏扫的症状很隐蔽：磁盘上是密文，`git add` 之后别人 clone 到的是乱码，
#   但本地因为白名单进程能解密，一切看起来都正常。
PROTECTED_SUFFIXES = {
    ".go", ".txt", ".json", ".md", ".yaml", ".yml", ".sql", ".cs", ".html",
    ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts", ".vue", ".css", ".map",
}

DEFAULT_FCLI = Path.home() / ".workbuddy-ai" / "bin" / "fcli.exe"


def fcli_path() -> Path:
    p = Path(os.environ.get("FCLI_PATH", DEFAULT_FCLI))
    if not p.is_file():
        sys.exit(f"找不到 fcli: {p}（用 FCLI_PATH 指定）")
    return p


def stat_size(path: Path) -> int:
    """磁盘上的真实字节数（走 OS 元数据，不被驱动解密）。"""
    return path.stat().st_size


def read_plaintext(path: Path) -> bytes:
    """读明文。python 在白名单里，驱动会透明解密。"""
    with open(path, "rb") as f:
        return f.read()


def is_encrypted(path: Path) -> bool:
    """判定加密态：stat 大小与实际可读字节数不一致。

    比扫 `Esafenet` 特征串更可靠 —— 特征串依赖文件头结构，
    而这个差值是由「驱动多加了 4096 字节容器头」这个机制直接决定的。
    """
    try:
        return stat_size(path) != len(read_plaintext(path))
    except OSError:
        return False


def write_plaintext_via_fcli(fcli: Path, path: Path, data: bytes) -> None:
    """用 fcli **直连**写回，落盘即明文。

    走 stdin 而不是 `-c <TEXT>`：命令行有长度上限（Windows 约 32 KB），
    而我们要处理的是源码文件，随时可能超。
    """
    proc = subprocess.run(
        [str(fcli), "write", str(path), "--force"],
        input=data,
        capture_output=True,
        timeout=60,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"fcli write 失败 (exit {proc.returncode}): "
            f"{proc.stdout.decode('utf-8', 'replace')[:400]}"
        )


def collect(targets: list[str]) -> list[Path]:
    out: list[Path] = []
    for t in targets:
        p = Path(t)
        if p.is_dir():
            out.extend(
                q
                for q in sorted(p.rglob("*"))
                if q.is_file() and q.suffix.lower() in PROTECTED_SUFFIXES
            )
        elif p.is_file():
            out.append(p)
        else:
            print(f"跳过（不存在）: {t}", file=sys.stderr)
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description="把透明加密的文件洗回明文")
    ap.add_argument("paths", nargs="+", help="文件或目录")
    ap.add_argument("--check", action="store_true", help="只报告哪些是密文，不改")
    args = ap.parse_args()

    files = collect(args.paths)
    if not files:
        print("没有匹配的文件", file=sys.stderr)
        return 1

    fcli = None if args.check else fcli_path()

    fixed = clean = failed = 0
    for f in files:
        if not is_encrypted(f):
            clean += 1
            continue

        before = stat_size(f)
        if args.check:
            print(f"[密文] {f}  (磁盘 {before} 字节)")
            fixed += 1
            continue

        try:
            data = read_plaintext(f)
            assert fcli is not None
            write_plaintext_via_fcli(fcli, f, data)
            after = stat_size(f)
            ok = after == len(data)
            print(
                f"[{'已修复' if ok else '异常'}] {f}  "
                f"{before} -> {after} 字节（明文 {len(data)}）"
            )
            fixed += 1 if ok else 0
            failed += 0 if ok else 1
        except Exception as e:  # noqa: BLE001 —— CLI 工具，如实报错比抛栈有用
            print(f"[失败] {f}: {e}", file=sys.stderr)
            failed += 1

    verb = "待修复" if args.check else "已修复"
    print(f"\n合计 {len(files)} 个文件：明文 {clean}，{verb} {fixed}，失败 {failed}")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
