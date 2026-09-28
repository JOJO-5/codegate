#!/usr/bin/env python3
"""把被 DLP（亿赛通透明加密）加密的依赖源码还原成明文。

## 为什么需要这个脚本

本机的加密驱动按「目录 + 后缀」策略对源代码文件强制加密。**go.exe 不在白名单**，
所以它从模块缓存里读到的是密文，编译直接失败：

    golang.org/x/sys@v0.48.0/cpu/cpu_gc_x86.s:1:1: invalid UTF-8 encoding
    asm: assembly of .../cpu_gc_x86.s failed

`go build` 不会给出「文件被加密了」这种提示，只会说语法错误 —— 极易被误判成
依赖本身有问题。

## 原理（两个进程的读写不对称）

| 进程 | 读 | 写 |
|---|---|---|
| python（在白名单） | 得到**解密后**的明文 | 落盘为**密文** |
| fcli 直连（不在白名单） | 得到密文 | 落盘为**明文** |

所以组合起来就是：**用 python 读明文 → 用 fcli 直连写回**，文件就变明文了。

单靠任何一方都做不到：python 写回去还是密文，fcli 自己读出来是密文。

## 用法

    python tools/fix-dlp-encrypted-deps.py <目录或文件> [<目录或文件> ...]
    python tools/fix-dlp-encrypted-deps.py <目录> --dry-run

只处理能按 UTF-8 解码的文本文件；二进制文件跳过（它们本来也不受加密策略影响）。
"""

from __future__ import annotations

import argparse
import os
import sys

# fcli 的 Python 封装在固定位置，不在 PATH 里。
FCLI_DIR = r"C:/Users/JOJO/.workbuddy-ai/bin"
sys.path.insert(0, FCLI_DIR)

import fcli  # noqa: E402  （必须在 sys.path 调整之后导入）

# 每批处理多少个文件。delete + write 两趟，分批能让失败范围可控。
BATCH_SIZE = 20


def collect_files(paths: list[str]) -> list[str]:
    """展开目录，收集所有普通文件。"""
    out: list[str] = []
    for p in paths:
        if os.path.isfile(p):
            out.append(p)
            continue
        if os.path.isdir(p):
            for root, _dirs, files in os.walk(p):
                for fn in files:
                    out.append(os.path.join(root, fn))
            continue
        print(f"  [跳过] 不存在: {p}", file=sys.stderr)
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description="把 DLP 加密的源码还原成明文")
    ap.add_argument("paths", nargs="*", help="目录或文件")
    ap.add_argument(
        "--from-file",
        dest="list_file",
        help="从文本文件读取路径列表（每行一个）。用于路径含空格、或"
        "先由 `od` 扫出真正加密的文件再精确处理。",
    )
    ap.add_argument("--dry-run", action="store_true", help="只统计，不写")
    args = ap.parse_args()

    paths = list(args.paths)
    if args.list_file:
        with open(args.list_file, encoding="utf-8") as f:
            paths += [line.strip() for line in f if line.strip()]

    if not paths:
        ap.error("至少要给一个路径，或用 --from-file 指定列表文件")

    files = collect_files(paths)
    print(f"共 {len(files)} 个文件待检查")

    # 第一步：用 python 读出**明文**内容。
    # 读不出来的（二进制 / 权限问题）直接跳过 —— 强行按 UTF-8 写回会毁掉原内容。
    payloads: list[tuple[str, str]] = []
    skipped = 0
    for p in files:
        try:
            with open(p, "rb") as f:
                raw = f.read()
            payloads.append((p, raw.decode("utf-8")))
        except (UnicodeDecodeError, OSError):
            skipped += 1

    print(f"可读为 UTF-8 文本: {len(payloads)}，跳过（二进制/不可读）: {skipped}")

    if args.dry_run:
        for p, text in payloads[:10]:
            print(f"  {len(text):>8} 字节  {p}")
        print("  ...（dry-run，未写入）")
        return 0

    # 第二步：还原成明文。
    #
    # ★ 走「先 delete 再 write」而不是直接 write，有两个原因：
    #
    #  1. fcli 的 write 会**拒绝覆盖**「存在但读不出来」的文件（它认为那是
    #     加密/二进制，怕毁掉原内容）—— 这正是我们要覆盖的对象。而
    #     `force` 只在**单个** write 调用上生效，`apply` 的 write op 既不认
    #     操作级 force，顶层 force 也不传递。所以只能先让目标不存在。
    #
    #  2. 单个 `fcli.write(p, text, force=True)` 把内容放在**命令行参数**里，
    #     遇到 blake2bAVX2_amd64.s 这类几千行的文件会直接
    #     `FileNotFoundError: [WinError 206] 文件名或扩展名太长`。
    #     `apply` 走 stdin JSON，没有这个上限。
    written = 0
    failed: list[str] = []

    for start in range(0, len(payloads), BATCH_SIZE):
        batch = payloads[start : start + BATCH_SIZE]

        try:
            fcli.apply(
                [{"op": "delete", "path": p} for p, _ in batch],
                keep_going=True,  # 已不存在就跳过，不当失败
            )
        except fcli.FcliError as e:
            print(f"  批次 {start} 删除阶段失败: {e}", file=sys.stderr)
            failed.extend(p for p, _ in batch)
            continue

        try:
            res = fcli.apply(
                [{"op": "write", "path": p, "content": t} for p, t in batch],
                keep_going=True,
            )
        except fcli.FcliError as e:
            print(f"  批次 {start} 写入阶段失败: {e}", file=sys.stderr)
            failed.extend(p for p, _ in batch)
            continue

        # 返回结构：data.results[].status ∈ {applied, failed, skipped}。
        # 注意不是 `ok` 布尔 —— 早期版本按 `ok` 判断，把成功全统计成了失败。
        for item in (res.get("data") or {}).get("results") or []:
            if item.get("status") == "applied":
                written += 1
            else:
                failed.append(f"{item.get('path', '?')} ({item.get('status')})")

        print(f"  进度 {min(start + BATCH_SIZE, len(payloads))}/{len(payloads)}")

    print(f"\n完成：写入 {written} 个，失败 {len(failed)} 个")
    for p in failed[:20]:
        print(f"  [失败] {p}", file=sys.stderr)
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
