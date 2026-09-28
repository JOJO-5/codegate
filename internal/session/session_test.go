package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/terminal"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakePTY 是一个可控的 Terminal 实现。
//
// 它的存在让会话层可以完全脱离真实 PTY 测试 —— 这是 Phase 1 能在
// 没有 ConPTY 实现的情况下就测出会话不变量的原因。
type fakePTY struct {
	pid int

	mu      sync.Mutex
	reads   chan []byte
	done    chan struct{} // Close 触发
	finish  chan struct{} // 进程自然退出
	written bytes.Buffer
	resizes [][2]uint16
	signals []terminal.Signal
	closed  int
	exit    terminal.ExitResult
}

func newFakePTY(pid int) *fakePTY {
	return &fakePTY{
		pid:    pid,
		reads:  make(chan []byte, 64),
		done:   make(chan struct{}),
		finish: make(chan struct{}),
	}
}

func (f *fakePTY) Start(context.Context, terminal.StartConfig) error { return nil }

func (f *fakePTY) Read(p []byte) (int, error) {
	select {
	case chunk, ok := <-f.reads:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, chunk), nil
	case <-f.done:
		return 0, io.EOF
	case <-f.finish:
		return 0, io.EOF
	}
}

func (f *fakePTY) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.Write(p)
}

func (f *fakePTY) Resize(cols, rows uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint16{cols, rows})
	return nil
}

func (f *fakePTY) Signal(sig terminal.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	return nil
}

func (f *fakePTY) Wait() terminal.ExitResult {
	select {
	case <-f.done:
	case <-f.finish:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exit
}

func (f *fakePTY) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	if f.closed == 1 {
		close(f.done)
	}
	return nil
}

func (f *fakePTY) PID() int { return f.pid }

// ---- 测试驱动用 ----

// push 模拟 PTY 产生输出。
func (f *fakePTY) push(s string) { f.reads <- []byte(s) }

// exit 模拟进程自然退出。
func (f *fakePTY) exitWith(code int) {
	f.mu.Lock()
	f.exit = terminal.ExitResult{ExitCode: code}
	f.mu.Unlock()
	close(f.finish)
}

func (f *fakePTY) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed > 0
}

func (f *fakePTY) writtenString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.String()
}

func (f *fakePTY) resizeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resizes)
}

func (f *fakePTY) lastResize() (uint16, uint16, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resizes) == 0 {
		return 0, 0, false
	}
	r := f.resizes[len(f.resizes)-1]
	return r[0], r[1], true
}

// fakeFactory 生产 fakePTY，并记住最后一个，方便测试驱动。
type fakeFactory struct {
	mu      sync.Mutex
	ptys    []*fakePTY
	lastCfg terminal.StartConfig
	err     error
}

func (f *fakeFactory) New(_ context.Context, cfg terminal.StartConfig) (terminal.Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	p := newFakePTY(1000 + len(f.ptys))
	f.ptys = append(f.ptys, p)
	f.lastCfg = cfg
	return p, nil
}

func (f *fakeFactory) last() *fakePTY {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ptys) == 0 {
		return nil
	}
	return f.ptys[len(f.ptys)-1]
}

// fakeSink 记录会话发给客户端的所有数据，并按顺序保留。
type fakeSink struct {
	mu sync.Mutex
	// frames 按调用顺序记录每一次 Send*，便于断言重放的六步顺序。
	frames []frame
	full   bool // 模拟消费端跟不上
}

type frame struct {
	kind    string // "buffer" | "output"
	data    []byte
	flag    bool // buffer 的 end / output 的 dropped
	dropped bool
}

func (s *fakeSink) SendOutput(p []byte, dropped bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return false
	}
	s.frames = append(s.frames, frame{kind: "output", data: append([]byte(nil), p...), dropped: dropped})
	return true
}

func (s *fakeSink) SendBuffer(p []byte, end bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frame{kind: "buffer", data: append([]byte(nil), p...), flag: end})
	return true
}

func (s *fakeSink) setFull(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.full = v
}

func (s *fakeSink) snapshot() []frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]frame(nil), s.frames...)
}

// bufferFrames 返回所有 buffer 帧（attach 的重放序列）。
func (s *fakeSink) bufferFrames() []frame {
	var out []frame
	for _, f := range s.snapshot() {
		if f.kind == "buffer" {
			out = append(out, f)
		}
	}
	return out
}

// bufferBytes 把所有 buffer 帧拼起来，方便做子串断言。
func (s *fakeSink) bufferBytes() []byte {
	var buf bytes.Buffer
	for _, f := range s.bufferFrames() {
		buf.Write(f.data)
	}
	return buf.Bytes()
}

func (s *fakeSink) outputBytes() []byte {
	var buf bytes.Buffer
	for _, f := range s.snapshot() {
		if f.kind == "output" {
			buf.Write(f.data)
		}
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

func newTestManager(t *testing.T, cfg Config) (*Manager, *fakeFactory) {
	t.Helper()
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 8
	}
	f := &fakeFactory{}
	return NewManager(f.New, cfg), f
}

func testRequest() CreateRequest {
	return CreateRequest{
		DeviceID: uuid.New(),
		UserID:   uuid.New(),
		Name:     "test",
		Command:  "fake.exe",
		Cwd:      `C:\work`,
		Cols:     100,
		Rows:     30,
	}
}

// waitFor 轮询等待条件成立。异步的读循环需要它。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// ---------------------------------------------------------------------------
// 核心不变量
// ---------------------------------------------------------------------------

// ★ 这是整个项目最重要的一条测试。
//
// 不变量 I1/I2：浏览器断开只能触发 detach，绝不能终止 PTY。
func TestDetachKeepsProcessAlive(t *testing.T) {
	m, f := newTestManager(t, Config{})
	ctx := context.Background()

	s, err := m.Create(ctx, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	pty := f.last()

	sink1 := &fakeSink{}
	if _, err := s.Attach(AttachRequest{ConnID: "c1", Sink: sink1}); err != nil {
		t.Fatal(err)
	}

	pty.push("hello\n")
	waitFor(t, "第一段输出进入 ring", func() bool { return s.Buffer().Total() >= 6 })

	// 浏览器断开
	s.Detach("c1")

	if pty.isClosed() {
		t.Fatal("★ Detach 把 PTY 关掉了 —— 这违反核心不变量 I2")
	}
	if s.Status() != StatusDetached {
		t.Errorf("Status = %s, 期望 detached", s.Status())
	}
	if s.ViewCount() != 0 {
		t.Errorf("ViewCount = %d, 期望 0", s.ViewCount())
	}

	// CLI 必须继续运行、继续产生输出
	pty.push("still running\n")
	waitFor(t, "断开后仍在接收输出", func() bool { return s.Buffer().Total() >= 20 })

	// 重新 attach 能拿到断开期间的输出
	sink2 := &fakeSink{}
	res, err := s.Attach(AttachRequest{ConnID: "c2", Sink: sink2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sink2.bufferBytes()), "still running") {
		t.Errorf("重放里没有断开期间的输出:\n%q", sink2.bufferBytes())
	}
	if res.SeqTo != s.Buffer().Total() {
		t.Errorf("SeqTo = %d, 期望 %d", res.SeqTo, s.Buffer().Total())
	}
}

// attach 的重放必须严格按六步顺序，且以 ?2026l 收尾。
func TestAttachReplayOrder(t *testing.T) {
	m, f := newTestManager(t, Config{})
	s, err := m.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	pty := f.last()

	// 模拟一个进了备用屏幕的 TUI
	pty.push("\x1b[?1049h\x1b[?2027h\x1b[?2004hHELLO")
	waitFor(t, "输出进入 ring", func() bool { return s.Buffer().Total() >= 24 })

	sink := &fakeSink{}
	if _, err := s.Attach(AttachRequest{ConnID: "c1", Sink: sink}); err != nil {
		t.Fatal(err)
	}

	frames := sink.bufferFrames()
	if len(frames) < 4 {
		t.Fatalf("buffer 帧太少: %d 个", len(frames))
	}

	// 1) 第一步必须是"退出备用屏幕 + 清屏"
	if !bytes.HasPrefix(frames[0].data, []byte("\x1b[?1049l")) {
		t.Errorf("第 1 帧应以退出备用屏幕开头，实际 %q", frames[0].data)
	}

	all := sink.bufferBytes()

	// 2) 模式前导必须把 ?1049h / ?2027h / ?2004h 都重建出来
	for _, want := range []string{"\x1b[?1049h", "\x1b[?2027h", "\x1b[?2004h"} {
		if !bytes.Contains(all, []byte(want)) {
			t.Errorf("模式前导缺少 %q —— 重连后 TUI 会画到错的缓冲区", want)
		}
	}

	// 3) ?1049h 必须排在其它模式之前（决定后续绘制落在哪个缓冲区）
	if i1049, i2027 := bytes.Index(all, []byte("\x1b[?1049h")), bytes.Index(all, []byte("\x1b[?2027h")); i1049 > i2027 {
		t.Errorf("?1049h 必须最先发（1049@%d, 2027@%d）", i1049, i2027)
	}

	// 4) 必须原样带上 ring buffer 的内容
	if !bytes.Contains(all, []byte("HELLO")) {
		t.Error("重放里没有 ring buffer 的内容")
	}

	// 5) 最后一帧必须是 end，且内容是补的 ?2026l
	last := frames[len(frames)-1]
	if !last.flag {
		t.Error("最后一帧没有标记 end")
	}
	if !bytes.Equal(last.data, syncOffSeq) {
		t.Errorf("最后一帧应为 ?2026l，实际 %q", last.data)
	}

	// 6) 只有最后一帧带 end
	for i, fr := range frames[:len(frames)-1] {
		if fr.flag {
			t.Errorf("第 %d 帧不该标记 end", i)
		}
	}
}

// 没有进备用屏幕时，前导序列里不该出现 ?1049h。
func TestPreambleOmitsUnsetModes(t *testing.T) {
	m, f := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	f.last().push("plain text output")
	waitFor(t, "输出进入 ring", func() bool { return s.Buffer().Total() >= 17 })

	sink := &fakeSink{}
	if _, err := s.Attach(AttachRequest{ConnID: "c1", Sink: sink}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sink.bufferBytes(), []byte("\x1b[?1049h")) {
		t.Error("没进过备用屏幕，却重放了 ?1049h")
	}
}

// viewer 是只读的：不能输入、不能 resize。
func TestViewerIsReadOnly(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())

	c1 := &fakeSink{}
	c2 := &fakeSink{}
	r1, err := s.Attach(AttachRequest{ConnID: "c1", Sink: c1})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Attach(AttachRequest{ConnID: "c2", Sink: c2})
	if err != nil {
		t.Fatal(err)
	}

	if r1.Role != RoleController {
		t.Errorf("首个 attach 者应为 controller，实际 %s", r1.Role)
	}
	if r2.Role != RoleViewer {
		t.Errorf("第二个 attach 者应为 viewer，实际 %s", r2.Role)
	}

	if _, err := s.Write("c2", []byte("x")); !errors.Is(err, ErrReadOnlyViewer) {
		t.Errorf("viewer 输入应被拒，实际 %v", err)
	}
	if err := s.Resize("c2", 80, 24); !errors.Is(err, ErrNotController) {
		t.Errorf("viewer resize 应被拒，实际 %v", err)
	}

	// controller 可以
	if _, err := s.Write("c1", []byte("y")); err != nil {
		t.Errorf("controller 输入失败: %v", err)
	}
	if err := s.Resize("c1", 80, 24); err != nil {
		t.Errorf("controller resize 失败: %v", err)
	}
}

// controller 断开后必须顺位，不能出现"没人能 resize"的死锁状态。
func TestControllerPromotion(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())

	s.Attach(AttachRequest{ConnID: "c1", Sink: &fakeSink{}})
	s.Attach(AttachRequest{ConnID: "c2", Sink: &fakeSink{}})
	s.Attach(AttachRequest{ConnID: "c3", Sink: &fakeSink{}})

	if got := s.ControllerID(); got != "c1" {
		t.Fatalf("ControllerID = %s, 期望 c1", got)
	}

	s.Detach("c1")
	if got := s.ControllerID(); got != "c2" {
		t.Errorf("c1 断开后 controller 应为 c2，实际 %s", got)
	}
	if role, _ := s.RoleOf("c2"); role != RoleController {
		t.Errorf("c2 的角色没有跟着升级: %s", role)
	}

	s.Detach("c2")
	if got := s.ControllerID(); got != "c3" {
		t.Errorf("应为 c3，实际 %s", got)
	}

	s.Detach("c3")
	if got := s.ControllerID(); got != "" {
		t.Errorf("全断开后 controller 应为空，实际 %s", got)
	}
}

// 接管控制：原 controller 降级为 viewer。
func TestClaimControl(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	s.Attach(AttachRequest{ConnID: "c1", Sink: &fakeSink{}})
	s.Attach(AttachRequest{ConnID: "c2", Sink: &fakeSink{}})

	prev, err := s.ClaimControl("c2")
	if err != nil {
		t.Fatal(err)
	}
	if prev != "c1" {
		t.Errorf("被降级的前任应为 c1，实际 %s", prev)
	}
	if got := s.ControllerID(); got != "c2" {
		t.Errorf("ControllerID = %s, 期望 c2", got)
	}
	if role, _ := s.RoleOf("c1"); role != RoleViewer {
		t.Errorf("c1 应降级为 viewer，实际 %s", role)
	}

	// 未 attach 的连接不能夺权
	if _, err := s.ClaimControl("c9"); !errors.Is(err, ErrNotAttached) {
		t.Errorf("未 attach 的连接夺权应报 ErrNotAttached，实际 %v", err)
	}
}

// Close 必须真的终止进程，并把状态收敛到 terminated。
//
// 注意这里走的是 Manager.Close 而不是 Session.Close：
// 两者语义不同 ——
//
//	Manager.Close(id)  = 用户要求关掉这个会话 → 终止进程 + 从注册表移除
//	Session.Close()    = 只终止进程（内部用、CloseAll 用）
//
// 会话本身不会把自己从 Manager 里摘掉，因为已退出的会话要保留一段时间
// 供用户回看输出（不变量 I3）。
func TestCloseTerminates(t *testing.T) {
	m, f := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	pty := f.last()
	s.Attach(AttachRequest{ConnID: "c1", Sink: &fakeSink{}})

	if err := m.Close(s.ID, "user_request"); err != nil {
		t.Fatalf("Close 报错: %v", err)
	}
	if !pty.isClosed() {
		t.Error("Close 之后 PTY 没被关闭")
	}
	if s.Status() != StatusTerminated {
		t.Errorf("Status = %s, 期望 terminated", s.Status())
	}
	if s.ViewCount() != 0 {
		t.Errorf("Close 后应清空 view，实际 %d", s.ViewCount())
	}
	if _, ok := m.Get(s.ID); ok {
		t.Error("Manager.Close 之后不该还能查到该会话")
	}

	// 关闭后的会话不能再 attach（只有 terminated 才彻底拒绝）
	if _, err := s.Attach(AttachRequest{ConnID: "c2", Sink: &fakeSink{}}); !errors.Is(err, ErrClosed) {
		t.Errorf("已关闭会话的 attach 应报 ErrClosed，实际 %v", err)
	}
}

// 不变量 I3：已退出的会话仍可 attach 回看最后的输出。
func TestExitedSessionStillAttachable(t *testing.T) {
	m, f := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	pty := f.last()

	pty.push("build failed: 3 errors\n")
	waitFor(t, "输出进入 ring", func() bool { return s.Buffer().Total() > 0 })

	pty.exitWith(1)
	waitFor(t, "状态变 exited", func() bool { return s.Status() == StatusExited })

	sink := &fakeSink{}
	if _, err := s.Attach(AttachRequest{ConnID: "c1", Sink: sink}); err != nil {
		t.Fatalf("已退出会话应允许 attach（不变量 I3），实际 %v", err)
	}
	if !strings.Contains(string(sink.bufferBytes()), "build failed") {
		t.Error("没能回看到退出前的输出")
	}

	sum := s.Summary()
	if sum.ExitCode == nil || *sum.ExitCode != 1 {
		t.Errorf("Summary.ExitCode = %v, 期望 1", sum.ExitCode)
	}
	if sum.Status != string(StatusExited) {
		t.Errorf("Summary.Status = %s", sum.Status)
	}
}

// 会话数上限必须生效。
func TestSessionLimit(t *testing.T) {
	m, _ := newTestManager(t, Config{MaxSessions: 2})
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := m.Create(ctx, testRequest()); err != nil {
			t.Fatalf("第 %d 个创建失败: %v", i, err)
		}
	}
	if _, err := m.Create(ctx, testRequest()); !errors.Is(err, ErrLimitReached) {
		t.Errorf("第 3 个应报 ErrLimitReached，实际 %v", err)
	}
	if m.Count() != 2 {
		t.Errorf("Count = %d, 期望 2", m.Count())
	}
}

// 创建失败时不能留下半成品。
func TestCreateFailureCleansUp(t *testing.T) {
	f := &fakeFactory{err: errors.New("boom")}
	m := NewManager(f.New, Config{})

	if _, err := m.Create(context.Background(), testRequest()); err == nil {
		t.Fatal("期望创建失败")
	}
	if m.Count() != 0 {
		t.Errorf("失败后 Count = %d, 期望 0", m.Count())
	}
}

// 慢客户端丢帧只影响它自己，不能影响别人，更不能影响 PTY。
func TestSlowSinkIsIsolated(t *testing.T) {
	m, f := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	pty := f.last()

	slow := &fakeSink{}
	fast := &fakeSink{}
	s.Attach(AttachRequest{ConnID: "slow", Sink: slow})
	s.Attach(AttachRequest{ConnID: "fast", Sink: fast})

	slow.setFull(true)

	for i := 0; i < 5; i++ {
		pty.push("tick\n")
	}
	waitFor(t, "fast 收到全部输出", func() bool {
		return bytes.Count(fast.outputBytes(), []byte("tick")) == 5
	})

	// slow 一个字节都没收到，但 PTY 和 ring 不受影响
	if got := len(slow.outputBytes()); got != 0 {
		t.Errorf("slow 应该一帧都没收到，实际 %d 字节", got)
	}
	if pty.isClosed() {
		t.Error("慢客户端把 PTY 拖挂了")
	}
	waitFor(t, "ring 仍完整", func() bool { return s.Buffer().Total() >= 25 })

	// slow 恢复后应收到带 dropped 标记的帧
	slow.setFull(false)
	pty.push("after\n")
	waitFor(t, "slow 恢复接收", func() bool {
		for _, fr := range slow.snapshot() {
			if fr.kind == "output" && fr.dropped {
				return true
			}
		}
		return false
	})
}

// resize 合并：连续 resize 只保留最后一次（R8）。
func TestResizeThrottle(t *testing.T) {
	m, f := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	pty := f.last()
	s.Attach(AttachRequest{ConnID: "c1", Sink: &fakeSink{}})

	// 第一次立刻生效
	if err := s.Resize("c1", 100, 30); err != nil {
		t.Fatal(err)
	}
	// 紧接着的几次应被合并
	for _, r := range [][2]uint16{{101, 30}, {102, 30}, {103, 31}} {
		if err := s.Resize("c1", r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "合并后的 resize 被发出", func() bool { return pty.resizeCount() >= 2 })
	c, r, _ := pty.lastResize()
	if c != 103 || r != 31 {
		t.Errorf("最后一次 resize 应为 103x31（只保留最后一次），实际 %dx%d", c, r)
	}
}

// 非法的尺寸直接拒绝。
func TestResizeRejectsZero(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	s.Attach(AttachRequest{ConnID: "c1", Sink: &fakeSink{}})

	if err := s.Resize("c1", 0, 30); err == nil {
		t.Error("0 列应被拒绝")
	}
}

// Agent 重启后的对账：旧的活会话必须被标记为 terminated。
func TestMarkOrphaned(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	dev := uuid.New()
	req := testRequest()
	req.DeviceID = dev

	s1, _ := m.Create(context.Background(), req)
	req2 := testRequest()
	req2.DeviceID = uuid.New()
	s2, _ := m.Create(context.Background(), req2)

	if n := m.MarkOrphaned(dev, "agent_restart"); n != 1 {
		t.Errorf("应标记 1 个，实际 %d", n)
	}
	if s1.Status() != StatusTerminated {
		t.Errorf("s1 状态 = %s, 期望 terminated", s1.Status())
	}
	if s2.Status() == StatusTerminated {
		t.Error("别的设备的会话被误伤了")
	}
}

// 序号语义：attach 带 since 时只补增量。
func TestAttachSinceDeliversIncrement(t *testing.T) {
	m, f := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	pty := f.last()

	pty.push("AAAA")
	waitFor(t, "AAAA 到达", func() bool { return s.Buffer().Total() == 4 })
	mark := s.Buffer().Total()

	pty.push("BBBB")
	waitFor(t, "BBBB 到达", func() bool { return s.Buffer().Total() == 8 })

	sink := &fakeSink{}
	res, err := s.Attach(AttachRequest{ConnID: "c1", Sink: sink, Since: mark})
	if err != nil {
		t.Fatal(err)
	}
	got := sink.bufferBytes()
	if bytes.Contains(got, []byte("AAAA")) {
		t.Errorf("带了 since 不该重放 AAAA:\n%q", got)
	}
	if !bytes.Contains(got, []byte("BBBB")) {
		t.Errorf("应补上 BBBB:\n%q", got)
	}
	if res.SeqFrom != mark {
		t.Errorf("SeqFrom = %d, 期望 %d", res.SeqFrom, mark)
	}
	if res.Truncated {
		t.Error("不该报 truncated")
	}
}

// Reap 回收超过保留期的已退出会话。
func TestReap(t *testing.T) {
	m, f := newTestManager(t, Config{ExitedRetention: time.Millisecond})
	s, _ := m.Create(context.Background(), testRequest())
	f.last().exitWith(0)
	waitFor(t, "状态变 exited", func() bool { return s.Status() == StatusExited })

	time.Sleep(5 * time.Millisecond)
	if n := m.Reap(); n != 1 {
		t.Errorf("Reap = %d, 期望 1", n)
	}
	if m.Count() != 0 {
		t.Errorf("回收后 Count = %d, 期望 0", m.Count())
	}
}

// CloseAll 用于 Agent 优雅退出。
func TestCloseAll(t *testing.T) {
	m, f := newTestManager(t, Config{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := m.Create(ctx, testRequest()); err != nil {
			t.Fatal(err)
		}
	}

	if failed := m.CloseAll("agent_shutdown"); failed != 0 {
		t.Errorf("CloseAll 失败数 = %d, 期望 0", failed)
	}
	if m.Count() != 0 {
		t.Errorf("Count = %d, 期望 0", m.Count())
	}
	for i, p := range f.ptys {
		if !p.isClosed() {
			t.Errorf("第 %d 个 PTY 没被关闭", i)
		}
	}
	// 关闭后不能再创建
	if _, err := m.Create(ctx, testRequest()); !errors.Is(err, ErrManagerClosed) {
		t.Errorf("Manager 关闭后创建应报 ErrManagerClosed，实际 %v", err)
	}
}

// Factory 收到的 StartConfig 必须原样携带请求参数。
func TestCreatePassesConfigThrough(t *testing.T) {
	m, f := newTestManager(t, Config{})
	req := testRequest()
	req.Args = []string{"--flag"}
	req.Env = []string{"TERM=xterm-256color"}

	if _, err := m.Create(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	cfg := f.lastCfg
	if cfg.Command != req.Command || cfg.Dir != req.Cwd {
		t.Errorf("Command/Dir 没透传: %+v", cfg)
	}
	if cfg.Cols != 100 || cfg.Rows != 30 {
		t.Errorf("尺寸没透传: %dx%d", cfg.Cols, cfg.Rows)
	}
	if len(cfg.Env) != 1 || cfg.Env[0] != "TERM=xterm-256color" {
		t.Errorf("Env 没透传: %v", cfg.Env)
	}
}

// 信号透传。
func TestSignal(t *testing.T) {
	m, f := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	pty := f.last()

	if err := s.Signal(terminal.SignalInterrupt); err != nil {
		t.Fatal(err)
	}
	pty.mu.Lock()
	n := len(pty.signals)
	pty.mu.Unlock()
	if n != 1 {
		t.Errorf("信号数 = %d, 期望 1", n)
	}
}
