#!/usr/bin/env python3
"""扫描二进制/文本，识别 TUI 框架与终端能力协商特征。

用途：CodeGate Phase 0 风险评估（R1）——搞清目标 CLI 用什么渲染，
以及它们会向终端请求哪些能力（备用屏幕、鼠标、括号粘贴、同步输出等）。
这些能力直接决定 ConPTY 中转的兼容性。

用法: python scan_tui.py <path> [<path> ...]
"""
import mmap
import os
import sys

ESC = b"\x1b"

# (字节特征, 人类可读名称, 分类)
SIGS = [
    # ---- 终端模式协商：决定 TUI 是否走备用屏幕、是否需要鼠标 ----
    (ESC + b"[?1049h", "DECSET 1049  备用屏幕 ON", "mode"),
    (ESC + b"[?1049l", "DECSET 1049  备用屏幕 OFF", "mode"),
    (ESC + b"[?47h", "DECSET 47    备用屏幕 ON (旧式)", "mode"),
    (ESC + b"[?47l", "DECSET 47    备用屏幕 OFF (旧式)", "mode"),
    (ESC + b"[?2026h", "DECSET 2026  同步输出 ON", "mode"),
    (ESC + b"[?2026l", "DECSET 2026  同步输出 OFF", "mode"),
    (ESC + b"[?1000h", "DECSET 1000  鼠标点击上报", "mode"),
    (ESC + b"[?1002h", "DECSET 1002  鼠标拖动上报", "mode"),
    (ESC + b"[?1003h", "DECSET 1003  鼠标移动上报", "mode"),
    (ESC + b"[?1006h", "DECSET 1006  SGR 扩展鼠标", "mode"),
    (ESC + b"[?2004h", "DECSET 2004  括号粘贴 ON", "mode"),
    (ESC + b"[?2004l", "DECSET 2004  括号粘贴 OFF", "mode"),
    (ESC + b"[?25l", "DECSET 25    隐藏光标", "mode"),
    (ESC + b"[?25h", "DECSET 25    显示光标", "mode"),
    (ESC + b"[?7h", "DECSET 7     自动换行 ON", "mode"),
    (ESC + b"[?7l", "DECSET 7     自动换行 OFF", "mode"),
    (ESC + b"[?1h", "DECSET 1     应用光标键", "mode"),
    (ESC + b"[<", "SGR 鼠标上报报文", "mode"),
    (ESC + b"[>1u", "Kitty 键盘协议 (progressive)", "mode"),
    (ESC + b"[?u", "Kitty 键盘协议 查询", "mode"),
    # ---- 渲染框架 ----
    (b"crossterm", "crossterm  (Rust 终端库)", "fw"),
    (b"ratatui", "ratatui    (Rust TUI)", "fw"),
    (b"Ratatui", "Ratatui", "fw"),
    (b"bubbletea", "bubbletea  (Go TUI)", "fw"),
    (b"charmbracelet", "charmbracelet (Go)", "fw"),
    (b"lipgloss", "lipgloss   (Go 样式)", "fw"),
    (b"go-runewidth", "go-runewidth (Go 宽字符)", "fw"),
    (b"unicode-width", "unicode-width (Rust 宽字符)", "fw"),
    (b"react-reconciler", "react-reconciler (Ink 依赖)", "fw"),
    (b"ink", "ink (React for CLI)", "fw"),
    (b"Yoga", "Yoga 布局引擎", "fw"),
    (b"wcwidth", "wcwidth (宽字符)", "fw"),
    (b"ansi-escapes", "ansi-escapes", "fw"),
    (b"wrap-ansi", "wrap-ansi", "fw"),
    # ---- 运行时常量（辅助判断语言/打包方式）----
    (b"Bun", "Bun 运行时", "rt"),
    (b"bun.sh", "bun.sh", "rt"),
    (b"node:internal", "Node 内置模块", "rt"),
    (b"deno", "Deno", "rt"),
    (b"rustc", "rustc", "rt"),
    (b"go1.", "Go 版本串", "rt"),
    (b"goroutine", "Go runtime (goroutine)", "rt"),
]


def scan(path):
    """整文件读入后计数。

    不用 mmap：Windows 上 mmap.mmap 没有 .count()。
    分块读又会在块边界漏掉跨块的匹配，而这些特征本身就是
    转义序列，正好最容易落在边界上。文件最大几百 MB，直接读最省心。
    """
    size = os.path.getsize(path)
    with open(path, "rb") as f:
        data = f.read()
    hits = []
    for pat, name, cat in SIGS:
        c = data.count(pat)
        if c:
            hits.append((cat, name, c))
    del data
    return size, hits


def main():
    for path in sys.argv[1:]:
        if not os.path.exists(path):
            print(f"!! 不存在: {path}")
            continue
        print("=" * 72)
        print(f"FILE  {path}")
        try:
            size, hits = scan(path)
        except Exception as e:  # noqa: BLE001
            print(f"!! 扫描失败: {e}")
            continue
        print(f"SIZE  {size:,} bytes")
        if not hits:
            print("  (无命中)")
            continue
        for cat in ("fw", "rt", "mode"):
            rows = [h for h in hits if h[0] == cat]
            if not rows:
                continue
            label = {"fw": "渲染框架", "rt": "运行时", "mode": "终端模式协商"}[cat]
            print(f"  --- {label} ---")
            for _, name, c in sorted(rows, key=lambda x: -x[2]):
                print(f"    {c:>7,}  {name}")
        print()


if __name__ == "__main__":
    main()
