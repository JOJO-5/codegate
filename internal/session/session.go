package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/buffer"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/terminal"
)

// 会话层的哨兵错误。
var (
	// ErrClosed 表示会话已被用户显式关闭（terminated），不能再操作。
	ErrClosed = errors.New("session: 会话已关闭")
	// ErrNotAttached 表示这条连接没有 attach 到这个会话。
	ErrNotAttached = errors.New("session: 该连接未 attach 到此会话")
	// ErrNotController 表示只有主控客户端才能做这个操作（当前是 resize）。
	ErrNotController = errors.New("session: 只有主控客户端可以 resize")
	// ErrReadOnlyViewer 表示 viewer 角色是只读的。
	ErrReadOnlyViewer = errors.New("session: viewer 是只读的，不能输入")
)

// ---------------------------------------------------------------------------
// Sink：会话输出的接收端
// ---------------------------------------------------------------------------

// Sink 是会话输出的接收端，通常是 Agent 的 WS 连接。
//
// ★ 两个方法都必须【非阻塞】且【自己复制数据】：
//
//   - 非阻塞：它们在 Session 的锁内被调用。慢一秒就阻塞 attach/detach，
//     进而拖住整个会话的管理操作。
//   - 复制：传入的切片指向会被复用的 PTY 读缓冲，方法返回后内容即失效。
//
// 返回 false 表示消费端跟不上、这一帧被丢弃。Session 会记账，
// 并在后续帧上打 dropped 标记，让客户端知道该做全量重放。
type Sink interface {
	// SendOutput 发送实时输出。dropped 为 true 表示本帧之前有字节丢失。
	SendOutput(p []byte, dropped bool) bool
	// SendBuffer 发送重放数据。end 为 true 表示重放结束、后续是实时流。
	SendBuffer(p []byte, end bool) bool
}

// Role 是客户端在会话里的角色。
//
// 为什么要有角色（§7.6）：同一个账号可能同时有手机和电脑 attach 上来，
// 而它们的分辨率差好几倍。如果都能 resize，TUI 会在 40 列和 120 列之间
// 反复横跳。所以 resize 必须有一个唯一权威。
//
// viewer 是**只读**的：它不能输入，也不能 resize。要输入就先「接管控制」。
// 这不是防人（同一账号都是自己），是防"两台设备同时敲键盘"这种混乱。
type Role string

const (
	RoleController Role = "controller"
	RoleViewer     Role = "viewer"
)

// View 是一个已 attach 的客户端。
type View struct {
	ConnID string
	Role   Role

	sink    Sink
	lastSeq uint64 // 已发给这个客户端的最大序号
	dropped bool   // 上次发送以来是否发生过丢帧
}

// ---------------------------------------------------------------------------
// attach 重放序列（§7.7）
// ---------------------------------------------------------------------------

var (
	// resetSeq 把客户端拉回一个干净的已知状态：退出备用屏幕 + 清屏 + 归位。
	//
	// 为什么必须先退出备用屏幕：新 attach 的 xterm.js 默认在主缓冲区，
	// 而应用可能一直在备用缓冲区里画。不先对齐，后面所有绘制都会落到错的缓冲区。
	resetSeq = []byte("\x1b[?1049l\x1b[2J\x1b[H")

	// clearSeq 在正确的缓冲区里清屏。
	clearSeq = []byte("\x1b[2J\x1b[H")

	// syncOffSeq 无条件关闭同步输出。
	//
	// ★ 这一步不能省。Codex 和 OpenCode 都用 `?2026h ... ?2026l` 把整帧包起来防撕裂。
	// 如果 ring buffer 的切片恰好落在一个 2026 块中间，客户端就会收到 2026h
	// 而永远等不到配对的 2026l —— xterm.js 停在同步模式，画面完全冻住。
	// 这是低概率但只在重连路径复现的严重 bug。
	syncOffSeq = []byte("\x1b[?2026l")
)

// resizeThrottle 是 resize 的合并窗口（R8）。
//
// ConPTY 的 resize 会触发全屏重绘，高频调用会引发输出风暴。
// 拖窗口时前端已经 debounce 过一次，这里是服务端侧的第二道闸。
const resizeThrottle = 50 * time.Millisecond

// closeWaitTimeout 是 Close 等待读循环退出的上限。
//
// 不能让 Close 无限等：万一某个平台实现有 bug 不返回，
// 整个优雅关闭流程就会挂住。
const closeWaitTimeout = 5 * time.Second

// readBufSize 是 PTY 读缓冲大小。
//
// 32 KB 是折中：太小则高频输出时系统调用过多，太大则单次扇出的延迟变高。
const readBufSize = 32 << 10

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// Session 是一个终端会话。
//
// # 最重要的设计约束
//
// **Session 的生命周期与浏览器连接完全无关**（不变量 I1/I2）。
// 代码层面的体现是：Session 里没有任何 *websocket.Conn 之类的引用，
// 只有一个 io.Writer 抽象的 Sink。浏览器断开时走 Detach，
// 它只把 view 从 map 里删掉 —— PTY、读循环、ring buffer 全都不受影响。
//
// 读循环的 context 由 Session 自己持有，只在 Close 时被 cancel。
// 因此「浏览器断开」在物理上不可能终止读循环。
type Session struct {
	ID       uuid.UUID
	DeviceID uuid.UUID
	UserID   uuid.UUID

	Recovery *protocol.ConversationBinding
	Name     string
	Command  string
	Args     []string
	Cwd      string

	CreatedAt time.Time

	pty     terminal.Terminal
	ring    *buffer.Ring
	tracker *terminal.ModeTracker

	cancel    context.CancelFunc
	closed    chan struct{} // 读循环退出后关闭
	closeOnce sync.Once

	mu             sync.Mutex
	proc           ProcState
	cols, rows     uint16
	views          map[string]*View
	order          []string // attach 顺序，用于 controller 顺位
	controller     string
	startedAt      time.Time
	endedAt        time.Time
	exitCode       int
	exitSignal     terminal.Signal
	exitReason     string
	lastAttachedAt time.Time
	lastResizeAt   time.Time
	pendingResize  [2]uint16
	resizeTimer    *time.Timer
}

// newSession 由 Manager 调用。不导出：会话必须经 Manager 创建，
// 以保证 MaxSessions 之类的全局约束不被绕过。
func newSession(
	ctx context.Context,
	req CreateRequest,
	pty terminal.Terminal,
	ringSize int,
	now time.Time,
) *Session {
	ctx, cancel := context.WithCancel(ctx)

	s := &Session{
		ID:        uuid.New(),
		DeviceID:  req.DeviceID,
		UserID:    req.UserID,
		Recovery:  req.Recovery,
		Name:      req.Name,
		Command:   req.Command,
		Args:      req.Args,
		Cwd:       req.Cwd,
		CreatedAt: now,
		pty:       pty,
		ring:      buffer.New(ringSize),
		tracker:   terminal.NewModeTracker(),
		cancel:    cancel,
		closed:    make(chan struct{}),
		proc:      ProcStarting,
		views:     make(map[string]*View, 2),
	}
	s.startedAt = now
	s.proc = ProcRunning
	s.cols, s.rows = req.Cols, req.Rows

	go s.readLoop(ctx)
	return s
}

// ---------------------------------------------------------------------------
// 读循环与扇出
// ---------------------------------------------------------------------------

// readLoop 是唯一从 PTY 读数据的地方。它的生命周期属于 Session，不属于任何连接。
func (s *Session) readLoop(ctx context.Context) {
	defer close(s.closed)

	buf := make([]byte, readBufSize)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			chunk := buf[:n]

			// 1) ring buffer 永远写，永不阻塞、永不失败。
			//    ★ 这是 R3 的核心对策：如果这里会阻塞（比如等某个慢客户端），
			//      PTY 缓冲写满后子进程的 write 就会阻塞 —— 用户本地的 CLI
			//      会被一个网络慢的手机卡死。绝不能发生。
			s.ring.Write(chunk)

			// 2) 跟踪终端模式，供后续 attach 时重建状态（§7.7）。
			//    只记录模式开关的存在性，不理解内容、不修改字节。
			s.tracker.Scan(chunk)

			// 3) 非阻塞扇出给各客户端。
			s.fanout(chunk)
		}
		if err != nil {
			break
		}
		// ctx 只用于 Close 时的快速退出；正常路径靠 Read 返回错误退出。
		if ctx.Err() != nil {
			break
		}
	}

	// ★ 必须在 Read 返回之后才 Wait —— 这样能保证子进程退出前最后写入的
	//   输出已经全部读完，不会丢最后几行（§6.2 第 3 点）。
	res := s.pty.Wait()
	s.finish(res)
}

// fanout 把一段输出发给所有已 attach 的客户端。
//
// 全程持锁：这样 Attach 里的重放序列不会被实时字节插进来，
// 保证「重放 → 实时」的边界是干净的。
// Sink 的契约是非阻塞的，所以持锁调用是可接受的。
func (s *Session) fanout(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.views) == 0 {
		return
	}
	total := s.ring.Total()
	for _, v := range s.views {
		if v.sink.SendOutput(p, v.dropped) {
			v.lastSeq = total
			v.dropped = false
		} else {
			// 这个客户端跟不上了。只影响它自己，不影响别人，更不影响 PTY。
			v.dropped = true
		}
	}
}

// finish 在读循环退出后收敛状态。
func (s *Session) finish(res terminal.ExitResult) {
	s.mu.Lock()
	switch {
	case s.proc == ProcTerminated:
		// 用户已显式关闭：保持 terminated 语义，不要被覆写成 exited（不变量 I4）。
	case res.Err != nil:
		s.proc = ProcFailed
		s.exitReason = "pty_error"
	default:
		s.proc = ProcExited
		s.exitCode = res.ExitCode
		s.exitSignal = res.Signal
		s.exitReason = "exited"
	}
	s.endedAt = time.Now()
	if s.resizeTimer != nil {
		s.resizeTimer.Stop()
		s.resizeTimer = nil
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 生命周期操作
// ---------------------------------------------------------------------------

// AttachRequest 是一次 attach 的输入。
type AttachRequest struct {
	ConnID string
	Sink   Sink
	// Since 的语义见 §15.3：0 = 要全量，否则只要该序号之后的增量。
	Since uint64
	Cols  uint16
	Rows  uint16
}

// AttachResult 是一次 attach 的结果。
type AttachResult struct {
	Role Role
	// SeqFrom/SeqTo 是本次重放覆盖的区间。
	// 若 SeqFrom > 请求的 Since，说明客户端落后太多、中间有丢帧。
	SeqFrom   uint64
	SeqTo     uint64
	Truncated bool
}

// Attach 把一个客户端接到会话上，并按固定顺序重放状态。
//
// 发送顺序（§7.7，六步不能变）：
//
//  1. 退出备用屏幕 + 清屏 + 归位   —— 先把客户端拉回已知状态
//  2. 模式前导（ModeTracker.Preamble）—— 重放当前所有 ON 的模式
//  3. 清屏 + 归位                  —— 在正确的缓冲区里清
//  4. ring buffer 增量/全量
//  5. 无条件补一个 ?2026l          —— 防同步输出卡死
//  6. 登记 view，之后进入实时流
//
// 整个序列在锁内一次性发出，保证中间不会插进实时字节。
func (s *Session) Attach(req AttachRequest) (AttachResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !CanAttach(s.proc) {
		return AttachResult{}, fmt.Errorf("%w: 状态 %s", ErrClosed, s.proc)
	}

	// 已 attach 过就更新 sink（重连场景），避免重复登记。
	if _, exists := s.views[req.ConnID]; !exists {
		s.order = append(s.order, req.ConnID)
	}

	role := RoleViewer
	if s.controller == "" {
		role = RoleController
		s.controller = req.ConnID
	}

	seqTo := s.ring.Total()
	data, truncated := s.ring.Since(req.Since)
	seqFrom := seqTo - uint64(len(data))

	// An incremental attach retains the browser's existing screen. Resetting
	// it and then sending only the tail leaves an incomplete TUI frame.
	// A new browser (Since=0), or one whose ring window was overrun, needs
	// the full replay and the mode preamble.
	send := func(data []byte, end bool) error {
		if !req.Sink.SendBuffer(data, end) {
			return fmt.Errorf("会话历史重放队列已满，请稍后重试")
		}
		return nil
	}
	if req.Since == 0 || truncated {
		if err := send(resetSeq, false); err != nil {
			return AttachResult{}, err
		}
		if pre := s.tracker.Preamble(); len(pre) > 0 {
			if err := send(pre, false); err != nil {
				return AttachResult{}, err
			}
		}
		if err := send(clearSeq, false); err != nil {
			return AttachResult{}, err
		}
	}
	if len(data) > 0 { // 4
		if err := send(data, false); err != nil {
			return AttachResult{}, err
		}
	}
	if err := send(syncOffSeq, true); err != nil {
		return AttachResult{}, err
	}

	// ---- 6. 登记 ----
	s.views[req.ConnID] = &View{
		ConnID:  req.ConnID,
		Role:    role,
		sink:    req.Sink,
		lastSeq: seqTo,
	}
	s.lastAttachedAt = time.Now()

	return AttachResult{
		Role:      role,
		SeqFrom:   seqFrom,
		SeqTo:     seqTo,
		Truncated: truncated,
	}, nil
}

// Detach 断开一个客户端。
//
// ★ 它只做一件事：把 view 从 map 里删掉。
//
//	不 Close(pty)、不 cancel(ctx)、不动 ring buffer。
//	这是"浏览器断开后 CLI 继续运行"这条核心要求最直接的落点。
func (s *Session) Detach(connID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeViewLocked(connID)
}

// removeViewLocked 摘除一个 view，并在必要时顺位新的 controller。
// 调用方必须持有锁。
func (s *Session) removeViewLocked(connID string) {
	if _, ok := s.views[connID]; !ok {
		return
	}
	delete(s.views, connID)

	// 从 attach 顺序里摘掉
	for i, id := range s.order {
		if id == connID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}

	// controller 走了就顺位给最早 attach 的那个（§7.6）
	if s.controller == connID {
		s.controller = ""
		if len(s.order) > 0 {
			next := s.order[0]
			s.controller = next
			if v, ok := s.views[next]; ok {
				v.Role = RoleController
			}
		}
	}
}

// ClaimControl 把主控权交给指定客户端。
//
// 手机上想接管电脑的会话时需要它（§7.6）。返回被降级的前任 controller 的
// ConnID，便于 Agent 层通知它切换 UI 状态。
func (s *Session) ClaimControl(connID string) (previous string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.views[connID]
	if !ok {
		return "", ErrNotAttached
	}
	if s.controller == connID {
		return "", nil
	}
	previous = s.controller
	if prev, ok := s.views[previous]; ok {
		prev.Role = RoleViewer
	}
	s.controller = connID
	v.Role = RoleController
	return previous, nil
}

// Close 关闭会话并终止进程。
//
// reason 会写进审计（区分"用户关的"和"Agent 重启导致的"）。
func (s *Session) Close(reason string) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.proc.Alive() {
			s.proc = ProcTerminated
		}
		if s.exitReason == "" {
			s.exitReason = reason
		}
		// 清空所有 view：连接层会各自感知到，但这里先把引用断掉，
		// 避免关闭后还有人往已死的会话里发东西。
		s.views = make(map[string]*View, 0)
		s.order = nil
		s.controller = ""
		if s.resizeTimer != nil {
			s.resizeTimer.Stop()
			s.resizeTimer = nil
		}
		s.mu.Unlock()

		s.cancel()
		_ = s.pty.Close()
	})

	select {
	case <-s.closed:
	case <-time.After(closeWaitTimeout):
		// 读循环没在超时内退出。不阻塞关闭流程，但要留痕。
		return fmt.Errorf("session %s: 等待读循环退出超时", s.ID)
	}
	return nil
}

// Done 在读循环退出（进程结束或会话关闭）后关闭。
// Agent 层监听它来广播 session.exit。
func (s *Session) Done() <-chan struct{} { return s.closed }

// ---------------------------------------------------------------------------
// 输入路径
// ---------------------------------------------------------------------------

// Write 把字节写进 PTY（即用户按键）。
//
// 只有 controller 能输入：viewer 是只读的（§7.6）。
// 这不是防人 —— 同一账号都是自己 —— 而是防"手机和电脑同时敲键盘"的混乱。
func (s *Session) Write(connID string, p []byte) (int, error) {
	s.mu.Lock()
	v, ok := s.views[connID]
	if !ok {
		s.mu.Unlock()
		return 0, ErrNotAttached
	}
	if v.Role != RoleController {
		s.mu.Unlock()
		return 0, ErrReadOnlyViewer
	}
	if !CanInput(s.proc) {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: 状态 %s", ErrClosed, s.proc)
	}
	s.mu.Unlock()

	// PTY 写入可能阻塞，所以放在锁外。
	return s.pty.Write(p)
}

// Signal 向会话投递信号。
func (s *Session) Signal(sig terminal.Signal) error {
	s.mu.Lock()
	alive := s.proc.Alive()
	s.mu.Unlock()
	if !alive {
		return fmt.Errorf("%w: 进程已结束", ErrClosed)
	}
	return s.pty.Signal(sig)
}

// Resize 调整终端尺寸。
//
// 只有 controller 的请求会被接受；同时对 50ms 内的连续调用做合并（R8），
// 只保留最后一次 —— 拖窗口时前端 debounce 过了，但网络抖动可能带来
// 成簇的 resize，服务端再兜一道。
func (s *Session) Resize(connID string, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return fmt.Errorf("session: 非法的尺寸 %dx%d", cols, rows)
	}

	s.mu.Lock()
	v, ok := s.views[connID]
	if !ok {
		s.mu.Unlock()
		return ErrNotAttached
	}
	if v.Role != RoleController {
		s.mu.Unlock()
		return ErrNotController
	}
	if !s.proc.Alive() {
		s.mu.Unlock()
		return fmt.Errorf("%w: 进程已结束", ErrClosed)
	}

	now := time.Now()
	if s.lastResizeAt.IsZero() || now.Sub(s.lastResizeAt) >= resizeThrottle {
		s.lastResizeAt = now
		s.mu.Unlock()
		return s.pty.Resize(cols, rows)
	}

	// 距上次太近：只记下最后一次，稍后统一发。
	s.pendingResize = [2]uint16{cols, rows}
	if s.resizeTimer == nil {
		s.resizeTimer = time.AfterFunc(resizeThrottle, s.flushResize)
	}
	s.mu.Unlock()
	return nil
}

// flushResize 发出被合并掉的最后一次 resize。
func (s *Session) flushResize() {
	s.mu.Lock()
	pending := s.pendingResize
	has := pending[0] != 0 && pending[1] != 0
	s.pendingResize = [2]uint16{}
	s.resizeTimer = nil
	s.lastResizeAt = time.Now()
	alive := s.proc.Alive()
	s.mu.Unlock()

	if has && alive {
		_ = s.pty.Resize(pending[0], pending[1])
	}
}

// ---------------------------------------------------------------------------
// 只读视图
// ---------------------------------------------------------------------------

// Status 返回对外的扁平状态。
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Derive(s.proc, s.attachStateLocked())
}

// attachStateLocked 由 view 数量派生 —— 不额外存一个字段，
// 免得两处状态不同步。调用方必须持有锁。
func (s *Session) attachStateLocked() AttachState {
	if len(s.views) > 0 {
		return AttachAttached
	}
	return AttachNone
}

// ViewCount 返回当前 attach 的客户端数。
func (s *Session) ViewCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.views)
}

// ControllerID 返回当前主控客户端的 ConnID。
func (s *Session) ControllerID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controller
}

// RoleOf 返回某个客户端在会话里的角色。
func (s *Session) RoleOf(connID string) (Role, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.views[connID]
	if !ok {
		return "", false
	}
	return v.Role, true
}

// Buffer 返回会话的环形缓冲（只读用途）。
func (s *Session) Buffer() *buffer.Ring { return s.ring }

// Tracker 返回模式跟踪器（测试与调试用）。
func (s *Session) Tracker() *terminal.ModeTracker { return s.tracker }

// PID 返回子进程 ID。
func (s *Session) PID() int { return s.pty.PID() }

// Summary 生成对外快照。
func (s *Session) Summary() protocol.SessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summaryLocked()
}

func (s *Session) summaryLocked() protocol.SessionSummary {
	sum := protocol.SessionSummary{
		Recovery:      s.Recovery,
		SessionID:     s.ID.String(),
		DeviceID:      s.DeviceID.String(),
		Name:          s.Name,
		Command:       s.Command,
		Args:          s.Args,
		Cwd:           s.Cwd,
		Status:        string(Derive(s.proc, s.attachStateLocked())),
		PID:           s.pty.PID(),
		Cols:          s.cols,
		Rows:          s.rows,
		CreatedAt:     s.CreatedAt.UnixMilli(),
		BufferSeqFrom: s.ring.Oldest(),
		BufferSeqTo:   s.ring.Total(),
	}
	if s.proc == ProcExited || s.proc == ProcFailed {
		code := s.exitCode
		sum.ExitCode = &code
	}
	if !s.startedAt.IsZero() {
		v := s.startedAt.UnixMilli()
		sum.StartedAt = &v
	}
	if !s.endedAt.IsZero() {
		v := s.endedAt.UnixMilli()
		sum.EndedAt = &v
	}
	if !s.lastAttachedAt.IsZero() {
		v := s.lastAttachedAt.UnixMilli()
		sum.LastAttachedAt = &v
	}
	return sum
}

// ExitInfo 返回退出详情，供 Agent 层组装 session.exit 消息。
func (s *Session) ExitInfo() (code int, sig terminal.Signal, reason string, ended bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode, s.exitSignal, s.exitReason, !s.endedAt.IsZero()
}

// BindConversation publishes immutable copies under the session lock.
func (s *Session) BindConversation(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Recovery == nil || s.Recovery.NativeID == id {
		return false
	}
	next := *s.Recovery
	next.NativeID = id
	s.Recovery = &next
	return true
}

func (s *Session) BindConversationIdentity(id, root string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Recovery == nil || (s.Recovery.NativeID == id && s.Recovery.NativeRoot == root) {
		return false
	}
	next := *s.Recovery
	next.NativeID = id
	next.NativeRoot = root
	s.Recovery = &next
	return true
}
