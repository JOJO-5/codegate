#!/usr/bin/env python3
"""ConPTY 探针 —— 在 Windows 上用真正的 ConPTY 启动一个 CLI，捕获它的原始输出字节。

为什么写这个（而不是用 winpty / 或先装 Go）：
  * CodeGate 的 Agent 最终就是要用 ConPTY。用 Python ctypes 直接调同一套 API，
    拿到的字节流与未来 Go 实现拿到的**是同一份东西**，可以直接当作 R1 的判据。
  * 不依赖任何第三方库（pywinpty / go-pty 都不需要），立刻可跑。

它回答的问题是：ConPTY 到底是不是透明字节管道？目标 TUI 会向终端请求哪些能力？

用法:
  python conpty_probe.py -- cmd.exe /c "echo hi"
  python conpty_probe.py --seconds 6 --out cap.bin -- codex.exe
  python conpty_probe.py --seconds 6 --resize 100x30 --input "abc" --out cap.bin -- claude.exe

输出：cap.bin（原始字节）+ 终端摘要。用 ansi_report.py 做详细分析。
"""
from __future__ import annotations

import argparse
import ctypes
import os
import subprocess
import sys
import threading
import time
from ctypes import wintypes

kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)

PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE = 0x00020016
EXTENDED_STARTUPINFO_PRESENT = 0x00080000
CREATE_UNICODE_ENVIRONMENT = 0x00000400
INFINITE = 0xFFFFFFFF
ERROR_BROKEN_PIPE = 109

# GetStdHandle/SetStdHandle 用的是无符号 DWORD，直接写 -10/-11 会被 ctypes 拒绝
STD_INPUT_HANDLE = 0xFFFFFFF6   # -10
STD_OUTPUT_HANDLE = 0xFFFFFFF5  # -11
STD_ERROR_HANDLE = 0xFFFFFFF4   # -12


class COORD(ctypes.Structure):
    _fields_ = [("X", ctypes.c_short), ("Y", ctypes.c_short)]


class STARTUPINFOW(ctypes.Structure):
    _fields_ = [
        ("cb", wintypes.DWORD),
        ("lpReserved", wintypes.LPWSTR),
        ("lpDesktop", wintypes.LPWSTR),
        ("lpTitle", wintypes.LPWSTR),
        ("dwX", wintypes.DWORD),
        ("dwY", wintypes.DWORD),
        ("dwXSize", wintypes.DWORD),
        ("dwYSize", wintypes.DWORD),
        ("dwXCountChars", wintypes.DWORD),
        ("dwYCountChars", wintypes.DWORD),
        ("dwFillAttribute", wintypes.DWORD),
        ("dwFlags", wintypes.DWORD),
        ("wShowWindow", wintypes.WORD),
        ("cbReserved2", wintypes.WORD),
        ("lpReserved2", ctypes.POINTER(ctypes.c_byte)),
        ("hStdInput", wintypes.HANDLE),
        ("hStdOutput", wintypes.HANDLE),
        ("hStdError", wintypes.HANDLE),
    ]


class STARTUPINFOEXW(ctypes.Structure):
    _fields_ = [("StartupInfo", STARTUPINFOW), ("lpAttributeList", ctypes.c_void_p)]


class PROCESS_INFORMATION(ctypes.Structure):
    _fields_ = [
        ("hProcess", wintypes.HANDLE),
        ("hThread", wintypes.HANDLE),
        ("dwProcessId", wintypes.DWORD),
        ("dwThreadId", wintypes.DWORD),
    ]


def _declare() -> None:
    k = kernel32
    k.CreatePipe.argtypes = [
        ctypes.POINTER(wintypes.HANDLE),
        ctypes.POINTER(wintypes.HANDLE),
        ctypes.c_void_p,
        wintypes.DWORD,
    ]
    k.CreatePipe.restype = wintypes.BOOL

    k.CreatePseudoConsole.argtypes = [
        COORD,
        wintypes.HANDLE,
        wintypes.HANDLE,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.HANDLE),
    ]
    k.CreatePseudoConsole.restype = ctypes.c_long

    k.ResizePseudoConsole.argtypes = [wintypes.HANDLE, COORD]
    k.ResizePseudoConsole.restype = ctypes.c_long

    k.ClosePseudoConsole.argtypes = [wintypes.HANDLE]
    k.ClosePseudoConsole.restype = None

    k.InitializeProcThreadAttributeList.argtypes = [
        ctypes.c_void_p,
        wintypes.DWORD,
        wintypes.DWORD,
        ctypes.POINTER(ctypes.c_size_t),
    ]
    k.InitializeProcThreadAttributeList.restype = wintypes.BOOL

    k.UpdateProcThreadAttribute.argtypes = [
        ctypes.c_void_p,
        wintypes.DWORD,
        ctypes.c_size_t,
        ctypes.c_void_p,
        ctypes.c_size_t,
        ctypes.c_void_p,
        ctypes.c_void_p,
    ]
    k.UpdateProcThreadAttribute.restype = wintypes.BOOL

    k.DeleteProcThreadAttributeList.argtypes = [ctypes.c_void_p]

    k.CreateProcessW.argtypes = [
        wintypes.LPCWSTR,
        wintypes.LPWSTR,
        ctypes.c_void_p,
        ctypes.c_void_p,
        wintypes.BOOL,
        wintypes.DWORD,
        ctypes.c_void_p,
        wintypes.LPCWSTR,
        ctypes.c_void_p,
        ctypes.POINTER(PROCESS_INFORMATION),
    ]
    k.CreateProcessW.restype = wintypes.BOOL

    k.ReadFile.argtypes = [
        wintypes.HANDLE,
        ctypes.c_void_p,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.DWORD),
        ctypes.c_void_p,
    ]
    k.ReadFile.restype = wintypes.BOOL

    k.WriteFile.argtypes = [
        wintypes.HANDLE,
        ctypes.c_void_p,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.DWORD),
        ctypes.c_void_p,
    ]
    k.WriteFile.restype = wintypes.BOOL

    k.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
    k.WaitForSingleObject.restype = wintypes.DWORD

    k.GetExitCodeProcess.argtypes = [wintypes.HANDLE, ctypes.POINTER(wintypes.DWORD)]
    k.GetExitCodeProcess.restype = wintypes.BOOL

    k.CloseHandle.argtypes = [wintypes.HANDLE]
    k.CloseHandle.restype = wintypes.BOOL

    k.GetConsoleCP.restype = wintypes.UINT
    k.SetConsoleOutputCP.argtypes = [wintypes.UINT]

    k.GetStdHandle.argtypes = [wintypes.DWORD]
    k.GetStdHandle.restype = wintypes.HANDLE

    k.SetStdHandle.argtypes = [wintypes.DWORD, wintypes.HANDLE]
    k.SetStdHandle.restype = wintypes.BOOL


_declare()


def check_conpty_available() -> tuple[bool, str]:
    """ConPTY 需要 Win10 1809 (17763)+。这里直接看导出符号在不在。"""
    try:
        addr = ctypes.cast(kernel32.CreatePseudoConsole, ctypes.c_void_p).value
    except AttributeError:
        return False, "kernel32!CreatePseudoConsole 不存在"
    if not addr:
        return False, "kernel32!CreatePseudoConsole 存在但地址为空"
    return True, f"CreatePseudoConsole @ 0x{addr:x}"


class ConPTY:
    """最小可用的 ConPTY 封装：创建 / 写输入 / 读输出 / resize / 关闭。"""

    def __init__(self, cols: int = 120, rows: int = 30):
        self.cols, self.rows = cols, rows
        self.hpcon = wintypes.HANDLE()
        self.h_in_w = wintypes.HANDLE()   # 我们写输入用
        self.h_out_r = wintypes.HANDLE()  # 我们读输出用
        self.pi = PROCESS_INFORMATION()
        self._attr_buf = None
        self._attr_size = ctypes.c_size_t(0)
        self._closed = False
        self.output = bytearray()
        self._read_err = None
        self._read_done = threading.Event()

    # ---------- 生命周期 ----------
    def start(self, cmdline: str, cwd: str | None = None, null_parent_std: bool = True) -> None:
        k = kernel32
        in_r, in_w = wintypes.HANDLE(), wintypes.HANDLE()
        out_r, out_w = wintypes.HANDLE(), wintypes.HANDLE()
        if not k.CreatePipe(ctypes.byref(in_r), ctypes.byref(in_w), None, 0):
            raise OSError(ctypes.get_last_error(), "CreatePipe(input) 失败")
        if not k.CreatePipe(ctypes.byref(out_r), ctypes.byref(out_w), None, 0):
            raise OSError(ctypes.get_last_error(), "CreatePipe(output) 失败")

        hr = k.CreatePseudoConsole(
            COORD(self.cols, self.rows), in_r, out_w, 0, ctypes.byref(self.hpcon)
        )
        if hr != 0:
            raise OSError(f"CreatePseudoConsole 失败, HRESULT=0x{hr & 0xFFFFFFFF:08x}")

        # ConPTY 已 dup 了这两端，我们手上的副本必须立刻关掉，
        # 否则读端永远等不到 EOF（经典坑）。
        k.CloseHandle(in_r)
        k.CloseHandle(out_w)
        self.h_in_w = in_w
        self.h_out_r = out_r

        # 属性列表：把 HPCON 挂到 PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE 上
        k.InitializeProcThreadAttributeList(None, 1, 0, ctypes.byref(self._attr_size))
        self._attr_buf = ctypes.create_string_buffer(self._attr_size.value)
        if not k.InitializeProcThreadAttributeList(
            self._attr_buf, 1, 0, ctypes.byref(self._attr_size)
        ):
            raise OSError(ctypes.get_last_error(), "InitializeProcThreadAttributeList 失败")

        # 注意：lpValue 直接传 HPCON 本身，不是它的指针
        if not k.UpdateProcThreadAttribute(
            self._attr_buf,
            0,
            PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
            ctypes.c_void_p(self.hpcon.value),
            ctypes.sizeof(wintypes.HANDLE),
            None,
            None,
        ):
            raise OSError(ctypes.get_last_error(), "UpdateProcThreadAttribute 失败")

        si_ex = STARTUPINFOEXW()
        si_ex.StartupInfo.cb = ctypes.sizeof(STARTUPINFOEXW)
        si_ex.lpAttributeList = ctypes.cast(self._attr_buf, ctypes.c_void_p)

        buf = ctypes.create_unicode_buffer(cmdline)

        # ★ 关键：ConPTY 子进程的 std 句柄是从父进程【复制】的，不是从伪控制台派生的。
        #   父进程的 stdout 只要被重定向（管道 / 文件 / 服务环境），子进程就会跟着
        #   指向那个句柄，输出完全绕开伪控制台 —— 表现是 ConPTY 只吐出自己的初始化
        #   序列（\x1b[?9001h\x1b[?1004h），拿不到任何 CLI 输出。
        #   实测：置 NULL 后子进程 stdout 才真正变成伪控制台（BUF=cols x rows）。
        #   Agent 将来作为 Windows Service 运行必然无控制台，这段逻辑不可省。
        saved: tuple = ()
        if null_parent_std:
            for slot in (STD_INPUT_HANDLE, STD_OUTPUT_HANDLE, STD_ERROR_HANDLE):
                saved += (k.GetStdHandle(slot),)
                k.SetStdHandle(slot, None)

        try:
            ok = k.CreateProcessW(
                None,
                buf,
                None,
                None,
                False,
                EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT,
                None,
                cwd,
                ctypes.byref(si_ex),
                ctypes.byref(self.pi),
            )
            err = ctypes.get_last_error()
        finally:
            if null_parent_std:
                for slot, h in zip(
                    (STD_INPUT_HANDLE, STD_OUTPUT_HANDLE, STD_ERROR_HANDLE), saved
                ):
                    k.SetStdHandle(slot, h)

        if not ok:
            self.close()
            raise OSError(err, f"CreateProcessW 失败: {cmdline}")

        # 读线程：唯一的读者
        self._t = threading.Thread(target=self._read_loop, daemon=True)
        self._t.start()

    def _read_loop(self) -> None:
        k = kernel32
        buf = ctypes.create_string_buffer(1 << 16)
        n = wintypes.DWORD(0)
        while True:
            ok = k.ReadFile(self.h_out_r, buf, len(buf), ctypes.byref(n), None)
            if not ok:
                err = ctypes.get_last_error()
                if err != ERROR_BROKEN_PIPE:
                    self._read_err = err
                break
            if n.value == 0:
                break
            self.output.extend(buf.raw[: n.value])
        self._read_done.set()

    # ---------- 操作 ----------
    def write(self, data: bytes) -> int:
        n = wintypes.DWORD(0)
        buf = ctypes.create_string_buffer(data, len(data))
        kernel32.WriteFile(self.h_in_w, buf, len(data), ctypes.byref(n), None)
        return n.value

    def resize(self, cols: int, rows: int) -> None:
        self.cols, self.rows = cols, rows
        kernel32.ResizePseudoConsole(self.hpcon, COORD(cols, rows))

    def wait(self, timeout_ms: int = INFINITE) -> int:
        kernel32.WaitForSingleObject(self.pi.hProcess, timeout_ms)
        code = wintypes.DWORD(0)
        kernel32.GetExitCodeProcess(self.pi.hProcess, ctypes.byref(code))
        return code.value

    def kill_tree(self) -> None:
        """连同子进程一起杀。TUI 常会 fork，只杀父进程会留孤儿。"""
        if self.pi.dwProcessId:
            subprocess.run(
                ["taskkill", "/F", "/T", "/PID", str(self.pi.dwProcessId)],
                capture_output=True,
                creationflags=0x08000000,  # CREATE_NO_WINDOW
            )

    def drain(self, timeout: float = 2.0) -> None:
        self._read_done.wait(timeout)

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        k = kernel32
        # 顺序要紧：先关输入（让子进程看到 EOF），再关伪控制台，最后关句柄
        if self.h_in_w.value:
            k.CloseHandle(self.h_in_w)
        if self.hpcon.value:
            k.ClosePseudoConsole(self.hpcon)
        if self.h_out_r.value:
            k.CloseHandle(self.h_out_r)
        # 注意：PROCESS_INFORMATION 的字段是 c_void_p，ctypes 取出来是 int 而不是对象，
        # 所以这里只能判真假，不能写 .value
        if self.pi.hThread:
            k.CloseHandle(self.pi.hThread)
        if self.pi.hProcess:
            k.CloseHandle(self.pi.hProcess)
        if self._attr_buf is not None:
            k.DeleteProcThreadAttributeList(self._attr_buf)
            self._attr_buf = None


def main() -> int:
    ap = argparse.ArgumentParser(description="ConPTY 探针")
    ap.add_argument("--seconds", type=float, default=5.0, help="捕获时长")
    ap.add_argument("--cols", type=int, default=120)
    ap.add_argument("--rows", type=int, default=30)
    ap.add_argument("--out", default="cap.bin", help="原始字节输出文件")
    ap.add_argument("--cwd", default=None, help="子进程工作目录")
    ap.add_argument("--input", default=None, help="启动 1 秒后写入的文本（\\r 表示回车）")
    ap.add_argument("--resize", default=None, help="启动 2 秒后 resize，形如 100x40")
    ap.add_argument("--no-kill", action="store_true", help="不主动杀进程（自己会退出的命令用）")
    ap.add_argument(
        "--no-null-std",
        action="store_true",
        help="不把父进程 std 句柄置 NULL（用于复现输出绕开伪控制台的现象）",
    )
    ap.add_argument("cmd", nargs=argparse.REMAINDER, help="-- 之后是要跑的命令")
    args = ap.parse_args()

    cmd = [c for c in args.cmd if c != "--"]
    if not cmd:
        ap.error("缺少要执行的命令，写成: -- cmd.exe /c \"echo hi\"")

    ok, info = check_conpty_available()
    print(f"[ConPTY] {info}")
    if not ok:
        return 2

    # 用 list2cmdline 拼命令行：它会正确处理带空格的参数
    cmdline = subprocess.list2cmdline(cmd)
    print(f"[exec ] {cmdline}")
    print(f"[size ] {args.cols}x{args.rows}   捕获 {args.seconds}s")

    p = ConPTY(args.cols, args.rows)
    t0 = time.time()
    try:
        p.start(cmdline, cwd=args.cwd, null_parent_std=not args.no_null_std)
    except OSError as e:
        print(f"[FAIL ] {e}")
        return 3
    print(f"[pid  ] {p.pi.dwProcessId}")

    if args.input:
        time.sleep(1.0)
        p.write(args.input.replace("\\r", "\r").encode("utf-8"))

    if args.resize:
        time.sleep(1.0)
        c, r = args.resize.lower().split("x")
        p.resize(int(c), int(r))
        print(f"[rsize] -> {c}x{r}")

    deadline = t0 + args.seconds
    exited = None
    while time.time() < deadline:
        rc = p.wait(200)
        if rc != 259:  # STILL_ACTIVE
            exited = rc
            break
    if exited is not None:
        print(f"[exit ] 进程自行退出, code={exited}")
    else:
        print("[exit ] 仍在运行，即将终止")

    if not args.no_kill:
        p.kill_tree()
    time.sleep(0.4)
    p.drain(2.0)

    data = bytes(p.output)
    with open(args.out, "wb") as f:
        f.write(data)

    print(f"[cap  ] {len(data):,} bytes -> {os.path.abspath(args.out)}")
    if p._read_err:
        print(f"[warn ] 读管道错误码 {p._read_err}")
    esc = bytes([27])
    print(f"[esc  ] ESC 出现 {data.count(esc):,} 次")

    p.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
