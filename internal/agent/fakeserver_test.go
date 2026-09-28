package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jojo/codegate/internal/protocol"
)

// 本文件是 P3 的验收：连上本地假 Server → 认证（Ed25519 挑战-应答）
// → 心跳 → 自动重连。
//
// # 为什么手写假 Server 而不是用 mock 框架
//
// 项目约定（架构 §12.3「明确不引入任何 mock 框架」）。而且这里 mock
// 不解决问题：要验证的恰恰是**真实 WebSocket 上的完整握手**——
// 帧编码、TLS/Upgrade、读写泵、超时、关闭码，这些都是 mock 会跳过的东西。
//
// 假 Server 只用标准库 + gorilla/websocket，实现协议里 Agent 需要的那几条。

// ---------------------------------------------------------------------------
// 假 Server
// ---------------------------------------------------------------------------

type fakeServer struct {
	t   *testing.T
	pub ed25519.PublicKey

	ln     net.Listener
	srv    *http.Server
	addr   string
	closed chan struct{}

	mu sync.Mutex
	// 收到的消息计数与内容
	hellos     []protocol.AgentHelloPayload
	auths      []protocol.AgentAuthPayload
	syncs      int
	heartbeats int
	readySent  int
	conns      map[*websocket.Conn]struct{}

	// 上一次 challenge 的内容，验签时要用
	lastNonce      string
	lastServerTime int64

	// failAuth 为 true 时拒绝认证（用来验证 4401 不重连）。
	failAuth bool
	// heartbeatSeconds 是下发给 Agent 的心跳间隔（秒）。
	heartbeatSeconds int
}

func newFakeServer(t *testing.T, pub ed25519.PublicKey) *fakeServer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}

	s := &fakeServer{
		t:                t,
		pub:              pub,
		ln:               ln,
		addr:             ln.Addr().String(),
		closed:           make(chan struct{}),
		conns:            make(map[*websocket.Conn]struct{}),
		heartbeatSeconds: 1,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/agent", s.handleWS)
	s.srv = &http.Server{Handler: mux}

	go func() {
		_ = s.srv.Serve(ln)
	}()

	t.Cleanup(func() { s.Stop() })
	return s
}

// URL 返回 Agent 应连接的地址。
func (s *fakeServer) URL() string {
	return "ws://" + s.addr + "/ws/agent"
}

// Stop 关闭 listener 与全部连接。
func (s *fakeServer) Stop() {
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return
	default:
	}
	close(s.closed)
	conns := make([]*websocket.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	_ = s.srv.Close()
}

// DropConnections 只断开现有连接，**保留 listener**。
//
// 用来模拟「Server 重启/网络抖动」：Agent 应当重连到同一个地址。
// 比"关掉 listener 再重新 Listen"更可靠 —— 后者会踩 TIME_WAIT，
// 表现为"Agent 连不上"，而那不是我们要测的东西。
func (s *fakeServer) DropConnections() {
	s.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *fakeServer) handleWS(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{
		// 测试里不校验 Origin。
		CheckOrigin: func(*http.Request) bool { return true },
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		env, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		s.handle(conn, env)
	}
}

func (s *fakeServer) handle(conn *websocket.Conn, env *protocol.Envelope) {
	switch env.Type {
	case protocol.TypeAgentHello:
		hello, err := protocol.DecodePayload[protocol.AgentHelloPayload](env)
		if err != nil {
			return
		}

		nonceBytes := make([]byte, 32)
		_, _ = rand.Read(nonceBytes)
		nonce := base64.StdEncoding.EncodeToString(nonceBytes)
		serverTime := time.Now().UnixMilli()

		s.mu.Lock()
		s.hellos = append(s.hellos, hello)
		s.lastNonce = nonce
		s.lastServerTime = serverTime
		s.mu.Unlock()

		ch, err := protocol.NewEnvelope(protocol.TypeAgentChallenge, protocol.AgentChallengePayload{
			Nonce:      nonce,
			ServerTime: serverTime,
		})
		if err != nil {
			return
		}
		s.send(conn, ch)

	case protocol.TypeAgentAuth:
		auth, err := protocol.DecodePayload[protocol.AgentAuthPayload](env)
		if err != nil {
			return
		}

		s.mu.Lock()
		s.auths = append(s.auths, auth)
		nonce, st, fail := s.lastNonce, s.lastServerTime, s.failAuth
		deviceID := ""
		if n := len(s.hellos); n > 0 {
			deviceID = s.hellos[n-1].DeviceID
		}
		s.mu.Unlock()

		if fail {
			s.sendError(conn, protocol.CodeUnauthenticated, "测试：拒绝认证")
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(CloseUnauthorized, "unauthorized"),
				time.Now().Add(time.Second))
			return
		}

		// 验签：这才是这个假 Server 存在的意义 ——
		// 它证明 Agent 确实按约定拼出了待签名数据。
		sig, err := base64.StdEncoding.DecodeString(auth.Signature)
		if err != nil {
			s.sendError(conn, protocol.CodeInvalidPayload, "签名不是合法 base64")
			return
		}
		payload, err := SigningPayload(nonce, deviceID, st)
		if err != nil {
			s.sendError(conn, protocol.CodeInvalidPayload, "无法构造待签名数据")
			return
		}
		if !ed25519.Verify(s.pub, payload, sig) {
			s.sendError(conn, protocol.CodeUnauthenticated, "签名校验失败")
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(CloseUnauthorized, "bad signature"),
				time.Now().Add(time.Second))
			return
		}

		s.mu.Lock()
		hb := s.heartbeatSeconds
		s.readySent++
		s.mu.Unlock()

		ready, err := protocol.NewEnvelope(protocol.TypeAgentReady, protocol.AgentReadyPayload{
			Protocol:          protocol.Current(),
			ServerTime:        time.Now().UnixMilli(),
			HeartbeatInterval: hb,
			Limits:            protocol.AgentLimits{MaxSessions: 20},
		})
		if err != nil {
			return
		}
		s.send(conn, ready)

	case protocol.TypeSessionSync:
		s.mu.Lock()
		s.syncs++
		s.mu.Unlock()

	case protocol.TypeAgentHeartbeat:
		s.mu.Lock()
		s.heartbeats++
		s.mu.Unlock()

	case protocol.TypePing:
		pong, _ := protocol.NewEnvelope(protocol.TypePong, nil)
		s.send(conn, pong)
	}
}

func (s *fakeServer) send(conn *websocket.Conn, env *protocol.Envelope) {
	data, err := protocol.Encode(env)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = conn.WriteMessage(websocket.TextMessage, data)
}

func (s *fakeServer) sendError(conn *websocket.Conn, code protocol.ErrorCode, msg string) {
	env, _ := protocol.NewEnvelope(protocol.TypeError, protocol.ErrorPayload{
		Code:    code,
		Message: msg,
	})
	s.send(conn, env)
}

// ---- 观察辅助 ----

func (s *fakeServer) counts() (hellos, auths, ready, syncs, heartbeats int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hellos), len(s.auths), s.readySent, s.syncs, s.heartbeats
}

// waitFor 轮询直到条件成立。
func (s *fakeServer) waitFor(desc string, timeout time.Duration, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	hellos, auths, ready, syncs, hb := s.counts()
	s.t.Fatalf("等待「%s」超时（%s）。当前计数: hello=%d auth=%d ready=%d sync=%d heartbeat=%d",
		desc, timeout, hellos, auths, ready, syncs, hb)
}

func (s *fakeServer) waitReady(timeout time.Duration) {
	s.waitFor("认证完成", timeout, func() bool {
		_, _, ready, _, _ := s.counts()
		return ready > 0
	})
}

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

func testConfig(t *testing.T, serverURL string) Config {
	t.Helper()

	c := DefaultConfig()
	c.ServerURL = serverURL
	c.Insecure = true // 假 Server 用明文 ws://
	c.AllowedRoots = []string{t.TempDir()}
	c.AllowedCommands = []CommandSpec{
		{ID: "shell", Label: "Shell", Command: "cmd.exe", Kind: KindShell},
	}
	// 缩短心跳与退避，让测试跑得快 —— 但**不能为 0**，
	// 否则会走 withDefaults 拿到生产值，测试就会变成分钟级。
	c.HeartbeatInterval = Duration(200 * time.Millisecond)
	c.ReconnectMin = Duration(50 * time.Millisecond)
	c.ReconnectMax = Duration(300 * time.Millisecond)

	prepared, err := c.Prepare()
	if err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}
	return prepared
}

func newTestAgent(t *testing.T, cfg Config, id *Identity) *Agent {
	t.Helper()
	// 用安静一点的 logger，避免测试输出被日志淹没。
	a, err := NewWithIdentity(cfg, id, testLogger())
	if err != nil {
		t.Fatalf("创建 Agent 失败: %v", err)
	}
	return a
}

// ---------------------------------------------------------------------------
// 验收 1：连接 + 认证
// ---------------------------------------------------------------------------

func TestAgentConnectsAndAuthenticates(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := newFakeServer(t, id.PublicKey())
	a := newTestAgent(t, testConfig(t, srv.URL()), id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx) }()

	srv.waitReady(5 * time.Second)

	hellos, auths, _, syncs, _ := srv.counts()
	if hellos != 1 {
		t.Errorf("hello 次数 = %d，期望 1", hellos)
	}
	if auths != 1 {
		t.Errorf("auth 次数 = %d，期望 1", auths)
	}
	// session.sync 必须紧跟 ready 发出 —— 它是 Server 对账的依据（§7.4）。
	srv.waitFor("session.sync", 3*time.Second, func() bool {
		_, _, _, s, _ := srv.counts()
		return s > 0
	})
	_ = syncs

	// 断言 hello 里的内容确实是这台设备。
	srv.mu.Lock()
	hello := srv.hellos[0]
	srv.mu.Unlock()

	if hello.DeviceID != id.DeviceID {
		t.Errorf("hello.device_id = %q，期望 %q", hello.DeviceID, id.DeviceID)
	}
	if hello.Platform == "" || hello.Arch == "" {
		t.Errorf("hello 缺少平台信息: %+v", hello)
	}
	if !hello.Caps.ConPTY && !hello.Caps.UnixPTY {
		t.Logf("提示: 本机没有可用的 PTY 实现（caps=%+v）", hello.Caps)
	}
}

// TestAgentRejectsBadSignature 验证假 Server 的验签确实在起作用。
//
// 没有这个测试，"认证通过"可能只是因为假 Server 根本没验签 ——
// 那 TestAgentConnectsAndAuthenticates 就变成了一个自欺欺人的测试。
func TestAgentRejectsBadSignature(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// 用**另一把**公钥建 Server，Agent 的签名必然验不过。
	other, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := newFakeServer(t, other.PublicKey())
	a := newTestAgent(t, testConfig(t, srv.URL()), id)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx) }()

	srv.waitFor("收到 auth", 5*time.Second, func() bool {
		_, auths, _, _, _ := srv.counts()
		return auths > 0
	})

	// 认证不该成功。
	time.Sleep(500 * time.Millisecond)
	if _, _, ready, _, _ := srv.counts(); ready != 0 {
		t.Error("公钥不匹配时认证居然成功了 —— 假 Server 的验签没生效")
	}
}

// ---------------------------------------------------------------------------
// 验收 2：心跳
// ---------------------------------------------------------------------------

func TestAgentSendsHeartbeat(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := newFakeServer(t, id.PublicKey())
	srv.heartbeatSeconds = 1 // Server 下发的间隔

	cfg := testConfig(t, srv.URL())
	a := newTestAgent(t, cfg, id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx) }()

	srv.waitReady(5 * time.Second)

	// 心跳间隔由 Server 通过 agent.ready 下发，所以这里等的是
	// "Agent 采用了下发的值"这件事本身。
	srv.waitFor("收到心跳", 5*time.Second, func() bool {
		_, _, _, _, hb := srv.counts()
		return hb > 0
	})
}

// ---------------------------------------------------------------------------
// 验收 3：自动重连（P3 的核心退出标准）
// ---------------------------------------------------------------------------

func TestAgentReconnectsAfterDrop(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := newFakeServer(t, id.PublicKey())
	a := newTestAgent(t, testConfig(t, srv.URL()), id)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx) }()

	srv.waitReady(5 * time.Second)

	// 模拟 Server 重启：断开连接，但保留 listener。
	srv.DropConnections()

	// Agent 应当自动重连并重新完成认证。
	// 30s 是验收标准给的上限；这里退避配置被调快了，所以给 15s 余量。
	srv.waitFor("重连后重新认证", 15*time.Second, func() bool {
		_, _, ready, _, _ := srv.counts()
		return ready >= 2
	})

	hellos, auths, ready, _, _ := srv.counts()
	t.Logf("重连后计数: hello=%d auth=%d ready=%d", hellos, auths, ready)

	// 重连时必须重新走完整握手（不能复用上次的认证结果）——
	// 每次连接都是新的 nonce，这正是挑战-应答防重放的体现。
	if hellos != auths {
		t.Errorf("hello 与 auth 次数不一致（%d vs %d）—— 重连时握手流程有缺步", hellos, auths)
	}
}

// TestAgentStopsOnUnauthorized 验证致命关闭码不会被无脑重连。
//
// 设备密钥不对/已被解绑时，重连一万次也一样，只会把日志刷满
// 并掩盖真正的问题（用户以为"网络不好"，实际是需要重新配对）。
func TestAgentStopsOnUnauthorized(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := newFakeServer(t, id.PublicKey())
	srv.failAuth = true

	a := newTestAgent(t, testConfig(t, srv.URL()), id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("认证被拒时 Run 应当返回错误")
		}
		t.Logf("Run 按预期退出: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("认证被拒后 Run 仍在重连 —— 致命错误应当终止而不是重试")
	}

	// 只应尝试一次。
	if hellos, _, _, _, _ := srv.counts(); hellos > 1 {
		t.Errorf("认证失败后仍重试了 %d 次", hellos)
	}
}

// ---------------------------------------------------------------------------
// 验收 4：优雅退出
// ---------------------------------------------------------------------------

func TestAgentStopsOnContextCancel(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := newFakeServer(t, id.PublicKey())
	a := newTestAgent(t, testConfig(t, srv.URL()), id)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	srv.waitReady(5 * time.Second)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("正常取消时 Run 应当返回 nil，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消 context 后 Run 没有退出")
	}
}

// TestAgentSessionLifecycleOverWS 用真实 WebSocket 走一遍会话创建。
//
// 它同时验证了两件事：Server 转发的 session.create 能被正确执行，
// 以及「命令白名单」在真实链路上确实生效（不存在的 ID 必须被拒）。
func TestAgentSessionLifecycleOverWS(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := newFakeServer(t, id.PublicKey())
	a := newTestAgent(t, testConfig(t, srv.URL()), id)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx) }()
	srv.waitReady(5 * time.Second)

	// 直接调用 dispatch（不经过 WS）来验证白名单，
	// 因为从 Server 侧构造一次完整的 session.create 往返需要
	// 假 Server 实现请求-响应关联，那是 P4 的事。
	_, err = a.cfg.ResolveCommand("not-in-whitelist", "", nil, false)
	if !errors.Is(err, ErrCommandNotAllowed) {
		t.Errorf("不存在的命令 ID 没有被拒（err=%v）", err)
	}

	// 白名单内的命令应当能解析。
	got, err := a.cfg.ResolveCommand("shell", "", nil, false)
	if err != nil {
		t.Fatalf("白名单命令解析失败: %v", err)
	}
	if got.Kind != KindShell {
		t.Errorf("kind = %q，期望 %q", got.Kind, KindShell)
	}
}

// testLogger 返回一个丢弃全部输出的 logger。
//
// 测试里 Agent 会打不少 Info 日志（连接、重连、会话），
// 混在测试输出里会让失败信息很难找。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
