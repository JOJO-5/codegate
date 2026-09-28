package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

// ---------------------------------------------------------------------------
// WS 客户端辅助
// ---------------------------------------------------------------------------

// wsDial 建立一条 WebSocket 连接。返回的关闭码在连接结束后由 closeCode 读。
func (e *testEnv) wsDial(t *testing.T, path string, header http.Header) (*websocket.Conn, *http.Response) {
	t.Helper()

	url := "ws" + strings.TrimPrefix(e.http.URL, "http") + path
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, resp, err := dialer.Dial(url, header)
	if err != nil {
		// 握手被拒是**合法结果**（比如票据无效时应当在升级前返回 401），
		// 所以这里不 Fatal —— 由调用方看 resp 决定。
		return nil, resp
	}
	return ws, resp
}

// sendJSON 发一条控制消息。
func sendJSON(t *testing.T, ws *websocket.Conn, env *protocol.Envelope) {
	t.Helper()
	data, err := protocol.Encode(env)
	if err != nil {
		t.Fatalf("编码消息失败: %v", err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("发送消息失败: %v", err)
	}
}

// recvEnvelope 读一条控制消息（带超时）。
func recvEnvelope(t *testing.T, ws *websocket.Conn) *protocol.Envelope {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("期望 Text 帧，得到 %v", mt)
	}
	env, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("解码消息失败: %v（原始: %s）", err, data)
	}
	return env
}

// expectCloseCode 读关闭帧并断言关闭码。
func expectCloseCode(t *testing.T, ws *websocket.Conn, want int) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				if ce.Code != want {
					t.Fatalf("关闭码 = %d（%s），期望 %d", ce.Code, ce.Text, want)
				}
				return
			}
			t.Fatalf("连接在没有关闭帧的情况下断了: %v（期望关闭码 %d）", err, want)
		}
		// 关闭帧之前可能还有一条 error 消息（比如 4426 之前会先回 error），
		// 那是有意为之 —— 只靠关闭码用户看不到「请升级」这句话。
		_ = mt
		_ = data
	}
}

// helloEnvelope 构造一条合法的 agent.hello。
func helloEnvelope(deviceID string) *protocol.Envelope {
	env, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentHello, "",
		protocol.AgentHelloPayload{
			Protocol:     protocol.Current(),
			DeviceID:     deviceID,
			Name:         "test-pc",
			Platform:     "windows",
			Arch:         "amd64",
			AgentVersion: "0.1.0-test",
			Caps:         protocol.AgentCaps{MaxSessions: 20, ConPTY: true},
		})
	return env
}

// seedPairedDevice 塞一台**带真实密钥对**的已绑定设备，返回设备 ID 与私钥。
//
// ★ 必须用真密钥：认证路径就是 Ed25519 验签，用一个假公钥
// 只会让测试停在「签名不合法」上，永远走不到认证成功之后的分支。
func (e *testEnv) seedPairedDevice(t *testing.T, userID string) (string, ed25519.PrivateKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}

	deviceID := "agent-" + uuid.NewString()
	now := e.clock.Now()

	if err := e.store.CreateDevice(t.Context(), &storage.Device{
		ID:           deviceID,
		Name:         "test-pc",
		Platform:     "windows",
		Arch:         "amd64",
		AgentVersion: "0.1.0-test",
		PublicKey:    pub,
		CreatedAt:    now,
	}); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	if err := e.store.BindDevice(t.Context(), deviceID, userID, now); err != nil {
		t.Fatalf("绑定设备失败: %v", err)
	}
	return deviceID, priv
}

// agentHandshake 跑完 hello → challenge → auth → ready，返回连接。
func (e *testEnv) agentHandshake(t *testing.T, deviceID string, priv ed25519.PrivateKey) *websocket.Conn {
	t.Helper()

	ws, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if ws == nil {
		t.Fatal("Agent WS 连接失败")
	}
	t.Cleanup(func() { _ = ws.Close() })

	sendJSON(t, ws, helloEnvelope(deviceID))

	challenge := recvEnvelope(t, ws)
	if challenge.Type != protocol.TypeAgentChallenge {
		t.Fatalf("期望 agent.challenge，得到 %s（payload=%s）", challenge.Type, challenge.Payload)
	}
	cp, err := protocol.DecodePayload[protocol.AgentChallengePayload](challenge)
	if err != nil {
		t.Fatalf("解析 challenge 失败: %v", err)
	}

	payload, err := protocol.SigningPayload(cp.Nonce, deviceID, cp.ServerTime)
	if err != nil {
		t.Fatalf("构造签名对象失败: %v", err)
	}
	sig := ed25519.Sign(priv, payload)

	auth, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentAuth, "",
		protocol.AgentAuthPayload{Signature: base64.StdEncoding.EncodeToString(sig)})
	sendJSON(t, ws, auth)

	ready := recvEnvelope(t, ws)
	if ready.Type != protocol.TypeAgentReady {
		t.Fatalf("期望 agent.ready，得到 %s（payload=%s）", ready.Type, ready.Payload)
	}
	return ws
}

// ---------------------------------------------------------------------------
// Agent WS：握手与认证
// ---------------------------------------------------------------------------

func TestAgentWSFullHandshake(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)

	deviceID, priv := e.seedPairedDevice(t, userID)
	ws := e.agentHandshake(t, deviceID, priv)

	// 认证完成后设备必须出现在注册表里 —— 这是「在线」的唯一真相源。
	agent, ok := e.srv.Registry().Agent(deviceID)
	if !ok {
		t.Fatal("认证成功后设备没有进入注册表 —— 它会一直显示离线")
	}
	if agent.UserID != userID {
		t.Fatalf("注册表里的 owner = %s，期望 %s", agent.UserID, userID)
	}

	// 心跳：Agent 侧会周期发 ping，服务端必须回 pong。
	ping, _ := protocol.NewRequest(uuid.NewString(), protocol.TypePing, "", nil)
	sendJSON(t, ws, ping)

	pong := recvEnvelope(t, ws)
	if pong.Type != protocol.TypePong {
		t.Fatalf("期望 pong，得到 %s", pong.Type)
	}
}

func TestAgentWSRejectsWrongSignature(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)

	deviceID, _ := e.seedPairedDevice(t, userID)
	// ★ 用另一把私钥签名 —— 模拟「拿到了 device_id 但没有私钥」。
	_, attackerKey, _ := ed25519.GenerateKey(rand.Reader)

	ws, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if ws == nil {
		t.Fatal("Agent WS 连接失败")
	}
	defer ws.Close()

	sendJSON(t, ws, helloEnvelope(deviceID))
	challenge := recvEnvelope(t, ws)
	cp, _ := protocol.DecodePayload[protocol.AgentChallengePayload](challenge)

	payload, _ := protocol.SigningPayload(cp.Nonce, deviceID, cp.ServerTime)
	sig := ed25519.Sign(attackerKey, payload)

	auth, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentAuth, "",
		protocol.AgentAuthPayload{Signature: base64.StdEncoding.EncodeToString(sig)})
	sendJSON(t, ws, auth)

	// 必须用 4401 关闭 —— Agent 侧把它判成「不可重连，需重新配对」。
	// 用 1013 之类的可重连码会让它在错误的密钥上无限重试。
	expectCloseCode(t, ws, CloseUnauthorized)

	if _, online := e.srv.Registry().Agent(deviceID); online {
		t.Fatal("验签失败的连接竟然进了注册表")
	}
}

func TestAgentWSRejectsUnpairedDevice(t *testing.T) {
	e := newTestEnv(t)

	// 一台从未注册过的设备。
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ghost := "agent-ghost-" + uuid.NewString()

	ws, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if ws == nil {
		t.Fatal("Agent WS 连接失败")
	}
	defer ws.Close()

	sendJSON(t, ws, helloEnvelope(ghost))
	challenge := recvEnvelope(t, ws)
	cp, _ := protocol.DecodePayload[protocol.AgentChallengePayload](challenge)

	payload, _ := protocol.SigningPayload(cp.Nonce, ghost, cp.ServerTime)
	auth, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentAuth, "",
		protocol.AgentAuthPayload{
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)),
		})
	sendJSON(t, ws, auth)

	expectCloseCode(t, ws, CloseUnauthorized)
}

// TestAgentWSRejectsRuntimeMessageBeforeAuth 验证「未认证的连接
// 不能发运行期消息」。
//
// 少了这道闸，未认证的连接就能发 session.create、agent.heartbeat 之类，
// 服务端在还不知道它是谁的情况下就会去动状态机 ——
// 伪造「我的设备上线了」「我有个会话」这类假象。
func TestAgentWSRejectsRuntimeMessageBeforeAuth(t *testing.T) {
	e := newTestEnv(t)

	ws, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if ws == nil {
		t.Fatal("Agent WS 连接失败")
	}
	defer ws.Close()

	// 直接发心跳，跳过 hello。
	hb, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentHeartbeat, "",
		protocol.HeartbeatPayload{})
	sendJSON(t, ws, hb)

	expectCloseCode(t, ws, CloseUnauthorized)
}

func TestAgentWSRejectsIncompatibleVersion(t *testing.T) {
	e := newTestEnv(t)

	ws, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if ws == nil {
		t.Fatal("Agent WS 连接失败")
	}
	defer ws.Close()

	env := helloEnvelope("agent-whatever")
	// 声明一个本端完全不支持的范围（min/max 都远超当前）。
	bad, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentHello, "",
		protocol.AgentHelloPayload{
			Protocol: protocol.VersionInfo{Min: 99, Max: 100},
			DeviceID: "agent-whatever",
		})
	sendJSON(t, ws, bad)
	_ = env

	// 4426：让客户端去升级，而不是无限重连。
	expectCloseCode(t, ws, CloseVersionUnsupported)
}

// TestAgentWSAuthIsBoundToChallenge 验证挑战-应答的**防重放**属性。
//
// 攻击者的模型：录下一次完整握手（nonce + 签名 + server_time），
// 之后原样重发。如果签名不绑定「这一次连接的这一个 nonce」，
// 那段录音就能反复用来冒充这台设备 —— 而这不需要拿到私钥。
//
// 这里的具体做法是：在连接 1 上拿到 nonce1 并算出签名，
// 但把它发到连接 2 上（连接 2 有自己的 nonce2）。必须被拒。
func TestAgentWSAuthIsBoundToChallenge(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)

	deviceID, priv := e.seedPairedDevice(t, userID)

	// ---- 连接 1：拿到 nonce1，算签名，然后断开 ----
	ws1, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if ws1 == nil {
		t.Fatal("第一条 Agent 连接失败")
	}
	sendJSON(t, ws1, helloEnvelope(deviceID))
	cp1, _ := protocol.DecodePayload[protocol.AgentChallengePayload](recvEnvelope(t, ws1))
	ws1.Close()

	payload1, _ := protocol.SigningPayload(cp1.Nonce, deviceID, cp1.ServerTime)
	recorded := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload1))

	// ---- 连接 2：用自己的 nonce2，但发送录制下来的签名 ----
	ws2, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if ws2 == nil {
		t.Fatal("第二条 Agent 连接失败")
	}
	defer ws2.Close()

	sendJSON(t, ws2, helloEnvelope(deviceID))
	cp2, _ := protocol.DecodePayload[protocol.AgentChallengePayload](recvEnvelope(t, ws2))
	if cp2.Nonce == cp1.Nonce {
		t.Fatal("两次握手拿到了同一个 nonce —— 挑战没有随机性，重放必然成立")
	}

	replay, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentAuth, "",
		protocol.AgentAuthPayload{Signature: recorded})
	sendJSON(t, ws2, replay)

	expectCloseCode(t, ws2, CloseUnauthorized)

	if _, online := e.srv.Registry().Agent(deviceID); online {
		t.Fatal("重放的握手竟然让设备上线了")
	}
}

// ---------------------------------------------------------------------------
// Agent WS：连接顶替（§8.1）
// ---------------------------------------------------------------------------

func TestAgentWSNewConnectionEvictsOld(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)

	deviceID, priv := e.seedPairedDevice(t, userID)

	first := e.agentHandshake(t, deviceID, priv)
	// 等一下让第一条连接完成注册，避免两条连接在注册表里赛跑。
	waitFor(t, func() bool {
		_, ok := e.srv.Registry().Agent(deviceID)
		return ok
	})

	// 第二条连接（模拟 Agent 重启或切网，旧连接还没超时）。
	second, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if second == nil {
		t.Fatal("第二条 Agent 连接失败")
	}
	defer second.Close()

	sendJSON(t, second, helloEnvelope(deviceID))
	challenge := recvEnvelope(t, second)
	cp, _ := protocol.DecodePayload[protocol.AgentChallengePayload](challenge)
	payload, _ := protocol.SigningPayload(cp.Nonce, deviceID, cp.ServerTime)

	auth, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentAuth, "",
		protocol.AgentAuthPayload{
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)),
		})
	sendJSON(t, second, auth)

	if ready := recvEnvelope(t, second); ready.Type != protocol.TypeAgentReady {
		t.Fatalf("第二条连接应认证成功，得到 %s", ready.Type)
	}

	// ★ 旧连接必须被踢，关闭码 4409。
	//
	// 策略是「新连接顶掉旧连接」而不是「拒绝新连接」：
	// 后者会让设备在旧连接自然超时前（最长 60 秒）完全不可用。
	expectCloseCode(t, first, CloseDuplicateConnection)

	// 注册表里应当是**新**连接。
	waitFor(t, func() bool {
		cur, ok := e.srv.Registry().Agent(deviceID)
		return ok && cur != nil
	})
}

// ---------------------------------------------------------------------------
// Client WS
// ---------------------------------------------------------------------------

func TestClientWSRequiresValidTicket(t *testing.T) {
	e := newTestEnv(t)

	// 无票据。
	ws, resp := e.wsDial(t, "/api/v1/ws/client", nil)
	if ws != nil {
		ws.Close()
		t.Fatal("无票据竟然建连成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		got := 0
		if resp != nil {
			got = resp.StatusCode
		}
		t.Fatalf("无票据应返回 401，得到 %d", got)
	}

	// 伪造票据。
	ws2, resp2 := e.wsDial(t, "/api/v1/ws/client?ticket=made-up", nil)
	if ws2 != nil {
		ws2.Close()
		t.Fatal("伪造票据竟然建连成功")
	}
	if resp2 == nil || resp2.StatusCode != http.StatusUnauthorized {
		got := 0
		if resp2 != nil {
			got = resp2.StatusCode
		}
		t.Fatalf("伪造票据应返回 401，得到 %d", got)
	}
}

func TestClientWSRejectsTicketReuse(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	ticket := e.post(t, "/api/v1/ws-ticket", token, nil).Str(t, "ticket")

	first, _ := e.wsDial(t, "/api/v1/ws/client?ticket="+ticket, nil)
	if first == nil {
		t.Fatal("有效票据应该能建连")
	}
	defer first.Close()

	// ★ 票据用完即焚。
	//
	// 票据出现在 URL 里（浏览器的 WebSocket API 不支持自定义头），
	// 而 URL 会进服务器访问日志、Referer、浏览器历史。
	// 单次使用让「从日志里读到票据」这件事变得无用。
	second, resp := e.wsDial(t, "/api/v1/ws/client?ticket="+ticket, nil)
	if second != nil {
		second.Close()
		t.Fatal("票据可以被重复使用")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		got := 0
		if resp != nil {
			got = resp.StatusCode
		}
		t.Fatalf("重复使用票据应返回 401，得到 %d", got)
	}
}

func TestClientWSPingPong(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	ticket := e.post(t, "/api/v1/ws-ticket", token, nil).Str(t, "ticket")

	ws, _ := e.wsDial(t, "/api/v1/ws/client?ticket="+ticket, nil)
	if ws == nil {
		t.Fatal("浏览器 WS 建连失败")
	}
	defer ws.Close()

	ping, _ := protocol.NewRequest(uuid.NewString(), protocol.TypePing, "", nil)
	sendJSON(t, ws, ping)

	pong := recvEnvelope(t, ws)
	if pong.Type != protocol.TypePong {
		t.Fatalf("期望 pong，得到 %s", pong.Type)
	}
	if pong.ReplyTo != ping.RequestID {
		t.Fatalf("pong.reply_to = %q，期望 %q —— 前端无法把它对应到那次 ping",
			pong.ReplyTo, ping.RequestID)
	}
}

// TestClientWSCannotImpersonateAgent 验证方向校验。
//
// 浏览器发 `agent.ready` 这类服务端/Agent 专属消息必须被断连。
// 少了这道闸，一个已登录用户就能伪造「我的设备上线了」——
// 足以骗过 UI，也能污染服务端状态机。
func TestClientWSCannotImpersonateAgent(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	ticket := e.post(t, "/api/v1/ws-ticket", token, nil).Str(t, "ticket")

	ws, _ := e.wsDial(t, "/api/v1/ws/client?ticket="+ticket, nil)
	if ws == nil {
		t.Fatal("浏览器 WS 建连失败")
	}
	defer ws.Close()

	// agent.ready 是服务端发出的方向，浏览器发它属于方向越界。
	forged, _ := protocol.NewEnvelope(protocol.TypeAgentReady,
		protocol.AgentReadyPayload{ServerTime: e.clock.Now().UnixMilli()})
	forged.RequestID = uuid.NewString()
	sendJSON(t, ws, forged)

	expectCloseCode(t, ws, ClosePolicyViolation)
}

func TestClientWSFileOpsReturnExplicitError(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	ticket := e.post(t, "/api/v1/ws-ticket", token, nil).Str(t, "ticket")

	ws, _ := e.wsDial(t, "/api/v1/ws/client?ticket="+ticket, nil)
	if ws == nil {
		t.Fatal("浏览器 WS 建连失败")
	}
	defer ws.Close()

	// 文件传输是 Phase 8。此时必须回一条**明确的错误**，
	// 而不是静默丢弃 —— 静默丢弃会让前端永远停在 loading 上，
	// 而用户看到的只是「点了没反应」。
	req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeFileList, "",
		protocol.FileListPayload{Path: "/"})
	sendJSON(t, ws, req)

	resp := recvEnvelope(t, ws)
	if resp.Type != protocol.TypeError {
		t.Fatalf("未实现的文件操作应回 error，得到 %s", resp.Type)
	}
	if resp.ReplyTo != req.RequestID {
		t.Fatalf("error.reply_to = %q，期望 %q", resp.ReplyTo, req.RequestID)
	}
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// waitFor 轮询直到 cond 为真或超时。
//
// 用轮询而不是 sleep 固定时长：连接注册发生在另一个 goroutine 里，
// 固定 sleep 要么慢（保守取大值）要么不稳（取小了在慢机器上翻车）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件成立超时")
}
