//go:build windows

package terminal

// 本文件是 CodeGate 在 Windows 上的 PTY 实现，基于 ConPTY（伪控制台）。
//
// 设计文档：docs/PHASE0-ARCHITECTURE.md §6
// 实测报告：docs/PHASE0-R1-CONPTY-FINDINGS.md（本文件的每个关键决策都有实测支撑）
//
// # 为什么不直接用现成的 ConPTY 封装库
//
// 因为下面这个坑是"能不能用"级别的，而常见的封装（go-winpty、conpty 等）
// 都没有处理它：
//
//	父进程的 std 句柄会被子进程复制过去。父进程 stdout 一旦被重定向
//	（管道 / 文件 / Windows Service 环境），子进程的 stdout 就跟着指向
//	那个句柄，输出完全绕开伪控制台 —— ConPTY 只吐出它自己的初始化序列
//	（实测：16 字节），子进程的真实输出一个字节都拿不到。
//
// Agent 将来必然以 Windows Service 方式运行（没有控制台），所以必须自己
// 精确控制 CreateProcessW 的每一个参数。见 spawn 的注释。
//
// # 资源与关闭顺序
//
// 这是本文件最容易写错的地方，先把不变式写清楚：
//
//	CreatePipe(in)   → hInR(伪控制台持有) + hInW(我们持有)
//	CreatePipe(out)  → hOutR(我们持有)     + hOutW(伪控制台持有)
//	CreatePseudoConsole(hInR, hOutW) → hpcon，之后立刻关掉 hInR / hOutW
//
//	Close 的顺序必须是：
//	  1. 关 hInW            —— 让子进程看到 stdin EOF，能优雅退出就退出
//	  2. ClosePseudoConsole —— 终止附属进程，并关闭它内部的管道写端
//	  3. 关 hOutR           —— 此时阻塞中的 ReadFile 才会返回
//
// ★ 第 2 步是第 3 步的前提。反过来（先关 hOutR）是未定义行为：
//   Windows 不允许关闭一个还有线程阻塞在上面的同步管道句柄。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// debugf 输出 ConPTY 的中间状态到 stderr，用 CODEGATE_TERMINAL_DEBUG=1 开启。
//
// 为什么需要它：ConPTY 的失败模式极其安静 —— 进程建起来了、
// CreateProcessW 返回成功、没有任何错误码，就是读不到输出。
// 没有中间状态的日志，这种问题基本只能靠猜。
var debugf = func(string, ...any) {}

func init() {
	if os.Getenv("CODEGATE_TERMINAL_DEBUG") == "1" {
		debugf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[terminal] "+format+"\n", args...)
		}
	}
}

// ---------------------------------------------------------------------------
// Win32 绑定
// ---------------------------------------------------------------------------

var (
	// kernel32.dll 是每个 Windows 进程的基础模块，在进程初始化时就已加载，
	// 因此不存在 DLL 搜索路径劫持的问题（LoadLibrary 会命中已加载模块）。
	// 这也是我们不需要 LoadLibraryExW + LOAD_LIBRARY_SEARCH_SYSTEM32 的原因。
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreatePseudoConsole           = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole           = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole            = kernel32.NewProc("ClosePseudoConsole")
	procCreateProcessW                = kernel32.NewProc("CreateProcessW")
	procInitializeProcThreadAttrList  = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute     = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList = kernel32.NewProc("DeleteProcThreadAttributeList")
	// Go 的 syscall 包只导出了 GetStdHandle，没有 SetStdHandle（它只在
	// syscall 包内部用来初始化 std 句柄）。而我们需要它来做 spawn 里
	// 那个"临时置空"的关键操作，所以自己绑一个。
	procSetStdHandle = kernel32.NewProc("SetStdHandle")

	procTerminateProcess    = kernel32.NewProc("TerminateProcess")
	procGetExitCodeProcess  = kernel32.NewProc("GetExitCodeProcess")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procCancelIoEx          = kernel32.NewProc("CancelIoEx")
)

// errConPTYUnavailable 表示系统太老，没有 ConPTY。
//
// ConPTY 从 Windows 10 1809 (build 17763) / Server 2019 起可用。
// 在这之前只能退回 winpty 那套（注入 conhost），本版本不支持。
var errConPTYUnavailable = errors.New(
	"terminal: 当前系统不支持 ConPTY（需要 Windows 10 1809 / Server 2019 或更高版本）")

const (
	// extendedStartupInfoPresent 告诉 CreateProcessW：第 9 个参数指向的是
	// STARTUPINFOEXW 而不是 STARTUPINFOW。没有它，属性列表会被忽略，
	// 伪控制台根本绑不到子进程上。
	extendedStartupInfoPresent = 0x00080000

	// createUnicodeEnvironment 表示环境块是 UTF-16 编码。
	createUnicodeEnvironment = 0x00000400

	// procThreadAttributePseudoConsole 是把伪控制台关联给子进程的属性 ID。
	procThreadAttributePseudoConsole = 0x00020016

	// infinite 是 WaitForSingleObject 的无限等待。
	infinite = 0xFFFFFFFF

	// waitFailed 是 WaitForSingleObject 的失败返回值。
	waitFailed = 0xFFFFFFFF

	// waitTimeout 是 WaitForSingleObject 超时的返回值。
	waitTimeout = 0x00000102

	// waitObject0 表示等待对象已触发（对进程句柄而言 = 进程已退出）。
	waitObject0 = 0x00000000

	// closeWaitTimeoutMS 是 Close 等待子进程退出的上限（毫秒）。
	//
	// 不能无限等：万一某个进程拒绝退出，Close 会挂住整个优雅关闭流程。
	// 也不能太短：实测 5 秒足够覆盖"进程已终止但内核还在回收句柄"的窗口。
	closeWaitTimeoutMS = 5000
)

// coord 对应 Win32 的 COORD（两个 SHORT）。
//
// 注意它只在"结构体字段"或"指针参数"的场合使用（例如
// GetConsoleScreenBufferInfo 里的 Window / Size）。凡是 Win32 签名写成
// 裸 `COORD size` 的（CreatePseudoConsole、ResizePseudoConsole），
// 都必须用 packCoord 打包成整数按值传 —— 见 packCoord 的注释。
type coord struct {
	X int16
	Y int16
}

// packCoord 把列/行打包成 COORD 按值传递时的位模式。
//
// ★ 这里踩过一个很隐蔽的坑，值得写清楚：
//
//	COORD 是 4 字节的结构体。在 x64 调用约定下，小于等于 8 字节的结构体
//	【按值】传递 —— 直接放进参数寄存器，而不是像大结构体那样传指针。
//	但 ctypes / Go 的 unsafe.Pointer 写法很容易让人顺手传 &coord，
//	编译不报错、调用也不报错，Windows 只是把指针值的低 32 位
//	当成 COORD 的内容用，于是伪控制台被创建成一个荒谬的尺寸。
//
//	实测表现极具误导性：CreatePseudoConsole 返回成功、进程正常启动、
//	CreateProcessW 返回成功，但输出管道立刻 EOF，一个字节都读不到 ——
//	看起来像"句柄继承"问题，实际是参数传递方式错了。
//
// COORD 的内存布局是小端：X 在低 16 位，Y 在高 16 位，
// 所以按 32 位整数加载正好是 X | Y<<16。
func packCoord(cols, rows uint16) uintptr {
	return uintptr(uint32(cols) | uint32(rows)<<16)
}

// startupInfoExW 对应 Win32 的 STARTUPINFOEXW。
//
// 直接复用 syscall.StartupInfo —— 它已经是 STARTUPINFOW 的正确映射
// （字段顺序、宽度、填充都与 C 结构体一致，64 位下 104 字节）。
type startupInfoExW struct {
	syscall.StartupInfo
	AttributeList unsafe.Pointer
}

// 编译期布局断言：64 位下 STARTUPINFOEXW = STARTUPINFOW(104) + lpAttributeList(8) = 112。
//
// 写成数组下标：等于 112 时下标为 0（合法），否则是越界常量索引 → 编译失败。
// 这个断言是必要的 —— 结构体布局错了不会报运行时错误，只会让
// CreateProcessW 读到垃圾指针然后崩溃，排查成本极高。
var _ = [1]struct{}{}[unsafe.Sizeof(startupInfoExW{})-112]

// ---------------------------------------------------------------------------
// 构造
// ---------------------------------------------------------------------------

// Available 检查本机是否支持 ConPTY。
//
// 给 doctor 用：与其等用户点了"新建会话"才报错，不如启动时就告诉他。
func Available() error {
	if err := procCreatePseudoConsole.Find(); err != nil {
		return errConPTYUnavailable
	}
	return nil
}

// New 创建一个尚未启动的 Windows ConPTY 终端。
//
// 返回的 Terminal 需要再调 Start 才会真正起进程。这样拆开是为了让
// StartConfig（命令、目录、环境、尺寸）能从网络请求里来，
// 而终端对象的创建时机由调用方决定。
func New() Terminal { return &conPTY{} }

// ---------------------------------------------------------------------------
// conPTY
// ---------------------------------------------------------------------------

// conPTY 是 Terminal 的 Windows 实现。
//
// 锁的划分：
//
//	mu    —— 保护控制平面状态（句柄归属、started/closed、尺寸、进程信息）
//	inMu  —— 保护输入句柄 hInW
//	outMu —— 保护输出句柄 hOutR
//
// ★ 输入和输出必须是【两个独立的锁】，这不是并发优化，是死锁防范。
//
//	Read 会长时间持有 outMu 的读锁阻塞在 ReadFile 上，而 Close 需要拿到
//	写锁才能安全地关句柄。如果两者共用一个锁，Close 就会一直等 Read ——
//	而 Read 又要等 Close 关掉伪控制台才会返回，直接互等死锁。
//
//	实测表现：集成测试跑满 3 分钟超时，goroutine dump 里
//	Close 卡在 RWMutex.Lock、Read 卡在 syscall.readFile。
//
//	拆开之后，Close 关输入端（拿 inMu 写锁）不会碰到 Read 持有的
//	outMu 读锁，于是能顺利走到"关闭伪控制台"那一步，把 Read 唤醒，
//	再由 outMu 写锁安全地关掉输出句柄。
type conPTY struct {
	mu    sync.Mutex
	inMu  sync.RWMutex
	outMu sync.RWMutex

	hpcon syscall.Handle
	hInW  syscall.Handle // 我们写 → 子进程读
	hOutR syscall.Handle // 子进程写 → 我们读
	pi    syscall.ProcessInformation

	cols, rows uint16
	started    bool
	closed     bool

	// waitOnce 让 Wait 幂等：多个调用者都拿到同一份结果。
	waitOnce sync.Once
	exit     ExitResult

	// reapOnce 让进程/线程句柄只被关闭一次（Wait 和 Close 都可能触发）。
	reapOnce sync.Once
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

// Start 创建伪控制台并启动子进程。
func (p *conPTY) Start(ctx context.Context, cfg StartConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started {
		return ErrAlreadyStarted
	}
	if p.closed {
		return ErrClosed
	}
	if err := Available(); err != nil {
		return err
	}
	if cfg.Command == "" {
		return errors.New("terminal: StartConfig.Command 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	cols, rows := cfg.Cols, cfg.Rows
	if cols == 0 {
		cols = DefaultCols
	}
	if rows == 0 {
		rows = DefaultRows
	}

	// ---- 1) 两根管道 ----
	//
	// 句柄不可继承（sa = nil）：子进程是通过属性列表拿到伪控制台的，
	// 不需要继承我们的任何句柄。可继承反而会把管道句柄泄漏进子进程，
	// 导致"子进程退出后管道写端仍然打开、读循环永远不返回 EOF"。
	var hInR, hInW, hOutR, hOutW syscall.Handle
	if err := syscall.CreatePipe(&hInR, &hInW, nil, 0); err != nil {
		return fmt.Errorf("terminal: 创建输入管道失败: %w", err)
	}
	if err := syscall.CreatePipe(&hOutR, &hOutW, nil, 0); err != nil {
		syscall.CloseHandle(hInR)
		syscall.CloseHandle(hInW)
		return fmt.Errorf("terminal: 创建输出管道失败: %w", err)
	}

	// ---- 2) 伪控制台 ----
	hpcon, err := createPseudoConsole(cols, rows, hInR, hOutW)
	// 无论成功与否，这两端都已交给伪控制台（或已无用），立刻关掉我们手里的副本。
	syscall.CloseHandle(hInR)
	syscall.CloseHandle(hOutW)
	if err != nil {
		syscall.CloseHandle(hInW)
		syscall.CloseHandle(hOutR)
		return err
	}

	debugf("CreatePseudoConsole ok: hpcon=%#x size=%dx%d hInW=%#x hOutR=%#x",
		uintptr(hpcon), cols, rows, uintptr(hInW), uintptr(hOutR))

	// ---- 3) 属性列表：把伪控制台绑给即将创建的子进程 ----
	attrList, attrBuf, err := newPseudoConsoleAttrList(hpcon)
	if err != nil {
		syscall.CloseHandle(hInW)
		syscall.CloseHandle(hOutR)
		procClosePseudoConsole.Call(uintptr(hpcon))
		return err
	}
	defer procDeleteProcThreadAttributeList.Call(uintptr(attrList))

	// ---- 4) 环境块 ----
	//
	// cfg.Env 是完整环境（不是"增量"），由 Agent 层的白名单构造。
	// nil 表示继承父进程环境，仅供 PoC 和测试使用 —— Agent 绝不能这么干：
	// 实测教训（F2）是，整体继承会把本机坏掉的 https_proxy 一起传下去，
	// Claude Code 会因此报 502。
	envBlock, err := buildEnvBlock(cfg.Env)
	if err != nil {
		syscall.CloseHandle(hInW)
		syscall.CloseHandle(hOutR)
		procClosePseudoConsole.Call(uintptr(hpcon))
		return err
	}

	// ---- 5) 起进程 ----
	cmdline := buildCommandLine(cfg.Command, cfg.Args)
	debugf("attrList=%p envBlockEntries=%d cmdline=%s", attrList, len(cfg.Env), cmdline)
	if err := p.spawn(cmdline, cfg.Dir, envBlock, attrList); err != nil {
		syscall.CloseHandle(hInW)
		syscall.CloseHandle(hOutR)
		procClosePseudoConsole.Call(uintptr(hpcon))
		return err
	}
	// 属性列表的内容已被 CreateProcessW 复制走，attrBuf 到这里才安全释放。
	runtime.KeepAlive(attrBuf)

	// 线程句柄我们不用，立刻关掉（否则每次建会话泄漏一个句柄）。
	syscall.CloseHandle(p.pi.Thread)
	p.pi.Thread = 0

	p.hpcon, p.hInW, p.hOutR = hpcon, hInW, hOutR
	p.cols, p.rows = cols, rows
	p.started = true
	return nil
}

// stdHandlesMu 串行化「临时置空父进程 std 句柄 → CreateProcessW → 恢复」这段临界区。
//
// ★ 必须串行。这是进程级全局状态，两个会话并发启动时如果交错，
//
//	会出现"A 恢复句柄时 B 还停在置空状态"，B 的子进程于是又继承到错误句柄 ——
//	而且是偶发的，极难复现。
//
// ★ 已知副作用：临界区内其他 goroutine 写 os.Stdout/os.Stderr 会失败
//
//	（句柄是 NULL）。临界区只有一次 CreateProcessW 调用（微秒级），
//	实践中可以接受。但如果 Agent 将来要高频往 stdout 打日志，
//	应该改成写日志文件 —— 这一点在设计文档 §23 里已经定了。
var stdHandlesMu sync.Mutex

// spawn 创建子进程，并把它绑到伪控制台上。
//
// ★ 这里是整个文件最关键的一段。前置知识：
//
//	CreateProcessW 的 bInheritHandles=FALSE 只能阻止【句柄继承】，
//	阻止不了【std 句柄复制】。当 STARTF_USESTDHANDLES 未设置时，
//	新进程的 stdin/stdout/stderr 会被设成父进程当前的那三个句柄值 ——
//	哪怕父进程的 stdout 是个重定向的管道。
//
//	一旦发生，子进程的 Console API 就不会返回伪控制台句柄，
//	所有输出都写进了父进程的管道，ConPTY 那边永远是空的。
//
// 实测验证（PHASE0-R1-CONPTY-FINDINGS §4）：把父进程三个 std 句柄
// 临时置 NULL 之后，子进程立刻拿到 BUF=100x30 的伪控制台，
// 正是我们请求的尺寸。
func (p *conPTY) spawn(cmdline, cwd string, envBlock []uint16, attrList unsafe.Pointer) error {
	stdHandlesMu.Lock()
	defer stdHandlesMu.Unlock()

	ids := [...]int{
		syscall.STD_INPUT_HANDLE,
		syscall.STD_OUTPUT_HANDLE,
		syscall.STD_ERROR_HANDLE,
	}
	var saved [len(ids)]syscall.Handle
	for i, id := range ids {
		if h, err := syscall.GetStdHandle(id); err == nil {
			saved[i] = h
		}
		// 置 NULL。SetStdHandle 的 stdhandle 参数是 DWORD，
		// 而常量 STD_*_HANDLE 是负数，所以要走一趟 uint32 再转 uintptr。
		procSetStdHandle.Call(uintptr(uint32(int32(id))), 0)
	}
	restore := func() {
		for i, id := range ids {
			procSetStdHandle.Call(uintptr(uint32(int32(id))), uintptr(saved[i]))
		}
	}
	// defer 是为了 panic 路径也能恢复；正常路径在 CreateProcessW 之后
	// 会先手动恢复一次（否则后面的 debugf 写不出 stderr）。
	defer restore()

	debugf("spawn: 父进程原 std 句柄 in=%#x out=%#x err=%#x（已临时置 NULL）",
		uintptr(saved[0]), uintptr(saved[1]), uintptr(saved[2]))

	var siEx startupInfoExW
	siEx.Cb = uint32(unsafe.Sizeof(siEx))
	siEx.AttributeList = attrList

	cmdPtr, err := syscall.UTF16PtrFromString(cmdline)
	if err != nil {
		return fmt.Errorf("terminal: 命令行编码失败: %w", err)
	}

	var cwdPtr *uint16
	if cwd != "" {
		if cwdPtr, err = syscall.UTF16PtrFromString(cwd); err != nil {
			return fmt.Errorf("terminal: 工作目录编码失败: %w", err)
		}
	}

	var envPtr *uint16
	if len(envBlock) > 0 {
		envPtr = &envBlock[0]
	}

	r1, _, callErr := procCreateProcessW.Call(
		0, // lpApplicationName = NULL：从命令行解析可执行文件
		uintptr(unsafe.Pointer(cmdPtr)),
		0, // lpProcessAttributes
		0, // lpThreadAttributes
		0, // bInheritHandles = FALSE
		extendedStartupInfoPresent|createUnicodeEnvironment,
		uintptr(unsafe.Pointer(envPtr)),
		uintptr(unsafe.Pointer(cwdPtr)),
		uintptr(unsafe.Pointer(&siEx)),
		uintptr(unsafe.Pointer(&p.pi)),
	)
	runtime.KeepAlive(envBlock)
	runtime.KeepAlive(cmdline)
	runtime.KeepAlive(cwd)

	// 先恢复 std 句柄，否则下面的 debugf 写不出 stderr。
	restore()

	debugf("spawn: CreateProcessW r1=%d err=%v pid=%d", r1, callErr, p.pi.ProcessId)

	if r1 == 0 {
		return fmt.Errorf("terminal: CreateProcessW(%s) 失败: %w", cmdline, callErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 数据平面
// ---------------------------------------------------------------------------

// Read 从伪控制台读取原始字节。
//
// 返回的切片可能把多字节字符或转义序列切成两半 —— 这是正常的，
// 上层必须当作不透明字节流转发（§16.3）。
func (p *conPTY) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}

	// ★ 持读锁跨越整个 ReadFile：Close 要靠 outMu 的写锁来确认
	//   "没有线程还阻塞在这个句柄上"，才敢关它。
	p.outMu.RLock()
	defer p.outMu.RUnlock()

	if p.hOutR == 0 {
		return 0, io.EOF
	}

	var n uint32
	err := syscall.ReadFile(p.hOutR, b, &n, nil)
	if err != nil {
		// ERROR_BROKEN_PIPE：所有写端都已关闭，也就是子进程和伪控制台都没了。
		// 这是正常结束，不是故障。
		if errors.Is(err, syscall.ERROR_BROKEN_PIPE) {
			return int(n), io.EOF
		}
		// CancelIoEx 之后的返回码，同样按正常结束处理。
		if errors.Is(err, syscall.ERROR_OPERATION_ABORTED) {
			return int(n), io.EOF
		}
		return int(n), err
	}
	if n == 0 {
		// 管道读返回 0 字节且无错误，理论上不该出现。
		// 防御性处理：报 EOF 而不是返回 (0, nil)，
		// 否则 Session 的读循环会空转烧 CPU。
		return 0, io.EOF
	}
	return int(n), nil
}

// Write 把字节写进伪控制台输入。
func (p *conPTY) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}

	p.inMu.RLock()
	defer p.inMu.RUnlock()

	if p.hInW == 0 {
		return 0, ErrClosed
	}

	var n uint32
	if err := syscall.WriteFile(p.hInW, b, &n, nil); err != nil {
		// 子进程已退出时写输入会拿到 ERROR_BROKEN_PIPE。
		// 换成 ErrClosed，让上层能区分"会话结束了"和"真出错了"。
		if errors.Is(err, syscall.ERROR_BROKEN_PIPE) {
			return int(n), ErrClosed
		}
		return int(n), err
	}
	return int(n), nil
}

// Resize 调整伪控制台尺寸。
//
// ★ 不能省。全屏 TUI（vim / htop / Claude Code / Codex）按这个尺寸布局，
// 尺寸不对会直接花屏或错位。
func (p *conPTY) Resize(cols, rows uint16) error {
	p.mu.Lock()
	started, closed, hpcon := p.started, p.closed, p.hpcon
	p.mu.Unlock()

	if !started {
		return ErrNotStarted
	}
	if closed {
		return ErrClosed
	}
	if cols == 0 || rows == 0 {
		return fmt.Errorf("terminal: 尺寸非法 %dx%d", cols, rows)
	}

	hr, _, _ := procResizePseudoConsole.Call(
		uintptr(hpcon),
		packCoord(cols, rows), // 同样是 COORD 按值传
	)
	// HRESULT：最高位为 1 表示失败。
	if int32(hr) < 0 {
		return fmt.Errorf("terminal: ResizePseudoConsole 失败: HRESULT 0x%08X", uint32(hr))
	}

	p.mu.Lock()
	p.cols, p.rows = cols, rows
	p.mu.Unlock()
	return nil
}

// Signal 投递信号。
//
// ★ Windows 没有 POSIX 信号，这里的三条路径都是"翻译"而不是"转发"：
//
//	SignalInterrupt        → 往 PTY 输入写 0x03
//	SignalTerminate/Kill   → TerminateProcess
//
// 关于 Ctrl+C：ConPTY 没有公开的"注入中断"API。写 0x03 会被 conhost
// 当作一个【输入字符】投递给子进程（实测读到的 ord 就是 3），
// 而不会产生 CTRL_C_EVENT。对 raw mode 的 TUI（Claude Code、Codex）
// 这正是想要的语义：由应用自己解释 Ctrl+C。对依赖控制台中断语义的
// 普通 shell，行为待 Phase 3 用 PowerShell / cmd 确认。
func (p *conPTY) Signal(sig Signal) error {
	switch sig {
	case SignalInterrupt:
		p.mu.Lock()
		started := p.started
		p.mu.Unlock()
		if !started {
			return ErrNotStarted
		}
		if _, err := p.Write([]byte{0x03}); err != nil {
			return err
		}
		return nil

	case SignalTerminate, SignalKill:
		// 两者都落到 TerminateProcess。刻意不返回 ErrUnsupported：
		// 上层（Session.Close）需要一条"确定能把进程停掉"的路径，
		// 否则关会话时会挂住。
		p.mu.Lock()
		h := p.pi.Process
		started := p.started
		p.mu.Unlock()

		if !started {
			return ErrNotStarted
		}
		if h == 0 {
			return nil // 已经 reap 过了，进程早没了
		}
		r1, _, callErr := procTerminateProcess.Call(uintptr(h), 1)
		if r1 == 0 {
			return fmt.Errorf("terminal: TerminateProcess 失败: %w", callErr)
		}
		return nil

	default:
		return fmt.Errorf("%w: %s", ErrUnsupported, sig)
	}
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// Wait 阻塞直到子进程结束。
//
// ★ 调用时机：必须在 Read 返回 EOF 之后。
// 反过来（先 Wait 再读完输出）会丢掉子进程退出前最后写入的那几行 ——
// 编译错误、panic 堆栈这类最有用的信息往往就在最后几行。
func (p *conPTY) Wait() ExitResult {
	p.waitOnce.Do(func() {
		p.exit = p.waitProcess()
		p.reap()
	})
	return p.exit
}

func (p *conPTY) waitProcess() ExitResult {
	p.mu.Lock()
	h, started := p.pi.Process, p.started
	p.mu.Unlock()

	if !started {
		return ExitResult{Err: ErrNotStarted}
	}
	if h == 0 {
		// 句柄已被 reap（Close 先跑完了）。进程确定已经终止，
		// 但拿不到 exit code 了 —— 如实说明，不要编一个 0 出来。
		return ExitResult{Err: ErrClosed}
	}

	r1, _, callErr := procWaitForSingleObject.Call(uintptr(h), infinite)
	if uint32(r1) == waitFailed {
		return ExitResult{Err: fmt.Errorf("terminal: WaitForSingleObject 失败: %w", callErr)}
	}

	var code uint32
	r1, _, callErr = procGetExitCodeProcess.Call(uintptr(h), uintptr(unsafe.Pointer(&code)))
	if r1 == 0 {
		return ExitResult{Err: fmt.Errorf("terminal: GetExitCodeProcess 失败: %w", callErr)}
	}
	return ExitResult{ExitCode: int(code)}
}

// reap 关闭进程与线程句柄。幂等。
func (p *conPTY) reap() {
	p.reapOnce.Do(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.pi.Thread != 0 {
			syscall.CloseHandle(p.pi.Thread)
			p.pi.Thread = 0
		}
		if p.pi.Process != 0 {
			syscall.CloseHandle(p.pi.Process)
			p.pi.Process = 0
		}
	})
}

// Close 释放资源。幂等，可被 Close 路径和 defer 各调一次。
//
// 语义是"终止"：会杀掉进程，而不是把它留在后台。这是 Session.Close
// （用户点了关闭会话）需要的语义。想保留进程请用 Detach，不要用 Close。
func (p *conPTY) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	started, hpcon := p.started, p.hpcon
	p.hpcon = 0
	p.mu.Unlock()

	if !started {
		return nil
	}

	// ---- 1) 关输入写端 ----
	// 子进程会看到 stdin EOF。对交互式 shell 来说这常常能让它自己退出，
	// 比直接 TerminateProcess 干净。
	//
	// 用 inMu 而不是 outMu：Read 正持有 outMu 的读锁阻塞着，
	// 这一步必须能绕开它，否则整条关闭路径都动不了。
	p.inMu.Lock()
	if p.hInW != 0 {
		syscall.CloseHandle(p.hInW)
		p.hInW = 0
	}
	p.inMu.Unlock()

	// ---- 2) 关伪控制台 ----
	// ★ 这一步会终止所有挂在伪控制台上的进程，并且关闭它内部的管道写端 ——
	//   后者正是让阻塞中的 ReadFile 返回 ERROR_BROKEN_PIPE 的原因。
	//   必须先做这一步，第 3 步才不会变成"关闭正在被阻塞读的句柄"这种未定义行为。
	if hpcon != 0 {
		procClosePseudoConsole.Call(uintptr(hpcon))
	}

	// ---- 2.5) 确保进程真的死了 ----
	//
	// ★ 这里有个反直觉的实测结论：ClosePseudoConsole 并【不】终止附属进程。
	//
	//   一直以来的说法是"关闭伪控制台会终止所有 client 进程"，但实测
	//   （Windows 11）：关掉伪控制台后进程仍在运行，WaitForSingleObject
	//   等满超时也没等到它退出。后果是 Close 返回了、会话标记成已关闭，
	//   而用户机器上那个 CLI 还在后台跑着、还占着工作目录 ——
	//   紧接着删除/重命名项目目录会报 "being used by another process"。
	//
	//   所以 Close 必须自己补一刀。这也符合它的语义：Close = 终止。
	//   想保留进程请用 Detach，不要用 Close。
	p.mu.Lock()
	proc := p.pi.Process
	p.mu.Unlock()
	if proc != 0 {
		// 先零等待探一下：进程可能已经自己退出了（用户敲了 exit / Ctrl+D）。
		r1, _, _ := procWaitForSingleObject.Call(uintptr(proc), 0)
		debugf("Close: 关伪控制台后进程状态 = %#x (0=已退出, 0x102=仍在运行)", uint32(r1))
		if uint32(r1) != waitObject0 {
			tr, _, terr := procTerminateProcess.Call(uintptr(proc), 1)
			debugf("Close: TerminateProcess r1=%d err=%v", tr, terr)
			rw, _, _ := procWaitForSingleObject.Call(uintptr(proc), closeWaitTimeoutMS)
			debugf("Close: 终止后等待结果 = %#x", uint32(rw))
		}
	}

	// ---- 3) 关输出读端 ----
	// CancelIoEx 是保险：对同步管道读它其实不生效（微软文档明确说
	// CancelIoEx 只能取消关联了 OVERLAPPED 的请求），真正让 Read 返回的是第 2 步。
	// 保留它是因为在部分系统版本上它确实能缩短关闭耗时，代价接近零。
	// 到这里 Read 已经被第 2 步唤醒并释放了 outMu 的读锁，写锁能立刻拿到。
	p.outMu.Lock()
	if p.hOutR != 0 {
		procCancelIoEx.Call(uintptr(p.hOutR), 0)
		syscall.CloseHandle(p.hOutR)
		p.hOutR = 0
	}
	p.outMu.Unlock()

	// ---- 4) 进程句柄 ----
	// 正常路径是 Wait 之后 reap。但调用方可能从不调 Wait
	// （启动失败清理、测试里直接 Close），这里兜底。
	// 进程已经被第 2 步终止了，所以不会泄漏一个活着的进程。
	p.reap()
	return nil
}

// PID 返回子进程 ID。未启动时返回 0。
func (p *conPTY) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return 0
	}
	return int(p.pi.ProcessId)
}

// ---------------------------------------------------------------------------
// Win32 辅助
// ---------------------------------------------------------------------------

// createPseudoConsole 创建伪控制台。
//
// 注意它返回的是 HRESULT 而不是 BOOL —— 成功是 S_OK(0)，
// 失败是负数的 HRESULT。用 syscall 的第三个返回值（GetLastError）
// 判断是错的，必须直接检查 r1。
func createPseudoConsole(cols, rows uint16, hIn, hOut syscall.Handle) (syscall.Handle, error) {
	var hpcon syscall.Handle

	hr, _, _ := procCreatePseudoConsole.Call(
		packCoord(cols, rows), // COORD 按值传，不是指针 —— 见 packCoord
		uintptr(hIn),
		uintptr(hOut),
		0, // dwFlags：0 = 默认。
		// PSEUDOCONSOLE_INHERIT_CURSOR 这里刻意不用 ——
		// 它会把光标的初始位置从父控制台继承过来，对远程会话没有意义。
		uintptr(unsafe.Pointer(&hpcon)),
	)
	if int32(hr) < 0 {
		return 0, fmt.Errorf("terminal: CreatePseudoConsole(%dx%d) 失败: HRESULT 0x%08X",
			cols, rows, uint32(hr))
	}
	if hpcon == 0 {
		return 0, errors.New("terminal: CreatePseudoConsole 返回了空句柄")
	}
	return hpcon, nil
}

// newPseudoConsoleAttrList 构造只含一个属性的进程属性列表。
//
// 返回的 buf 必须活到 CreateProcessW 返回为止 —— 列表内部有指针指向它，
// Go 的 GC 看不到这层引用。调用方负责 runtime.KeepAlive(buf)。
func newPseudoConsoleAttrList(hpcon syscall.Handle) (unsafe.Pointer, []byte, error) {
	// 第一次调用传 NULL 缓冲区，只为了问出所需大小。
	// 它必然返回 FALSE 并设置 ERROR_INSUFFICIENT_BUFFER，这是标准用法。
	var size uintptr
	procInitializeProcThreadAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return nil, nil, errors.New("terminal: InitializeProcThreadAttributeList 未能确定缓冲区大小")
	}

	buf := make([]byte, size)
	list := unsafe.Pointer(&buf[0])

	r1, _, callErr := procInitializeProcThreadAttrList.Call(
		uintptr(list), 1, 0, uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		return nil, nil, fmt.Errorf("terminal: InitializeProcThreadAttributeList 失败: %w", callErr)
	}

	r1, _, callErr = procUpdateProcThreadAttribute.Call(
		uintptr(list),
		0,
		procThreadAttributePseudoConsole,
		uintptr(hpcon),
		unsafe.Sizeof(hpcon),
		0, 0,
	)
	if r1 == 0 {
		procDeleteProcThreadAttributeList.Call(uintptr(list))
		return nil, nil, fmt.Errorf("terminal: 绑定伪控制台属性失败: %w", callErr)
	}

	return list, buf, nil
}

// buildEnvBlock 把 "K=V" 列表编码成 CreateProcessW 需要的 UTF-16 环境块。
//
// 格式：KEY=VALUE\0 KEY=VALUE\0 \0（结尾是双 NUL）
//
// ★ 必须按【大小写不敏感的字典序】排序，并且同名键去重（保留最后一个）。
//
//	Windows 查找环境变量用的是有序块上的二分查找，不排序虽然多数情况
//	能跑，但会出现"明明传了却查不到"的诡异问题。
//
// env 为 nil 时返回 nil，表示让子进程继承父进程环境。这只给 PoC 和
// 测试用 —— Agent 必须显式提供白名单环境，理由见 Start 里的注释。
func buildEnvBlock(env []string) ([]uint16, error) {
	if env == nil {
		return nil, nil
	}

	// 去重：同名键大小写不敏感，后者覆盖前者。
	seen := make(map[string]int, len(env))
	uniq := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			// 跳过空键和 "=C:=C:\..." 这种驱动器工作目录特例：
			// 它们语义特殊，交给上层显式构造更清楚。
			continue
		}
		key := strings.ToUpper(kv[:eq])
		if idx, ok := seen[key]; ok {
			uniq[idx] = kv
			continue
		}
		seen[key] = len(uniq)
		uniq = append(uniq, kv)
	}

	sort.Slice(uniq, func(i, j int) bool {
		return strings.ToUpper(uniq[i]) < strings.ToUpper(uniq[j])
	})

	block := make([]uint16, 0, len(uniq)*16+1)
	for _, kv := range uniq {
		// StringToUTF16 的结果末尾自带一个 NUL，正好作为条目分隔符。
		block = append(block, syscall.StringToUTF16(kv)...)
	}
	// 环境块以额外的 NUL 结束。
	block = append(block, 0)
	return block, nil
}

// buildCommandLine 把可执行文件与参数拼成命令行字符串。
//
// ★ CreateProcessW 的 lpApplicationName 为 NULL 时，它用自己的规则解析
//
//	第一个 token（和 C 运行库的 argv 规则不同）。标准做法是给每个参数
//	加引号并转义内部的引号和反斜杠，这里复用 Go 标准库
//	syscall.EscapeArg 的算法。
func buildCommandLine(command string, args []string) string {
	var b strings.Builder
	b.WriteString(escapeArg(command))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(escapeArg(a))
	}
	return b.String()
}

// escapeArg 按 CommandLineToArgvW 的规则给参数加引号。
//
// 与 Go 标准库 syscall.EscapeArg 的算法一致（BSD 许可）：
//
//	不含空格/制表符/引号的参数原样返回；
//	否则整体加引号，并处理"反斜杠紧邻引号"的转义规则 ——
//	引号前的反斜杠要翻倍，引号本身要加反斜杠。
func escapeArg(s string) string {
	if len(s) == 0 {
		return `""`
	}

	needsQuote := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t':
			needsQuote++
		case '"':
			needsQuote++
		}
	}
	if needsQuote == 0 {
		return s
	}

	b := make([]byte, 0, len(s)+2*needsQuote+2)
	b = append(b, '"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		default:
			slashes = 0
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b = append(b, '\\')
			}
			b = append(b, '\\')
		}
		b = append(b, c)
	}
	for ; slashes > 0; slashes-- {
		b = append(b, '\\')
	}
	b = append(b, '"')
	return string(b)
}
