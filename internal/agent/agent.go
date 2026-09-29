package agent

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"github.com/jojo/codegate/internal/terminal"
)

// Version 是 Agent 的版本号，由构建时注入（-ldflags）。
var Version = "dev"

// Agent 是 CodeGate Agent 的运行时。
//
// 它持有三样东西：配置、设备身份、以及**本机全部终端会话**。
// 生命周期上它比任何一条 WebSocket 连接都长 —— 连接断了重连，
// 会话不受影响（这是不变量 I1/I2 在 Agent 侧的体现）。
type Agent struct {
	cfg Config
	id  *Identity
	ws  *Workspace
	mgr *session.Manager
	log *slog.Logger

	startedAt time.Time

	// OnVerifiedUpdate is set by the supervised CLI before Run starts.
	OnVerifiedUpdate func(path, version string) error

	connMu sync.RWMutex
	conn   *Conn

	// views 记录每个会话当前有哪些 view 在 attach。
	//
	// ★ 为什么 Agent 要自己记一份，而不是问 session.Session：
	// Session 内部确实有 views map，但它是私有的；把它暴露出来会让
	// "谁在 attach" 变成两个包共享的状态，不同步时就是难查的 bug。
	// Agent 作为唯一调用方自己记一份，语义更清楚 ——
	// 这份记录只用于 detach 时反查，不参与任何业务判断。
	uploadMu sync.Mutex
	uploads map[string]*fileUpload

	viewMu sync.Mutex
	views  map[uuid.UUID]map[string]struct{}
}

// New 用配置创建 Agent。
//
// 它会读取（或首次生成）设备密钥 —— 这是 Agent 唯一的长期状态。
func New(cfg Config, log *slog.Logger) (*Agent, error) {
	prepared, err := cfg.Prepare()
	if err != nil {
		return nil, err
	}
	id, err := LoadOrCreate(prepared.StateDir)
	if err != nil {
		return nil, err
	}
	return NewWithIdentity(prepared, id, log)
}

// NewWithIdentity 用现成的身份创建 Agent（测试注入用）。
func NewWithIdentity(cfg Config, id *Identity, log *slog.Logger) (*Agent, error) {
	if id == nil {
		return nil, errors.New("agent: 设备身份为空")
	}
	if log == nil {
		log = slog.Default()
	}

	// Factory 是「创建并启动一个 PTY」。
	//
	// 注意这里没有做任何安全检查 —— 命令白名单、cwd 白名单、环境构造
	// 全部在 dispatch 层完成（见 session.CreateRequest 的注释）。
	// Factory 只负责"拿一个已经在跑的 Terminal"，职责单一。
	factory := func(ctx context.Context, startCfg terminal.StartConfig) (terminal.Terminal, error) {
		t := terminal.New()
		if err := t.Start(ctx, startCfg); err != nil {
			return nil, err
		}
		return t, nil
	}

	return &Agent{
		cfg:       cfg,
		id:        id,
		ws:        NewWorkspace(cfg.AllowedRoots),
		mgr:       session.NewManager(factory, session.Config{BufferSize: cfg.BufferSize, MaxSessions: cfg.MaxSessions}),
		log:       log,
		startedAt: time.Now(),
		views:     make(map[uuid.UUID]map[string]struct{}),
		uploads:   make(map[string]*fileUpload),
	}, nil
}

// Config 返回生效后的配置。
func (a *Agent) Config() Config { return a.cfg }

// Identity 返回设备身份（只读用途，比如 doctor 打印 device_id）。
func (a *Agent) Identity() *Identity { return a.id }

// Manager 返回会话管理器（测试与 doctor 用）。
func (a *Agent) Manager() *session.Manager { return a.mgr }

// ---------------------------------------------------------------------------
// 主循环
// ---------------------------------------------------------------------------

// Run 阻塞运行 Agent，直到 ctx 结束。
//
// # 重连语义（§3.2）
//
// 这里是一个「连接 → 认证 → 服务 → 断开 → 退避 → 再来」的循环。
// 两种错误会**终止**整个 Run 而不是重连：
//
//   - 认证失败（4401）：设备密钥不对或已被解绑，重连一万次也一样
//   - 版本不兼容（4426）：需要人工升级 Agent
//
// 其余一律重连 —— 包括 Server 重启、网络抖动、TLS 握手失败。
// 区分这两种情况很重要：前者重连只会把日志刷满并掩盖真正的问题，
// 后者不重连就等于"网络抖一下就要人工介入"。
func (a *Agent) Run(ctx context.Context) error {
	bo := newBackoff(a.cfg.ReconnectMin.Std(), a.cfg.ReconnectMax.Std())

	// 退出前把所有会话收干净：ConPTY 句柄随进程消失，
	// 但显式关闭能让子进程收到正确的终止信号，也避免留下孤儿 conhost。
	defer a.closeUploads()
	defer func() {
		if n := a.mgr.CloseAll("agent_shutdown"); n > 0 {
			a.log.Warn("退出时有会话未能正常关闭", "count", n)
		}
	}()

	// 会话回收定时器：已退出的会话保留一段时间（让用户能回看最后的输出），
	// 超期后回收（§7.5）。
	go a.reapLoop(ctx)
	if a.cfg.UpdateEnabled && Version != "dev" {
		go a.updateLoop(ctx)
	}

	for {
		err := a.runOnce(ctx)

		if ctx.Err() != nil {
			a.log.Info("收到退出信号，Agent 停止")
			return nil
		}
		if err == nil {
			// 正常断开（Server 主动关且给了正常码）。仍然重连 ——
			// Server 重启时通常就是这个形态。
			err = errors.New("连接被对端关闭")
		}
		if IsFatalError(err) {
			a.log.Error("致命错误，不再重连", "err", err)
			return err
		}

		delay := bo.Next()
		a.log.Warn("连接中断，准备重连",
			"err", err, "delay", delay, "attempt", bo.Attempts())

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// runOnce 完成一次完整的「连接 → 认证 → 服务」。
func (a *Agent) runOnce(ctx context.Context) error {
	disp := newDispatcher(a.log)

	// ---- 1. 建连 ----
	dialCtx, cancelDial := context.WithTimeout(ctx, DefaultConnectTimeout)
	conn, err := Dial(dialCtx, DialOptions{
		URL:      a.cfg.ServerURL,
		Insecure: a.cfg.Insecure,
		Log:      a.log,
		OnText:   disp.onText,
		OnBinary: disp.onBinary,
	})
	cancelDial()
	if err != nil {
		return err
	}
	defer conn.Close()

	// ---- 2. 认证 ----
	authCtx, cancelAuth := context.WithTimeout(ctx, DefaultAuthTimeout)
	ready, err := a.authenticate(authCtx, conn, disp)
	cancelAuth()
	if err != nil {
		return err
	}

	// 连上了才算一次成功：重置退避。
	// （调用方看到 nil 以外的错误才会退避，所以这里只能通过"返回 nil 之外的路径"
	//  来表达成功 —— 见 Run 里的处理。）
	if readyFile := os.Getenv("CODEGATE_AGENT_READY_FILE"); readyFile != "" {
		if err := os.WriteFile(readyFile, []byte(Version), 0o600); err != nil { a.log.Warn("写入更新健康标记失败", "err", err) }
	}
	a.log.Info("已连接到 Server",
		"server", a.cfg.ServerURL,
		"device_id", a.id.DeviceID,
		"heartbeat", time.Duration(ready.HeartbeatInterval)*time.Second)

	// ---- 3. 进入服务状态 ----
	a.setConn(conn)
	defer a.clearConn()

	// 会话对账：把本机的会话状态如实上报，让 Server 收敛（§7.4）。
	// Agent 刚重启时这里是空列表 —— 那正是 Server 需要知道的：
	// 它记录的旧会话已经不存在了，必须标成 terminated 而不是
	// 在 UI 上留一个永远 attach 不上的幽灵。
	a.sendSessionSync()

	// 心跳 + 分发切换到运行阶段。
	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()

	hbInterval := DefaultHeartbeatInterval
	if ready.HeartbeatInterval > 0 {
		hbInterval = time.Duration(ready.HeartbeatInterval) * time.Second
	}
	go newHeartbeatTicker(hbInterval, a.log, a.sendHeartbeat).run(hbCtx)

	disp.attach(a.handleMessage, a.handleFrame)

	// ---- 4. 等待连接结束或 ctx 取消 ----
	//
	// ★ 必须同时监听 ctx。只等 conn.Done() 的话，ctx 取消时 Run 会卡在
	// 这里直到连接自然断开 —— 而优雅退出恰恰要求在连接**还活着**的
	// 时候主动停下来（先关会话、再断连），否则 Agent 会被强杀，
	// 子进程收不到正确的终止信号。
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return conn.Err()
	}
}

// reapLoop 周期性回收已退出的会话（§7.5）。
func (a *Agent) reapLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.reapUploads()
			if n := a.mgr.Reap(); n > 0 {
				a.log.Debug("回收已退出的会话", "count", n)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 认证（§8.1）
// ---------------------------------------------------------------------------

// authenticate 执行 hello → challenge → auth → ready 四步握手。
//
// 挑战-应答而不是长期 bearer token：nonce 一次性 + 30s TTL，
// 所以即使有人在网络上录下了整段握手，也无法重放。
// 长期 token 一旦泄露就是永久有效，而它必然会以某种形式落盘或进日志。
func (a *Agent) authenticate(ctx context.Context, conn *Conn, disp *dispatcher) (protocol.AgentReadyPayload, error) {
	var zero protocol.AgentReadyPayload

	platform, arch := DescribePlatform()
	hello, err := protocol.NewEnvelope(protocol.TypeAgentHello, protocol.AgentHelloPayload{
		Protocol:     protocol.Current(),
		DeviceID:     a.id.DeviceID,
		Name:         a.cfg.DeviceName,
		Platform:     platform,
		Arch:         arch,
		AgentVersion: Version,
		Caps: protocol.AgentCaps{
			MaxSessions: a.cfg.MaxSessions,
			ConPTY:      platform == "windows" && terminal.Available() == nil,
			UnixPTY:     platform != "windows" && terminal.Available() == nil,
		},
	})
	if err != nil {
		return zero, err
	}
	if err := sendEnvelope(conn, hello); err != nil {
		return zero, fmt.Errorf("agent: 发送 hello 失败: %w", err)
	}

	// 等 challenge。
	chalEnv, err := disp.waitAuth(ctx, conn, protocol.TypeAgentChallenge)
	if err != nil {
		return zero, fmt.Errorf("agent: 等待 challenge 失败: %w", err)
	}
	chal, err := protocol.DecodePayload[protocol.AgentChallengePayload](chalEnv)
	if err != nil {
		return zero, err
	}

	// 签名。
	signing, err := SigningPayload(chal.Nonce, a.id.DeviceID, chal.ServerTime)
	if err != nil {
		return zero, err
	}
	authEnv, err := protocol.NewEnvelope(protocol.TypeAgentAuth, protocol.AgentAuthPayload{
		Signature: base64.StdEncoding.EncodeToString(a.id.Sign(signing)),
	})
	if err != nil {
		return zero, err
	}
	if err := sendEnvelope(conn, authEnv); err != nil {
		return zero, fmt.Errorf("agent: 发送 auth 失败: %w", err)
	}

	// 等 ready 或 error。
	resp, err := disp.waitAuthAny(ctx, conn, protocol.TypeAgentReady, protocol.TypeError)
	if err != nil {
		return zero, fmt.Errorf("agent: 等待 ready 失败: %w", err)
	}
	if resp.Type == protocol.TypeError {
		ep, _ := protocol.DecodePayload[protocol.ErrorPayload](resp)
		// ★ 包上 ErrAuthRejected：这是「不该重连」的信号。
		// 只返回一个普通 error 会让 Run 无脑重试，而设备密钥不对
		// 或已被解绑时重试一万次结果一样 —— 只会把日志刷满，
		// 并让用户误以为是网络问题。
		return zero, fmt.Errorf("%w（%s）: %s", ErrAuthRejected, ep.Code, ep.Message)
	}

	ready, err := protocol.DecodePayload[protocol.AgentReadyPayload](resp)
	if err != nil {
		return zero, err
	}
	if !protocol.Compatible(ready.Protocol) {
		return zero, fmt.Errorf(
			"agent: Server 声明协议版本 %d-%d，本端支持 %d-%d —— 请升级 Agent 或 Server",
			ready.Protocol.Min, ready.Protocol.Max,
			protocol.MinSupported, protocol.MaxSupported)
	}
	return ready, nil
}

// ---------------------------------------------------------------------------
// dispatcher：认证阶段与运行阶段的消息路由
// ---------------------------------------------------------------------------

// dispatcher 在两种消息处理模式之间切换。
//
// 认证阶段需要一个「同步等待下一条特定消息」的能力，而运行阶段需要
// 把消息交给完整的分发器。用一个可切换的中间层，比让认证逻辑
// 直接读 WebSocket 干净得多 —— 后者会和 readPump 抢同一批字节。
type dispatcher struct {
	log *slog.Logger

	mu     sync.RWMutex
	inbox  chan *protocol.Envelope // 非 nil 表示处于认证/配对阶段
	handle func(*protocol.Envelope)
	frames func([]byte)
}

// newDispatcher 创建分发器，初始处于认证阶段。
//
// ★ inbox 只在创建时建一次，认证全程复用。
//
// 如果每次 waitAuth 都重建 channel，两次等待之间到达的消息会落在
// 旧 channel 上被丢掉 —— 配对流程里 pair.code 和 pair.completed
// 可能挨得很近，症状就是"配对码打印出来了，但永远等不到完成通知"。
// 这类丢消息的 bug 只在消息到达得快时出现，最难复现。
func newDispatcher(log *slog.Logger) *dispatcher {
	return &dispatcher{
		log: log,
		// 容量 8：握手阶段的消息只有 challenge / ready / error /
		// pair.code / pair.completed 这几条，留足余量。
		inbox: make(chan *protocol.Envelope, 8),
	}
}

// attach 切换到运行阶段。
func (d *dispatcher) attach(handle func(*protocol.Envelope), frames func([]byte)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inbox = nil
	d.handle = handle
	d.frames = frames
}

func (d *dispatcher) onText(data []byte) {
	env, err := protocol.Decode(data)
	if err != nil {
		// 解析失败只记日志不断连：一条坏消息不该杀掉整个连接，
		// 而 Decode 已经做了长度/版本/类型校验，畸形报文进不来。
		d.log.Warn("收到无法解析的控制消息", "err", err)
		return
	}

	d.mu.RLock()
	inbox, handle := d.inbox, d.handle
	d.mu.RUnlock()

	if inbox != nil {
		select {
		case inbox <- env:
		default:
			d.log.Warn("认证阶段的消息队列已满，丢弃", "type", env.Type)
		}
		return
	}
	if handle != nil {
		handle(env)
	}
}

// onBinary 处理入站二进制帧。
//
// 认证阶段不会有二进制帧；运行阶段的处理在 dispatch.go。
func (d *dispatcher) onBinary(data []byte) {
	d.mu.RLock()
	inbox, frames := d.inbox, d.frames
	d.mu.RUnlock()

	if inbox != nil {
		// 认证期间收到二进制帧说明对端行为异常，忽略并记录。
		// 不报错：这不值得中断一次即将成功的握手。
		d.log.Warn("认证阶段收到二进制帧，已忽略", "bytes", len(data))
		return
	}
	if frames != nil {
		frames(data)
	}
}

// waitAuth 在握手阶段等待指定类型的消息。
func (d *dispatcher) waitAuth(ctx context.Context, conn *Conn, want protocol.Type) (*protocol.Envelope, error) {
	return d.waitAuthAny(ctx, conn, want)
}

// waitAuthAny 等待任意一个给定类型中先到达的那条。
//
// ★ 必须同时监听 conn.Done()：握手期间对端可能直接断开 ——
// 比如认证被拒后先关连接、或者服务端崩了。只等 ctx 会让调用方
// 干等到超时，症状是「认证失败要等 10 秒才报错」，
// 而实际上连接早就没了。
func (d *dispatcher) waitAuthAny(ctx context.Context, conn *Conn, want ...protocol.Type) (*protocol.Envelope, error) {
	d.mu.RLock()
	inbox := d.inbox
	d.mu.RUnlock()
	if inbox == nil {
		return nil, errors.New("agent: 已进入运行阶段，不能再等待握手消息")
	}

	var done <-chan struct{}
	if conn != nil {
		done = conn.Done()
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			// 连接已断：把真实原因带出去，而不是笼统的"连接关闭"。
			if err := conn.Err(); err != nil {
				return nil, err
			}
			return nil, ErrConnClosed
		case env := <-inbox:
			for _, t := range want {
				if env.Type == t {
					return env, nil
				}
			}
			// 收到非预期的消息：记录后继续等，而不是直接失败 ——
			// Server 可能在握手期间插入一些无关通知。
			d.log.Debug("认证阶段收到非预期消息，继续等待", "got", env.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// 发送辅助
// ---------------------------------------------------------------------------

func sendEnvelope(conn *Conn, env *protocol.Envelope) error {
	data, err := protocol.Encode(env)
	if err != nil {
		return err
	}
	return conn.SendText(data)
}

// setConn / clearConn 维护当前连接。
func (a *Agent) setConn(c *Conn) {
	a.connMu.Lock()
	a.conn = c
	a.connMu.Unlock()
}

func (a *Agent) clearConn() {
	a.connMu.Lock()
	a.conn = nil
	a.connMu.Unlock()
}

// currentConn 返回当前连接，可能为 nil（未连接时）。
func (a *Agent) currentConn() *Conn {
	a.connMu.RLock()
	defer a.connMu.RUnlock()
	return a.conn
}

// sendControl 发送一条控制消息。未连接时返回错误而不是静默丢弃 ——
// 静默丢弃会让"会话创建成功但 UI 没反应"这类问题变得无法定位。
func (a *Agent) sendControl(env *protocol.Envelope) error {
	conn := a.currentConn()
	if conn == nil {
		return ErrConnClosed
	}
	return sendEnvelope(conn, env)
}

// ---------------------------------------------------------------------------
// 会话输出 → 二进制帧（Sink 实现）
// ---------------------------------------------------------------------------

// sessionSink 把某个会话的输出转成二进制帧发给 Server。
//
// # 为什么它是非阻塞的
//
// Sink 的两个方法在 Session 的**锁内**被调用（见 session.Sink 的注释）。
// 一旦阻塞，就会拖住 attach/detach 这些管理操作，进而影响整个会话。
// 所以这里调用 Conn.SendBinary（队列满立即返回 false），
// 由 Session 记账并在后续帧上打 FlagDropped。
type sessionSink struct {
	a   *Agent
	sid uuid.UUID
}

func (s *sessionSink) SendOutput(p []byte, dropped bool) bool {
	var flags uint16
	if dropped {
		flags |= protocol.FlagDropped
	}
	return s.a.sendTerminalFrame(s.sid, protocol.FrameStdout, flags, p)
}

func (s *sessionSink) SendBuffer(p []byte, end bool) bool {
	var flags uint16
	if end {
		flags |= protocol.FlagBufferEnd
	}
	return s.a.sendTerminalFrame(s.sid, protocol.FrameBuffer, flags, p)
}

// sendTerminalFrame 编码并发送一个终端帧。
func (a *Agent) sendTerminalFrame(sid uuid.UUID, typ protocol.FrameType, flags uint16, payload []byte) bool {
	if len(payload) == 0 {
		return true
	}
	conn := a.currentConn()
	if conn == nil {
		return false
	}
	frame, err := protocol.EncodeFrame(typ, flags, sid, payload)
	if err != nil {
		a.log.Error("编码终端帧失败", "err", err, "session", sid)
		return false
	}
	return conn.SendBinary(frame)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// deviceUUID 把 32 字符 hex 的 deviceID 转成 uuid.UUID。
//
// Session 的 DeviceID 字段是 uuid.UUID，而我们的设备 ID 是公钥哈希的
// hex 表示（见 identity.go）。这里做一次转换，让上报出去的
// SessionSummary.DeviceID 是真实值，而不是 uuid.Nil ——
// 否则 Server 侧做权限校验时对不上。
func (a *Agent) deviceUUID() uuid.UUID {
	b, err := hex.DecodeString(a.id.DeviceID)
	if err != nil || len(b) != 16 {
		return uuid.Nil
	}
	u, err := uuid.FromBytes(b)
	if err != nil {
		return uuid.Nil
	}
	return u
}
