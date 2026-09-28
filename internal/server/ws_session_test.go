package server

import (
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/jojo/codegate/internal/protocol"
)

// ---------------------------------------------------------------------------
// 这一组测试补的是「会话结束」这条路径。
//
// 它是端到端测试（scripts/e2e-terminal.sh）抓出来的两个 bug 的回归：
//
//  1. session.close 的**响应**永远到不了客户端 —— 因为
//     onAuthenticated 把 session.closed 无条件交给 handleSessionEnded，
//     而那条路径只广播、不按 reply_to 转发，也不消费 pending 条目。
//     前端表现：点"关闭会话"后一直转圈，直到 60 秒超时才报错。
//
//  2. 会话结束时**其他观察者**收不到通知 —— 因为先 ForgetSession
//     （内部把订阅者名单整个删掉）再广播，遍历到空集合。
//     完全静默：不报错、不记日志。表现：另一个窗口的终端永远停在最后一行。
//
// 两者都只在「多个订阅者 + 有 pending 请求」时暴露，所以单元测试
// 里单开一条连接是看不出来的。
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 辅助：把 Agent 侧演成一个「会照剧本回话」的对端
// ---------------------------------------------------------------------------

// agentRecv 读一条 Server 转发过来的客户端请求。
func agentRecv(t *testing.T, ws *websocket.Conn) *protocol.Envelope {
	t.Helper()
	env := recvEnvelope(t, ws)
	if env.RequestID == "" {
		t.Fatalf("Server 转发给 Agent 的请求没有 request_id（type=%s）—— "+
			"没有它，Agent 的响应就无法被路由回客户端", env.Type)
	}
	return env
}

// agentReply 以 Agent 的身份回一条响应。
//
// ★ 用 protocol.NewReply 而不是手搓信封：它会把 reply_to 和 session_id
// 都从请求里带上 —— 这两个字段正是 Server 路由响应、以及判断
// 「这条消息到底是响应还是自发推送」的全部依据。手搓的话很容易漏掉，
// 而漏掉之后服务端会静默把它当成自发推送处理。
func agentReply(t *testing.T, ws *websocket.Conn, req *protocol.Envelope, typ protocol.Type, payload any) {
	t.Helper()
	env, err := protocol.NewReply(req, typ, payload)
	if err != nil {
		t.Fatalf("构造响应失败: %v", err)
	}
	sendJSON(t, ws, env)
}

// clientWS 开一条浏览器连接（自己换票据）。
func (e *testEnv) clientWS(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	ticket := e.post(t, "/api/v1/ws-ticket", token, nil).Str(t, "ticket")
	ws, _ := e.wsDial(t, "/api/v1/ws/client?ticket="+ticket, nil)
	if ws == nil {
		t.Fatal("浏览器 WS 建连失败")
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// attach 让一条客户端连接 attach 到某会话。Agent 侧由测试扮演。
func (e *testEnv) attach(t *testing.T, client, agent *websocket.Conn, sessionID string) {
	t.Helper()

	req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionAttach, sessionID,
		protocol.SessionAttachPayload{SessionID: sessionID, Cols: 80, Rows: 24})
	sendJSON(t, client, req)

	fwd := agentRecv(t, agent)
	if fwd.Type != protocol.TypeSessionAttach {
		t.Fatalf("Agent 收到 %s，期望 session.attach", fwd.Type)
	}
	agentReply(t, agent, fwd, protocol.TypeSessionAttached, protocol.SessionAttachedPayload{
		Session: protocol.SessionSummary{SessionID: sessionID, Status: "running"},
		Role:    "controller",
	})

	got := recvEnvelope(t, client)
	if got.Type != protocol.TypeSessionAttached {
		t.Fatalf("客户端收到 %s，期望 session.attached", got.Type)
	}
}

// openSession 走完「建会话 → attach」，返回 session_id。
func (e *testEnv) openSession(t *testing.T, client, agent *websocket.Conn, deviceID string) string {
	t.Helper()

	create, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionCreate, "",
		protocol.SessionCreatePayload{
			DeviceID:  deviceID,
			Name:      "shell",
			CommandID: "shell",
			Cwd:       `C:\`,
			Cols:      80,
			Rows:      24,
		})
	sendJSON(t, client, create)

	fwd := agentRecv(t, agent)
	if fwd.Type != protocol.TypeSessionCreate {
		t.Fatalf("Agent 收到的第一条请求是 %s，期望 session.create", fwd.Type)
	}

	sessionID := uuid.NewString()
	agentReply(t, agent, fwd, protocol.TypeSessionCreated, protocol.SessionCreatedPayload{
		Session: protocol.SessionSummary{
			SessionID: sessionID,
			DeviceID:  deviceID,
			Name:      "shell",
			Command:   `C:\Windows\System32\cmd.exe`,
			Cwd:       `C:\`,
			Status:    "running",
			Cols:      80,
			Rows:      24,
			CreatedAt: e.clock.Now().UnixMilli(),
		},
	})

	if got := recvEnvelope(t, client); got.Type != protocol.TypeSessionCreated {
		t.Fatalf("客户端收到 %s，期望 session.created", got.Type)
	}

	e.attach(t, client, agent, sessionID)
	return sessionID
}

// ---------------------------------------------------------------------------
// 回归 1：关闭请求的响应必须回到发起者
// ---------------------------------------------------------------------------

func TestSessionCloseResponseReachesRequester(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID, priv := e.seedPairedDevice(t, userID)

	agent := e.agentHandshake(t, deviceID, priv)
	client := e.clientWS(t, token)
	sessionID := e.openSession(t, client, agent, deviceID)

	closeReq, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionClose, sessionID,
		protocol.SessionClosePayload{SessionID: sessionID})
	sendJSON(t, client, closeReq)

	fwd := agentRecv(t, agent)
	if fwd.Type != protocol.TypeSessionClose {
		t.Fatalf("Agent 收到 %s，期望 session.close", fwd.Type)
	}
	agentReply(t, agent, fwd, protocol.TypeSessionClosed, protocol.SessionClosedPayload{
		SessionID: sessionID,
		Reason:    "user_requested",
	})

	got := recvEnvelope(t, client)
	if got.Type != protocol.TypeSessionClosed {
		t.Fatalf("客户端收到 %s，期望 session.closed —— "+
			"前端会一直停在 loading 直到 pending TTL（60 秒）超时", got.Type)
	}
	if got.ReplyTo != closeReq.RequestID {
		t.Errorf("session.closed.reply_to = %q，期望 %q —— "+
			"前端无法把它对应到那次关闭请求", got.ReplyTo, closeReq.RequestID)
	}

	// pending 条目必须被消费掉。不消费的话它会挂满 60 秒 TTL，
	// 而 sweep 超时时还会给客户端再补发一条「请求超时，请重试」——
	// 用户看到的是「关不掉，还报超时」。
	if n := e.srv.pending.Len(); n != 0 {
		t.Errorf("关闭之后还有 %d 条待回请求没被消费", n)
	}
}

// ---------------------------------------------------------------------------
// 回归 2：会话结束必须广播给其他观察者
// ---------------------------------------------------------------------------

func TestSessionCloseReachesOtherViewers(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID, priv := e.seedPairedDevice(t, userID)

	agent := e.agentHandshake(t, deviceID, priv)

	// 两个「浏览器」看同一个会话。
	closer := e.clientWS(t, token)
	sessionID := e.openSession(t, closer, agent, deviceID)

	viewer := e.clientWS(t, token)
	e.attach(t, viewer, agent, sessionID)

	// 前置条件：两条连接都订阅上了。不成立的话后面的断言毫无意义 ——
	// 而「广播发给谁」正是本测试要验的东西。
	if n := e.srv.Registry().SubscriberCount(sessionID); n != 2 {
		t.Fatalf("会话订阅者 = %d，期望 2", n)
	}

	closeReq, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionClose, sessionID,
		protocol.SessionClosePayload{SessionID: sessionID})
	sendJSON(t, closer, closeReq)

	fwd := agentRecv(t, agent)
	if fwd.Type != protocol.TypeSessionClose {
		t.Fatalf("Agent 收到 %s，期望 session.close", fwd.Type)
	}
	agentReply(t, agent, fwd, protocol.TypeSessionClosed, protocol.SessionClosedPayload{
		SessionID: sessionID,
		Reason:    "user_requested",
	})

	// 发起者：收到的是「响应」（routeToClient 按 reply_to 转回）。
	if got := recvEnvelope(t, closer); got.Type != protocol.TypeSessionClosed {
		t.Fatalf("发起者收到 %s，期望 session.closed", got.Type)
	}

	// ★ 另一个观察者：收到的是「广播」。这一条才是本测试的重点 ——
	// 它在 ForgetSession 早于 BroadcastToSession 的实现下会永远收不到。
	if got := recvEnvelope(t, viewer); got.Type != protocol.TypeSessionClosed {
		t.Fatalf("另一个观察者收到 %s，期望 session.closed —— "+
			"它的界面会永远停在一个已经死掉的终端上", got.Type)
	}

	// 收尾：订阅关系必须被清干净，否则会话广播会往不存在的会话投递。
	if n := e.srv.Registry().SubscriberCount(sessionID); n != 0 {
		t.Errorf("会话结束后还剩 %d 个订阅者没被清理", n)
	}
}

// ---------------------------------------------------------------------------
// 自发推送（没有对应请求）仍然走广播路径
// ---------------------------------------------------------------------------

// TestSpontaneousSessionExitStillBroadcasts 保证修复没有把自发推送弄丢。
//
// session.exit 是 Agent 主动推的：进程自己退出了、PTY 出错了。
// 它**没有**对应的客户端请求，所以 pending 里查不到 reply_to ——
// 必须原样走广播，而不是被「找不到待回请求」吞掉。
func TestSpontaneousSessionExitStillBroadcasts(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID, priv := e.seedPairedDevice(t, userID)

	agent := e.agentHandshake(t, deviceID, priv)
	client := e.clientWS(t, token)
	sessionID := e.openSession(t, client, agent, deviceID)

	// 一个没有 reply_to 的自发推送。
	push, _ := protocol.NewEnvelope(protocol.TypeSessionExit, protocol.SessionExitPayload{
		SessionID: sessionID,
		ExitCode:  0,
		Reason:    "exited",
	})
	push.SessionID = sessionID
	push.ReplyTo = uuid.NewString() // 一个谁也没发过的 request_id
	sendJSON(t, agent, push)

	got := recvEnvelope(t, client)
	if got.Type != protocol.TypeSessionExit {
		t.Fatalf("客户端收到 %s，期望 session.exit", got.Type)
	}
}
