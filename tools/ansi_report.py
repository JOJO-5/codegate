#!/usr/bin/env python3
"""分析 ConPTY 捕获的终端字节流。

三件事：
  1. 模式时间线   —— 谁在什么时候打开了备用屏幕 / 同步输出 / 鼠标 / 括号粘贴……
  2. 能力查询清单 —— 哪些序列是【在等终端回答】的（XTVERSION、Kitty 键盘、调色板……）。
                    这一项直接决定 CodeGate 的中继能不能只做透明转发。
  3. 屏幕还原     —— 用一个极简 VT 解析器把字节流重建成最终屏幕，肉眼确认渲染正确。

用法:
  python ansi_report.py cap.bin [cap2.bin ...]
  python ansi_report.py --no-screen cap.bin
"""
from __future__ import annotations

import re
import sys
import unicodedata

ESC = 0x1B

# DEC 私有模式
MODE_NAMES = {
    1: "应用光标键(1)",
    6: "原点模式(6)",
    7: "自动换行(7)",
    12: "光标闪烁(12)",
    25: "光标可见(25)",
    47: "备用屏幕-旧(47)",
    66: "应用小键盘(66)",
    1000: "鼠标-点击(1000)",
    1002: "鼠标-拖动(1002)",
    1003: "鼠标-移动(1003)",
    1004: "焦点上报(1004)",
    1005: "鼠标-UTF8(1005)",
    1006: "鼠标-SGR扩展(1006)",
    1015: "鼠标-urxvt(1015)",
    1016: "鼠标-SGR像素(1016)",
    1047: "备用屏幕(1047)",
    1048: "保存/恢复光标(1048)",
    1049: "备用屏幕+清屏(1049)",
    2004: "括号粘贴(2004)",
    2026: "同步输出(2026)",
    2027: "字素簇处理(2027)",
    2031: "主题变更通知(2031)",
    9001: "Win32输入模式(9001)",
}

# 等待终端回答的查询
QUERY_PATTERNS = [
    (re.compile(rb"\x1b\[>\d*q"), "XTVERSION 终端名/版本查询"),
    (re.compile(rb"\x1b\[>\d*c"), "DA2 次设备属性查询"),
    (re.compile(rb"\x1b\[\d*c"), "DA1 设备属性查询"),
    (re.compile(rb"\x1b\[\?\d*u"), "Kitty 键盘协议查询"),
    (re.compile(rb"\x1b\[\d*n"), "DSR 设备状态/光标位置查询"),
    (re.compile(rb"\x1b\[\d+t"), "窗口/像素尺寸查询"),
    (re.compile(rb"\x1b\]4;\d+;\?"), "OSC4 调色板颜色查询"),
    (re.compile(rb"\x1b\](10|11|12);\?"), "OSC10/11/12 前景/背景/光标色查询"),
    (re.compile(rb"\x1b\]52;"), "OSC52 剪贴板"),
    (re.compile(rb"\x1b\]1337;"), "OSC1337 iTerm2 私有查询"),
    (re.compile(rb"\x1b\]99;"), "OSC99 Kitty 通知协议"),
    (re.compile(rb"\x1b\]66;"), "OSC66 Kitty 文本尺寸协议"),
    (re.compile(rb"\x1b\](104|110|111);"), "OSC 恢复默认色"),
]


def _wc(ch: str) -> int:
    """字符占几个终端单元格。"""
    if unicodedata.combining(ch):
        return 0
    return 2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1


class Screen:
    """极简 VT 屏幕。够用来还原 TUI 的最终画面，不追求全规范覆盖。"""

    def __init__(self, cols: int, rows: int):
        self.cols, self.rows = cols, rows
        self.grid = [[" "] * cols for _ in range(rows)]
        self.x = self.y = 0
        self.saved = (0, 0)
        self.modes: set[int] = set()
        self.timeline: list[tuple[int, str, int]] = []
        self.queries: list[tuple[int, str]] = []
        self.alt = None
        self.autowrap = True

    # ---------- 屏幕操作 ----------
    def _clear(self) -> None:
        self.grid = [[" "] * self.cols for _ in range(self.rows)]

    def put(self, ch: str) -> None:
        w = _wc(ch)
        if w == 0:
            return
        if self.x + w > self.cols:
            if not self.autowrap:
                self.x = self.cols - w
            else:
                self.x = 0
                self.y += 1
                if self.y >= self.rows:
                    self.grid.pop(0)
                    self.grid.append([" "] * self.cols)
                    self.y = self.rows - 1
        self.grid[self.y][self.x] = ch
        if w == 2 and self.x + 1 < self.cols:
            self.grid[self.y][self.x + 1] = ""   # 宽字符占位
        self.x += w

    def _clamp(self) -> None:
        self.x = max(0, min(self.cols - 1, self.x))
        self.y = max(0, min(self.rows - 1, self.y))

    def erase_line(self, mode: int) -> None:
        if mode == 0:
            for i in range(self.x, self.cols):
                self.grid[self.y][i] = " "
        elif mode == 1:
            for i in range(0, self.x + 1):
                self.grid[self.y][i] = " "
        else:
            self.grid[self.y] = [" "] * self.cols

    def erase_display(self, mode: int) -> None:
        if mode == 0:
            self.erase_line(0)
            for r in range(self.y + 1, self.rows):
                self.grid[r] = [" "] * self.cols
        elif mode == 1:
            self.erase_line(1)
            for r in range(0, self.y):
                self.grid[r] = [" "] * self.cols
        else:
            self._clear()

    def scroll_up(self, n: int) -> None:
        for _ in range(max(1, n)):
            if self.grid:
                self.grid.pop(0)
                self.grid.append([" "] * self.cols)

    def set_mode(self, mode: int, on: bool, offset: int) -> None:
        if on:
            self.modes.add(mode)
        else:
            self.modes.discard(mode)
        if mode == 7:
            self.autowrap = on
        if mode == 1049:
            if on:
                self.alt = self.grid
                self._clear()
                self.x = self.y = 0
            elif self.alt is not None:
                self.grid = self.alt
                self.alt = None
        if mode == 1048:
            if on:
                self.saved = (self.x, self.y)
            else:
                self.x, self.y = self.saved
        self.timeline.append((offset, "SET" if on else "RST", mode))

    # ---------- 主解析循环 ----------
    def feed(self, data: bytes) -> None:
        i, n = 0, len(data)
        while i < n:
            b = data[i]

            if b == ESC:
                i = self._escape(data, i)
                continue

            if b == 0x0D:
                self.x = 0
                i += 1
                continue
            if b == 0x0A or b == 0x0B or b == 0x0C:
                self.y += 1
                if self.y >= self.rows:
                    self.scroll_up(1)
                    self.y = self.rows - 1
                i += 1
                continue
            if b == 0x08:
                self.x = max(0, self.x - 1)
                i += 1
                continue
            if b == 0x09:
                self.x = min(self.cols - 1, (self.x // 8 + 1) * 8)
                i += 1
                continue
            if b < 0x20 or b == 0x7F:
                i += 1
                continue

            # 可打印字节：攒一段，按 UTF-8 解码（自动处理跨帧截断的多字节字符）
            j = i
            while j < n and data[j] >= 0x20 and data[j] != 0x7F and data[j] != ESC:
                j += 1
            for ch in data[i:j].decode("utf-8", "replace"):
                if ch != "\ufffd":
                    self.put(ch)
            i = j

    def _escape(self, data: bytes, i: int) -> int:
        n = len(data)
        if i + 1 >= n:
            return n
        c = data[i + 1]

        # OSC：ESC ] ... BEL 或 ST
        if c == 0x5D:
            j = i + 2
            while j < n and data[j] != 0x07:
                if data[j] == ESC and j + 1 < n and data[j + 1] == 0x5C:
                    j += 1
                    break
                j += 1
            seq = data[i : min(j + 1, n)]
            for pat, label in QUERY_PATTERNS:
                if pat.match(seq):
                    self.queries.append((i, label))
                    break
            return min(j + 1, n)

        # CSI：ESC [ 参数 中间 终结
        if c == 0x5B:
            j = i + 2
            while j < n and 0x20 <= data[j] <= 0x3F:
                j += 1
            while j < n and 0x20 <= data[j] <= 0x2F:
                j += 1
            if j >= n:
                return n
            final = data[j]
            body = data[i + 2 : j].decode("ascii", "replace")
            self._csi(body, chr(final), i, data[i : j + 1])
            return j + 1

        # 两字节转义
        if c == 0x37:      # ESC 7 保存光标
            self.saved = (self.x, self.y)
        elif c == 0x38:    # ESC 8 恢复光标
            self.x, self.y = self.saved
        elif c == 0x44:    # ESC D 索引
            self.y += 1
            self._clamp()
        elif c == 0x4D:    # ESC M 反索引
            self.y -= 1
            self._clamp()
        return i + 2

    def _csi(self, body: str, final: str, off: int, raw: bytes) -> None:
        private = body[:1] in ("?", ">", "<", "=")
        params = body[1:] if private else body
        nums = []
        for p in params.split(";"):
            try:
                nums.append(int(p))
            except ValueError:
                nums.append(0)
        if not nums:
            nums = [0]
        p0 = nums[0]

        if final == "h" and private:
            for m in nums:
                self.set_mode(m, True, off)
            return
        if final == "l" and private:
            for m in nums:
                self.set_mode(m, False, off)
            return

        if final in ("H", "f"):
            row = (nums[0] or 1) - 1
            col = (nums[1] if len(nums) > 1 else 1) - 1
            self.y, self.x = row, col
            self._clamp()
        elif final == "A":
            self.y -= p0 or 1
            self._clamp()
        elif final == "B":
            self.y += p0 or 1
            self._clamp()
        elif final == "C":
            self.x += p0 or 1
            self._clamp()
        elif final == "D":
            self.x -= p0 or 1
            self._clamp()
        elif final == "G":
            self.x = (p0 or 1) - 1
            self._clamp()
        elif final == "d":
            self.y = (p0 or 1) - 1
            self._clamp()
        elif final == "J":
            self.erase_display(p0)
        elif final == "K":
            self.erase_line(p0)
        elif final == "S":
            self.scroll_up(p0 or 1)
        elif final == "m":
            pass                      # 颜色/样式：屏幕还原不需要
        elif final in ("s",) :
            self.saved = (self.x, self.y)
        elif final in ("u",):
            self.x, self.y = self.saved
        # 其余（r 滚动区、t 查询、c 属性……）忽略

        # 查询类：CSI 里也要认
        for pat, label in QUERY_PATTERNS:
            if pat.match(raw):
                self.queries.append((off, label))
                break

    def dump(self) -> str:
        lines = []
        for row in self.grid:
            lines.append("".join(ch if ch else " " for ch in row).rstrip())
        while lines and not lines[-1]:
            lines.pop()
        return "\n".join(lines)


def analyze(path: str, show_screen: bool = True, cols: int = 100, rows: int = 30) -> None:
    data = open(path, "rb").read()
    sc = Screen(cols, rows)
    sc.feed(data)

    print("=" * 74)
    print(f"FILE   {path}")
    print(f"SIZE   {len(data):,} bytes    ESC x{data.count(bytes([ESC])):,}")

    print("\n--- 模式时间线（按出现顺序，去重） ---")
    seen = set()
    for off, act, mode in sc.timeline:
        key = (act, mode)
        if key in seen:
            continue
        seen.add(key)
        name = MODE_NAMES.get(mode, f"未知模式({mode})")
        print(f"  @{off:>7}  {act}  {name}")
    if not sc.timeline:
        print("  (无)")

    print("\n--- 结束时仍生效的模式 ---")
    if sc.modes:
        for m in sorted(sc.modes):
            print(f"  {MODE_NAMES.get(m, f'未知模式({m})')}")
    else:
        print("  (无)")

    print("\n--- 等待终端回答的能力查询 ---")
    if sc.queries:
        agg: dict[str, list[int]] = {}
        for off, label in sc.queries:
            agg.setdefault(label, []).append(off)
        for label, offs in agg.items():
            print(f"  x{len(offs):<3} {label}   @{offs[0]}")
    else:
        print("  (无)")

    if show_screen:
        print("\n--- 还原后的最终屏幕 ---")
        print("+" + "-" * cols + "+")
        for line in sc.dump().split("\n"):
            print("|" + line.ljust(cols) + "|")
        print("+" + "-" * cols + "+")
    print()


def main() -> int:
    args = sys.argv[1:]
    show = True
    if "--no-screen" in args:
        show = False
        args.remove("--no-screen")
    if not args:
        print(__doc__)
        return 1
    for p in args:
        try:
            analyze(p, show)
        except OSError as e:
            print(f"!! {p}: {e}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
