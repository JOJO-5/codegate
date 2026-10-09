package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/jojo/codegate/internal/auth"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

// nonceTTL 是挑战 nonce 的有效期（§8.1）。
//
// 30 秒：足够覆盖一次网络往返（含跨洋 RTT 和 TLS 重协商），
// 又短到让「录下握手再重放」没有实用价值。
const nonceTTL = 30 * time.Second

// nonceBytes 是挑战随机数的长度。
//
// 32 字节 = 256 bit。这个长度下随机碰撞在物理上不可能发生，
// 所以 nonce 不需要「查重」—— 那是 64 bit 时代才需要的做法。
const nonceBytes = 32

// dbTimeout 是 WS 消息处理里单次数据库操作的超时。
//
// 必须有：WS 读循环是**串行**的，一次卡住的 DB 调用会让这条连接
// 完全停止读消息（包括心跳），最终被判死。给它一个短超时，
// 让它失败得干脆一点，比拖死整条连接好。
const dbTimeout = 5 * time.Second

// fatalClose 表示「必须断开这条连接」，并带上要发给对端的关闭码。
//
// 与普通 error 的分工：
//   - 业务失败（无权访问、设备离线）→ 回一条 error 消息，**连接继续用**
//   - 协议越界（未认证就发运行期消息、伪造帧）→ 断连
//
// 混在一起的后果是：要么把试探者留在连接上慢慢磨，
// 要么因为一次业务失败把一条好连接掐掉。
type fatalClose struct {
	code   int
	reason string
}

func (e *fatalClose) Error() string { return e.reason }

// agentSession 是一条 Agent 连接在生命周期内的全部状态。
//
// 用结构体而不是一堆局部变量：读循环里的每个处理函数都需要
// 「这条连接是谁、认证到哪一步了、nonce 是什么」，
// 靠参数传递会迅速变成七八个参数的函数签名。
type agentSession struct {
	srv *Server
	ac  *AgentConn
	ip  string

	// ---- 握手状态 ----
	authed bool
	nonce  string
	// nonceExp 是 nonce 的过期时刻。
	nonceExp time.Time
	// challengeAt 是签发挑战时的服务端毫秒时间戳。
	//
	// ★ 必须原样记住它：签名对象里包含 server_time，
	// 验证时要用**同一个值**重算。用 time.Now() 重算会得到不同的
	// 签名对象，导致一个完全正确的签名验不过 —— 而且因为时间只差
	// 几毫秒，日志里看不出任何异常。
	challengeAt int64

	// pairDeviceID 是「本次配对流程自称的 device_id」。
	//
	// ★ 刻意与 ac.DeviceID 分开：配对发生在认证**之前**，
	// 此时 device_id 只是 Agent 自称的。如果直接写进 ac.DeviceID，
	// 退出路径就会拿这个自称的 ID 去清理配对登记表 ——
	// 攻击者可以伪造别人的 device_id 来踢掉对方正在进行的配对。
	pairDeviceID string
}

// handleWSAgent 是 Agent 长连接的入口。
//
// GET /api/v1/ws/agent   （Ed25519 挑战-应答在连接内完成）
func (s *Server) handleWSAgent(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)

	// 建连限流放在升级之前：升级之后再拒就要走关闭帧，而
	// 「连上就被踢」对 Agent 来说是不可区分的（它只会看到一次失败），
	// 这里直接给 429 让 Agent 的退避逻辑能识别出是限流。
	if !s.wsLimit.Allow("agent:" + ip) {
		writeError(w, http.StatusTooManyRequests,
			string(protocol.CodeRateLimited), "连接过于频繁，请稍后重试")
		return
	}

	ws, err := s.upgrade(w, r)
	if err != nil {
		return
	}

	ac := newAgentConn(ws, s.cfg.SendQueueSize)
	go s.writePump(ws, ac.Send(), ac.Done(), "agent/"+ip)

	sess := &agentSession{srv: s, ac: ac, ip: ip}
	code, reason := sess.run()

	// ---- 统一退出路径 ----
	//
	// 所有清理都只在这里做。散在各个分支里的话，「某个分支忘了摘注册表」
	// 会让一台设备永远显示在线，而这是最难被发现的那类 bug。
	closeWithCode(ws, code, reason)
	ac.Close()
	s.closeWebAgent(ac)

	if sess.pairDeviceID != "" {
		s.pairs.Unregister(sess.pairDeviceID)
	}
	if ac.DeviceID != "" {
		// ★ 只在「没有更新的连接顶上来」时才清理待回请求。
		//
		// 被顶掉的旧连接退出时，注册表里已经是新连接了 —— 那些待回请求
		// 正好该由新连接来应答，清掉等于把正在飞的请求全部作废。
		if cur, live := s.reg.Agent(ac.DeviceID); !live || cur == ac {
			s.pending.DropDevice(ac.DeviceID)
		}
	}
	s.reg.RemoveAgent(ac)

	s.log.Info("Agent 连接结束",
		"device_id", ac.DeviceID, "user_id", ac.UserID, "code", code, "reason", reason)

	if ac.UserID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		s.audit(ctx, auditEntry{
			UserID: ac.UserID, DeviceID: ac.DeviceID,
			Action: auditAgentConnect, Result: "disconnect",
			IP:   ip,
			Meta: map[string]any{"code": code, "reason": reason},
		})
	}
}

// run 跑读循环，返回要发给对端的关闭码与原因。
func (s *agentSession) run() (int, string) {
	// 握手阶段有硬超时。未认证的连接不占注册表，但占着 goroutine 和 socket，
	// 没有超时的话「开一万条连接只发 hello」就是一个廉价的资源耗尽手段。
	_ = s.ac.conn.SetReadDeadline(time.Now().Add(wsHandshakeTimeout))

	for {
		mt, data, err := protocol.ReadWSMessage(s.ac.conn, wsMaxReadSize)
		if err != nil {
			return s.classifyReadError(err)
		}
		s.ac.Touch()
		if s.authed {
			refreshReadDeadline(s.ac.conn, wsPongWait)
		}

		var herr error
		switch mt {
		case websocket.TextMessage:
			herr = s.onText(data)
		case websocket.BinaryMessage:
			herr = s.onBinary(data)
		default:
			// gorilla 只会产出 Text/Binary；出现别的说明底层被换过了。
			herr = &fatalClose{ClosePolicyViolation, "unexpected_message_type"}
		}

		if herr != nil {
			var fc *fatalClose
			if errors.As(herr, &fc) {
				return fc.code, fc.reason
			}
			// 非致命错误：已经处理过（通常是回了一条 error 消息），继续读。
			s.srv.log.Debug("Agent 消息处理失败（连接继续）", "err", herr)
		}
	}
}

// classifyReadError 判断读错误意味着什么，返回要回给对端的关闭码。
//
// 恒返回 CloseNormal 是**刻意的**：读循环里的错误只有两种来源 ——
// 对端主动关闭（收到了关闭帧）和链路断了。两者对 Agent 来说都是
// 「退避后重连」，而重连策略由 Agent 侧决定（它认得的致命码只有
// 4401 / 4426）。服务端在这里自作主张给一个"错误"关闭码，
// 反而会让 Agent 把一次普通断线当成致命错误、彻底不重连了。
//
// 注意：走到这里时对端多半已经走了（TCP 断了），关闭帧大概率发不出去。
// 返回值仍然要正确 —— 它是日志和审计里的依据，不是给人看的。
func (s *agentSession) classifyReadError(err error) (int, string) {
	s.srv.log.Debug("Agent 读循环结束",
		"device_id", s.ac.DeviceID, "authed", s.authed, "err", err)
	return CloseNormal, "closed"
}

// ---------------------------------------------------------------------------
// 文本消息
// ---------------------------------------------------------------------------

func (s *agentSession) onText(data []byte) error {
	// ★ 必须用 DecodeFrom 而不是裸 Decode：它会按连接侧校验发出方。
	//
	// 少了这一步，Agent 就能发 `agent.ready`（服务端专属）或
	// `session.create`（客户端专属）—— 前者能欺骗服务端状态机，
	// 后者能让一台被攻陷的 Agent 冒充用户去操作别人的会话。
	env, err := protocol.DecodeFrom(data, protocol.SideAgentConn)
	if err != nil {
		s.srv.log.Warn("Agent 发来非法控制消息",
			"device_id", s.ac.DeviceID, "ip", s.ip, "err", err)
		return &fatalClose{ClosePolicyViolation, "invalid_message"}
	}

	if !s.authed {
		return s.onHandshake(env)
	}
	return s.onAuthenticated(env)
}

// onHandshake 处理认证完成之前的消息。
//
// 这个阶段只允许三种消息：hello、auth、pair.begin。
// 其余一律断连 —— 允许的话，未认证的连接就能发 `session.*`，
// 而服务端此时还不知道它是谁。
func (s *agentSession) onHandshake(env *protocol.Envelope) error {
	switch env.Type {
	case protocol.TypeAgentHello:
		return s.handleHello(env)
	case protocol.TypeAgentAuth:
		return s.handleAuth(env)
	case protocol.TypeAgentPairBegin:
		return s.handlePairBegin(env)
	case protocol.TypePing:
		return s.send(protocol.TypePong, nil, env)
	case protocol.TypeError, protocol.TypePong:
		s.srv.log.Debug("握手阶段收到意外消息", "type", env.Type, "ip", s.ip)
		return nil
	default:
		s.srv.log.Warn("未认证的 Agent 发送运行期消息",
			"type", env.Type, "ip", s.ip)
		return &fatalClose{CloseUnauthorized, "not_authenticated"}
	}
}

// handleHello 处理 `agent.hello`：校验版本、发挑战。
func (s *agentSession) handleHello(env *protocol.Envelope) error {
	p, err := protocol.DecodePayload[protocol.AgentHelloPayload](env)
	if err != nil {
		return &fatalClose{ClosePolicyViolation, "bad_hello"}
	}
	if p.DeviceID == "" {
		s.sendError(env, protocol.NewError(protocol.CodeInvalidPayload, "device_id 为空"))
		return &fatalClose{CloseUnauthorized, "missing_device_id"}
	}

	if !protocol.Compatible(p.Protocol) {
		// 先回一条 error 再关：光靠关闭码 4426，用户只能看到
		// 「连接被关闭」，而这条消息能让 Agent 打印出「请升级」。
		s.sendError(env, protocol.NewError(protocol.CodeVersionUnsupported,
			"协议版本不兼容，请升级 Agent"))
		return &fatalClose{CloseVersionUnsupported, "version_mismatch"}
	}

	// 记下 Agent 自报的信息。注意此时**还没认证** ——
	// DeviceID 只是它自称的，真正的归属要等 auth 之后从库里查。
	s.ac.DeviceID = p.DeviceID
	s.ac.agentVer = p.AgentVersion
	s.ac.platform = p.Platform
	s.ac.caps = p.Caps

	nonce, err := newNonce()
	if err != nil {
		s.srv.log.Error("生成挑战 nonce 失败", "err", err)
		return &fatalClose{CloseInternalError, "nonce_failed"}
	}

	now := s.srv.now()
	s.nonce = nonce
	s.nonceExp = now.Add(nonceTTL)
	s.challengeAt = now.UnixMilli()

	return s.send(protocol.TypeAgentChallenge, protocol.AgentChallengePayload{
		Nonce:      nonce,
		ServerTime: s.challengeAt,
	}, env)
}

// handleAuth 处理 `agent.auth`：验签、注册、发 ready。
func (s *agentSession) handleAuth(env *protocol.Envelope) error {
	// ---- 挑战必须存在且未过期 ----
	if s.nonce == "" {
		s.sendError(env, protocol.NewError(protocol.CodeUnauthenticated, "尚未收到挑战，请先发送 agent.hello"))
		return &fatalClose{CloseUnauthorized, "no_challenge"}
	}
	if s.srv.now().After(s.nonceExp) {
		s.sendError(env, protocol.NewError(protocol.CodeUnauthenticated, "挑战已过期，请重连"))
		return &fatalClose{CloseUnauthorized, "challenge_expired"}
	}

	// ★ nonce 一次性：无论验签结果如何都立刻作废。
	//
	// 只在「验签失败」时作废的话，成功的那次会留下一个仍然有效的
	// nonce —— 攻击者录下这次握手就能重放。作废放在最前面，
	// 就不存在「某条路径忘了作废」的可能。
	nonce, challengeAt := s.nonce, s.challengeAt
	s.nonce, s.challengeAt = "", 0

	p, err := protocol.DecodePayload[protocol.AgentAuthPayload](env)
	if err != nil {
		s.sendError(env, protocol.NewError(protocol.CodeInvalidPayload, "auth payload 非法"))
		return &fatalClose{CloseUnauthorized, "bad_auth_payload"}
	}

	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		s.authReject(env, "bad_signature_encoding")
		return &fatalClose{CloseUnauthorized, "bad_signature"}
	}

	ctx, cancel := s.dbCtx()
	defer cancel()

	d, err := s.srv.store.DeviceByID(ctx, s.ac.DeviceID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// 未注册 / 已被解绑 —— 两者对外同一句话：
			// 区分开的话，一个拿着旧 device_id 的攻击者就能探测
			// 某台设备是否存在过。
			s.sendError(env, protocol.NewError(protocol.CodeDeviceNotPaired,
				"设备未注册或已解绑，请在 Agent 上重新执行 codegate-agent pair"))
			return &fatalClose{CloseUnauthorized, "device_not_paired"}
		}
		s.srv.log.Error("查询设备失败", "device_id", s.ac.DeviceID, "err", err)
		return &fatalClose{CloseInternalError, "store_error"}
	}

	if d.RevokedAt != nil {
		s.sendError(env, protocol.NewError(protocol.CodeDeviceNotPaired, "设备已被吊销"))
		return &fatalClose{CloseUnauthorized, "device_revoked"}
	}
	if d.UserID == "" {
		s.sendError(env, protocol.NewError(protocol.CodeDeviceNotPaired,
			"设备尚未绑定账号，请先完成配对"))
		return &fatalClose{CloseUnauthorized, "device_not_paired"}
	}
	if len(d.PublicKey) != ed25519.PublicKeySize {
		// 公钥长度不对说明库里数据损坏。这**不是**攻击者的错，
		// 但也不能放行 —— 记 Error 让它被看见。
		s.srv.log.Error("设备公钥长度非法",
			"device_id", d.ID, "len", len(d.PublicKey))
		s.sendError(env, protocol.NewError(protocol.CodeInternal, "设备公钥异常，请重新配对"))
		return &fatalClose{CloseUnauthorized, "bad_public_key"}
	}

	payload, err := protocol.SigningPayload(nonce, s.ac.DeviceID, challengeAt)
	if err != nil {
		s.sendError(env, protocol.NewError(protocol.CodeInternal, "internal error"))
		return &fatalClose{CloseUnauthorized, "signing_payload_failed"}
	}

	if !ed25519.Verify(ed25519.PublicKey(d.PublicKey), payload, sig) {
		// 签名不对 = 对方没有这台设备的私钥。
		// 这是最值得记审计的一条：可能是设备被复制，也可能是有人在爆破。
		s.authReject(env, "bad_signature")
		s.srv.audit(ctx, auditEntry{
			DeviceID: d.ID, Action: auditAgentAuthFail, Result: auditResultDenied,
			IP: s.ip, Meta: map[string]any{"reason": "bad_signature"},
		})
		return &fatalClose{CloseUnauthorized, "bad_signature"}
	}

	// ---- 认证通过 ----
	s.ac.UserID = d.UserID

	// ★ 先发 ready 再注册。
	//
	// 注册之后这条连接就**可以被路由**了，而 Agent 在收到 ready 之前
	// 还没进入运行状态机 —— 此时投给它一帧终端数据，它只能丢弃
	// （或更糟，报一条"未知会话"）。
	if err := s.sendReady(env); err != nil {
		return &fatalClose{CloseInternalError, "send_ready_failed"}
	}

	if evicted := s.srv.reg.AddAgent(s.ac); evicted != nil {
		// 同一设备已有连接（Agent 重启或切网，旧连接还没超时）。
		// 策略是**新连接顶掉旧连接**（§8.1）—— 拒绝新连接会让设备
		// 最长 60 秒不可用，而顶掉旧连接的最坏后果只是旧连接上
		// 正在处理的请求失败一次。
		s.srv.log.Info("顶掉同一设备的旧连接",
			"device_id", s.ac.DeviceID, "old_user_id", evicted.UserID)
		closeWithCode(evicted.conn, CloseDuplicateConnection, "duplicate_connection")
		evicted.Close()
		s.srv.audit(ctx, auditEntry{
			UserID: d.UserID, DeviceID: d.ID,
			Action: auditAgentDuplicateConn, Result: auditResultOK, IP: s.ip,
		})
	}

	s.authed = true
	// 认证完成，握手窗口就此结束：从这一行起读超时改由 Pong 续命，
	// 不再依赖 Agent 的业务数据（见 armReadDeadline 的说明）。
	armReadDeadline(s.ac.conn, wsPongWait)

	if err := s.srv.store.TouchDeviceLastSeen(ctx, d.ID, s.srv.now()); err != nil {
		// 只影响设备页的"最后在线"，不该让认证失败。
		s.srv.log.Warn("更新设备最后在线时间失败", "device_id", d.ID, "err", err)
	}

	s.srv.audit(ctx, auditEntry{
		UserID: d.UserID, DeviceID: d.ID,
		Action: auditAgentConnect, Result: auditResultOK, IP: s.ip,
		Meta: map[string]any{
			"platform":      s.ac.platform,
			"agent_version": s.ac.agentVer,
		},
	})
	s.srv.log.Info("Agent 已认证",
		"device_id", d.ID, "user_id", d.UserID,
		"platform", s.ac.platform, "agent_version", s.ac.agentVer)

	return nil
}

// authReject 回一条统一的认证失败消息。
//
// ★ 所有验签失败路径都用**同一句话**：区分「签名格式错」和
// 「签名不对」会告诉攻击者他离成功有多近。
func (s *agentSession) authReject(env *protocol.Envelope, reason string) {
	s.sendError(env, protocol.NewError(protocol.CodeUnauthenticated, "认证失败"))
	s.srv.log.Warn("Agent 认证被拒", "device_id", s.ac.DeviceID, "ip", s.ip, "reason", reason)
}

// handlePairBegin 处理 `agent.pair.begin`：发配对码（§10.3）。
func (s *agentSession) handlePairBegin(env *protocol.Envelope) error {
	if s.authed {
		s.sendError(env, protocol.NewError(protocol.CodeInvalidMessage,
			"已认证的连接无需配对"))
		return nil
	}

	// 限流：配对码要落库，不限流的话一个脚本就能把 pairing_codes 撑大。
	if !s.srv.pairBeginLimit.Allow("pair:" + s.ip) {
		s.sendError(env, protocol.NewRetryableError(protocol.CodeRateLimited,
			"请求配对码过于频繁，请稍后再试"))
		return &fatalClose{CloseRateLimited, "rate_limited"}
	}

	p, err := protocol.DecodePayload[protocol.AgentPairBeginPayload](env)
	if err != nil {
		return &fatalClose{ClosePolicyViolation, "bad_pair_begin"}
	}
	if p.DeviceID == "" {
		s.sendError(env, protocol.NewError(protocol.CodeInvalidPayload, "device_id 为空"))
		return &fatalClose{CloseUnauthorized, "missing_device_id"}
	}

	pub, err := base64.StdEncoding.DecodeString(p.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		s.sendError(env, protocol.NewError(protocol.CodeInvalidPayload, "public_key 非法"))
		return &fatalClose{CloseUnauthorized, "bad_public_key"}
	}

	code, hash, err := auth.NewPairingCode()
	if err != nil {
		s.srv.log.Error("生成配对码失败", "err", err)
		return &fatalClose{CloseInternalError, "pair_code_failed"}
	}

	now := s.srv.now()
	expires := now.Add(s.srv.cfg.PairingCodeTTL)

	pc := &storage.PairingCode{
		ID:           uuid.NewString(),
		CodeHash:     hash,
		DeviceID:     p.DeviceID,
		PublicKey:    pub,
		Name:         p.Name,
		Platform:     p.Platform,
		Arch:         p.Arch,
		AgentVersion: p.AgentVersion,
		AgentIP:      s.ip,
		ExpiresAt:    expires,
		CreatedAt:    now,
	}

	ctx, cancel := s.dbCtx()
	defer cancel()
	if err := s.srv.store.CreatePairingCode(ctx, pc); err != nil {
		s.srv.log.Error("保存配对码失败", "device_id", p.DeviceID, "err", err)
		return &fatalClose{CloseInternalError, "store_error"}
	}

	// 登记这条连接，好让「确认绑定」时能通知它。
	// 用 pairDeviceID 而不是 ac.DeviceID —— 见字段注释。
	s.pairDeviceID = p.DeviceID
	s.srv.pairs.Register(p.DeviceID, s.ac.TrySendText, expires)

	s.srv.audit(ctx, auditEntry{
		DeviceID: p.DeviceID, Action: auditDevicePairBegin, Result: auditResultOK,
		IP: s.ip, Meta: map[string]any{"platform": p.Platform},
	})

	return s.send(protocol.TypeAgentPairCode, protocol.AgentPairCodePayload{
		Code:      code,
		ExpiresIn: int(s.srv.cfg.PairingCodeTTL.Seconds()),
	}, env)
}

// ---------------------------------------------------------------------------
// 认证后的文本消息
// ---------------------------------------------------------------------------

func (s *agentSession) onAuthenticated(env *protocol.Envelope) error {
	if (env.Type == protocol.TypeGitResult || env.Type == protocol.TypeError) && s.srv.acceptGitReply(s.ac, env) {
		return nil
	}
	if (env.Type == protocol.TypeRepositoryListed || env.Type == protocol.TypeError) && s.srv.acceptRepositoryReply(s.ac, env) {
		return nil
	}
	if (env.Type == protocol.TypeToolUpdated || env.Type == protocol.TypeError) && s.srv.acceptToolReply(s.ac, env) {
		return nil
	}
	switch env.Type {
	case protocol.TypeWebStarted, protocol.TypeWebStopped:
		s.srv.acceptWebReply(s.ac, env)
		return nil
	case protocol.TypeGitResult, protocol.TypeRepositoryListed:
		return nil
	case protocol.TypeToolUpdated:
		return nil
	case protocol.TypeWebClose:
		p, err := protocol.DecodePayload[protocol.WebStreamPayload](env)
		if err != nil {
			return nil
		}
		id, err := uuid.Parse(p.StreamID)
		if err != nil {
			return nil
		}
		s.srv.web.mu.Lock()
		stream := s.srv.web.streams[id]
		s.srv.web.mu.Unlock()
		if stream != nil && stream.agent == s.ac {
			s.srv.finishWebStream(id, s.ac)
		}
		return nil
	case protocol.TypeAgentHeartbeat:
		return s.handleHeartbeat(env)
	case protocol.TypeSessionSync:
		return s.handleSessionSync(env)
	case protocol.TypeConversationBound:
		return s.handleConversationBound(env)
	case protocol.TypeQuotaResult, protocol.TypeConversationListed:
		s.routeToClient(env)
		return nil
	case protocol.TypeSessionCreated:
		return s.handleSessionCreated(env)
	case protocol.TypeSessionClosed, protocol.TypeSessionExit:
		// ★ 这两类消息有**双重身份**，必须先判断是哪一种。
		//
		// `session.closed` 既可能是对客户端 `session.close` 的**响应**
		// （带 reply_to），也可能是 Agent 自发的推送（进程自己退出了）。
		// 两条路径的后果完全不同：
		//
		//   - 响应：必须按 reply_to 转发回发起的那条浏览器连接，
		//     否则它会一直等 —— 前端停在 loading，直到 60 秒 TTL 超时。
		//   - 自发：没有请求可回，广播给所有订阅者。
		//
		// 判据只有一个：pending 表里有没有这条 request_id。
		// 那是「这条消息是响应」的唯一证据，不能靠猜。
		//
		// 这里踩过一次坑：早期无条件走 handleSessionEnded，
		// 结果 session.close 的响应永远到不了客户端（pending 条目也
		// 一直不消费），端到端测试里表现为「关会话卡 20 秒然后超时」。
		if s.routeToClient(env) {
			return nil
		}
		return s.handleSessionEnded(env)
	case protocol.TypePing:
		return s.send(protocol.TypePong, nil, env)
	case protocol.TypePong, protocol.TypeAgentHello, protocol.TypeAgentAuth,
		protocol.TypeAgentPairBegin:
		// 重复的握手消息：不致命，但说明对端状态机有问题，记一句。
		s.srv.log.Debug("认证后收到握手阶段消息", "type", env.Type, "device_id", s.ac.DeviceID)
		return nil
	case protocol.TypeError:
		if s.srv.acceptWebReply(s.ac, env) {
			return nil
		}
		// Agent 报错：如果它是对某个客户端请求的响应，转给那个客户端；
		// 否则只记日志（Agent 主动报的内部错误）。
		s.routeToClient(env)
		return nil
	default:
		// 其余都是对客户端请求的响应：
		// session.list.result / session.info / session.attached /
		// session.detached / file.* —— 统一按 reply_to 转发。
		s.routeToClient(env)
		return nil
	}
}

// routeToClient 把 Agent 的响应按 reply_to 转发给发起的客户端。
//
// 返回**是否命中了一条待回请求**。调用方需要这个返回值来判断
// 「这条消息到底是响应还是自发推送」—— 见 onAuthenticated 里
// session.closed 的分支。
func (s *agentSession) routeToClient(env *protocol.Envelope) bool {
	p, ok := s.srv.pending.Take(env.ReplyTo)
	if !ok {
		// 找不到对应请求：可能已被超时清理，也可能是 Agent 主动推送。
		// 都不是错误 —— 记 Debug 就够，否则 Agent 一次异常推送
		// 就能在日志里刷出几百行 Warn。
		s.srv.log.Debug("Agent 响应找不到待回请求",
			"type", env.Type, "reply_to", env.ReplyTo, "device_id", s.ac.DeviceID)
		return false
	}

	// ★ 先做副作用，再转发。
	//
	// 顺序反过来的话，客户端可能在收到 `session.attached` 的同一瞬间
	// 就发来一帧 stdin —— 而那时订阅关系还没建立，帧会被
	// Relay 以「未 attach 到该会话」拒掉。用户看到的是
	// 「终端一连上就输不进字」，而且只在时序巧合时出现。
	if env.Type == protocol.TypeError && p.release != nil {
		p.release()
	}
	s.applyResponseSideEffects(p, env)

	data, err := protocol.Encode(env)
	if err != nil {
		s.srv.log.Error("编码响应失败", "type", env.Type, "err", err)
		return true
	}
	if err := p.client.TrySendText(data); err != nil {
		s.srv.log.Warn("转发响应给客户端失败（队列满或已关闭）",
			"type", env.Type, "conn_id", p.client.ID)
	}
	return true
}

// applyResponseSideEffects 更新「只有服务端知道」的会话订阅状态。
//
// 这些状态无法从消息内容推出来：`session.attached` 的 payload 里
// 有 session_id，但**没有**说「这条响应属于哪个客户端连接」——
// 那是 pending 表里的信息。
func (s *agentSession) applyResponseSideEffects(p *pendingReq, env *protocol.Envelope) {
	sid := p.sessionID
	if sid == "" {
		sid = env.SessionID
	}
	if sid == "" {
		return
	}

	switch env.Type {
	case protocol.TypeSessionAttached:
		// attach 成功：这条客户端开始订阅该会话的输出。
		s.srv.reg.Subscribe(sid, p.client)
		s.srv.reg.SetSessionOwner(sid, s.ac.DeviceID)
		p.client.Attach(sid)
		p.client.SetAttachID(sid, p.requestID)
		s.ac.AddSession(sid)

	case protocol.TypeSessionDetached:
		s.srv.reg.Unsubscribe(sid, p.client)
		p.client.Detach(sid)

	case protocol.TypeSessionClosed:
		// 会话结束。落库、审计、以及广播给**其他**正在看这个终端的客户端。
		//
		// except = 发起者：它马上会通过 routeToClient 收到这一条，
		// 不跳过的话它会收到两条一模一样的通知。
		s.finishSession(env, sid, false, p.client)
	}
}

// handleHeartbeat 处理 `agent.heartbeat`：刷新运行态 + 会话集合。
func (s *agentSession) handleHeartbeat(env *protocol.Envelope) error {
	p, err := protocol.DecodePayload[protocol.HeartbeatPayload](env)
	if err != nil {
		// 坏心跳不致命：下一次心跳就能纠正。断连反而会让设备
		// 因为一条格式错误的心跳而离线。
		s.srv.log.Warn("心跳 payload 非法", "device_id", s.ac.DeviceID, "err", err)
		return nil
	}

	ctx, cancel := s.dbCtx()
	defer cancel()

	ids := make([]string, 0, len(p.Sessions))
	for _, hs := range p.Sessions {
		ids = append(ids, hs.SessionID)
		s.srv.reg.SetSessionOwner(hs.SessionID, s.ac.DeviceID)

		// ★ 用 UpdateSessionRuntime，**不能**用 UpsertSession。
		//
		// 心跳的 payload 里没有 name / command / args / cwd ——
		// 走 Upsert 会把这些元数据写成空值。这个坑在写
		// TestSessionUpsertAndPrune 时才暴露出来（Args 被清空了）。
		err := s.srv.store.UpdateSessionRuntime(ctx, hs.SessionID, storage.SessionRuntime{
			Status: hs.Status,
			Cols:   hs.Cols,
			Rows:   hs.Rows,
			PID:    hs.PID,
		})
		if err != nil {
			s.srv.log.Warn("更新会话运行态失败",
				"session_id", hs.SessionID, "err", err)
		}
	}

	// ★ 整体替换，不做增量。
	//
	// Agent 重启会丢掉全部会话（R5），而增量更新在这种情况下来不及
	// 摘除旧条目 —— 结果是「设备页显示着一堆早就死掉的会话」。
	// 整体替换天然收敛：Agent 说它有什么，就是什么。
	s.ac.SetSessions(ids)
	s.ac.SetUpdateStatus(p.Update)
	s.ac.SetCommands(p.Commands)
	s.ac.SetDSHWebEnabled(p.DSHWebEnabled)
	s.ac.SetRoots(p.Roots)
	return nil
}

// handleSessionSync 处理 `session.sync`：Agent 重连后的全量对账（§7.4）。
func (s *agentSession) handleSessionSync(env *protocol.Envelope) error {
	p, err := protocol.DecodePayload[protocol.SessionSyncPayload](env)
	if err != nil {
		s.srv.log.Warn("session.sync payload 非法", "device_id", s.ac.DeviceID, "err", err)
		return nil
	}

	ctx, cancel := s.dbCtx()
	defer cancel()

	ids := make([]string, 0, len(p.Sessions))
	for _, sum := range p.Sessions {
		ids = append(ids, sum.SessionID)
		s.srv.reg.SetSessionOwner(sum.SessionID, s.ac.DeviceID)

		m := sessionMetaFromSummary(s.ac.DeviceID, s.ac.UserID, sum)
		if err := s.srv.store.UpsertSession(ctx, m); err != nil {
			// 一条写失败不该中断整轮对账 —— 其余的会话仍然要落库。
			s.srv.log.Warn("写入会话元数据失败",
				"session_id", sum.SessionID, "err", err)
		}
	}

	// 对账的收尾动作：Agent 没提的会话都是上次进程生命周期留下的残留。
	if n, err := s.srv.store.PruneSessions(ctx, s.ac.DeviceID, ids); err != nil {
		s.srv.log.Warn("清理残留会话失败", "device_id", s.ac.DeviceID, "err", err)
	} else if n > 0 {
		s.srv.log.Info("已清理残留会话", "device_id", s.ac.DeviceID, "count", n)
	}

	s.ac.SetSessions(ids)
	s.srv.log.Info("会话对账完成", "device_id", s.ac.DeviceID, "sessions", len(ids))
	return nil
}

// handleSessionCreated 处理 `session.created`：新会话诞生。
func (s *agentSession) handleSessionCreated(env *protocol.Envelope) error {
	p, err := protocol.DecodePayload[protocol.SessionCreatedPayload](env)
	if err != nil {
		s.srv.log.Warn("session.created payload 非法", "err", err)
		s.routeToClient(env)
		return nil
	}

	sum := p.Session
	if sum.SessionID == "" {
		s.routeToClient(env)
		return nil
	}

	ctx, cancel := s.dbCtx()
	defer cancel()

	if sum.Recovery != nil && sum.Recovery.SourceID != "" {
		source, e := s.srv.store.SessionByID(ctx, sum.Recovery.SourceID)
		if e == nil && source.DeviceID == s.ac.DeviceID && source.UserID == s.ac.UserID && source.Cwd == sum.Cwd {
			b := *sum.Recovery
			b.SourceID = ""
			source.Recovery = &b
			if err := s.srv.store.UpsertSession(ctx, source); err != nil {
				return err
			}
		}
	}
	m := sessionMetaFromSummary(s.ac.DeviceID, s.ac.UserID, sum)
	if err := s.srv.store.UpsertSession(ctx, m); err != nil {
		s.srv.log.Warn("写入新会话失败", "session_id", sum.SessionID, "err", err)
	}

	s.ac.AddSession(sum.SessionID)
	s.srv.reg.SetSessionOwner(sum.SessionID, s.ac.DeviceID)

	s.srv.audit(ctx, auditEntry{
		UserID: s.ac.UserID, DeviceID: s.ac.DeviceID, SessionID: sum.SessionID,
		Action: auditSessionCreate, Result: auditResultOK,
		Meta: map[string]any{"command": sum.Command, "cwd": sum.Cwd},
	})

	s.routeToClient(env)
	return nil
}

// handleSessionEnded 处理 Agent **主动**推送的会话结束事件。
//
// 与 session.closed 作为「关闭请求的响应」不同，这里处理的是
// 没有对应请求的那一种：进程自己退出了、PTY 出错了。
//
// ★ 只有在 routeToClient 没找到待回请求时才会走到这里 ——
// 见 onAuthenticated 里 session.closed 分支的说明。
func (s *agentSession) handleSessionEnded(env *protocol.Envelope) error {
	sid := env.SessionID
	if sid == "" {
		// 从 payload 里再找一次：有些实现会把 session_id 只放在 payload 里。
		if p, err := protocol.DecodePayload[protocol.SessionExitPayload](env); err == nil {
			sid = p.SessionID
		}
	}
	if sid == "" {
		return nil
	}

	s.finishSession(env, sid, true, nil)
	return nil
}

// finishSession 是「会话结束」的公共副作用，两条路径共用：
// 客户端请求的响应（spontaneous=false）和 Agent 自发推送（spontaneous=true）。
//
// 抽出来是因为两条路径要做的三件事完全一样 —— 落库、审计、广播 + 清理。
// 分开写的话必然漂移，而漂移出来的那半边就是「用户关了会话，
// 但另一个窗口里的终端还停在那儿」。
//
// spontaneous 只影响审计记录（区分「用户关的」和「进程自己退的」）。
// except 是广播时要跳过的连接：作为响应时跳过发起者，它已经单独收到过了。
func (s *agentSession) finishSession(env *protocol.Envelope, sid string, spontaneous bool, except *ClientConn) {
	ctx, cancel := s.dbCtx()
	defer cancel()
	// An authenticated Agent may only report exits for its own registered
	// sessions. Validate durable ownership before mutating or notifying anyone.
	stored, err := s.srv.store.SessionByID(ctx, sid)
	if err != nil || stored.UserID != s.ac.UserID || stored.DeviceID != s.ac.DeviceID || !s.ac.HasSession(sid) {
		return
	}
	if env.Type == protocol.TypeSessionExit {
		payload, err := protocol.DecodePayload[protocol.SessionExitPayload](env)
		if err != nil || payload.SessionID != sid {
			return
		}
	} else {
		payload, err := protocol.DecodePayload[protocol.SessionClosedPayload](env)
		if err != nil || payload.SessionID != sid {
			return
		}
	}

	now := s.srv.now()
	status := "exited"
	if env.Type == protocol.TypeSessionClosed {
		status = "terminated"
	}
	var exitCode *int
	if p, err := protocol.DecodePayload[protocol.SessionExitPayload](env); err == nil && p.SessionID != "" {
		code := p.ExitCode
		exitCode = &code
	}

	if err := s.srv.store.UpdateSessionRuntime(ctx, sid, storage.SessionRuntime{
		Status:   status,
		ExitCode: exitCode,
		EndedAt:  &now,
	}); err != nil {
		s.srv.log.Warn("更新会话结束状态失败", "session_id", sid, "err", err)
	}

	s.ac.RemoveSession(sid)

	// ★ 顺序铁律：**先广播，再 ForgetSession**。
	//
	// ForgetSession 内部是 `delete(sessionSubs, sid)` —— 把订阅者名单
	// 整个删掉。反过来写的话，BroadcastToSession 遍历到的是一个空集合，
	// 一条都发不出去，而且**完全静默**：不报错、不记日志，
	// 表现成「另一个窗口里的终端永远停在最后一行」。
	//
	// 这个顺序错了整整一个 Phase 才被端到端测试抓出来 ——
	// 因为单元测试里通常只有一个订阅者，而它恰好是 except 的那个。
	if spontaneous {
		// Clear request/reply IDs: lifecycle events are not request replies.
		push, err := protocol.NewEnvelope(env.Type, env.Payload)
		if err == nil {
			push.SessionID = sid
			s.srv.relay.BroadcastToUser(s.ac.UserID, push)
		}
	} else {
		s.srv.relay.BroadcastToSession(sid, env, except)
	}
	s.srv.reg.ForgetSession(sid)

	s.srv.audit(ctx, auditEntry{
		UserID: s.ac.UserID, DeviceID: s.ac.DeviceID, SessionID: sid,
		Action: auditSessionClose, Result: auditResultOK,
		Meta: map[string]any{"status": status, "spontaneous": spontaneous},
	})
}

// ---------------------------------------------------------------------------
// 二进制帧
// ---------------------------------------------------------------------------

func (s *agentSession) onBinary(data []byte) error {
	// 未认证就发终端数据 → 直接断连。
	//
	// 这不是"忽略一下也没关系"的情况：二进制帧里没有身份信息，
	// 唯一的归属依据是**这条连接是谁**。连接没认证，归属就无从谈起。
	if !s.authed {
		s.srv.log.Warn("未认证的 Agent 发送二进制帧", "ip", s.ip)
		return &fatalClose{CloseUnauthorized, "binary_before_auth"}
	}
	typ, _, _, err := protocol.PeekFrameHeader(data)
	if err != nil {
		return &fatalClose{ClosePolicyViolation, "invalid_frame"}
	}
	if typ == protocol.FrameWebToServer {
		if err := s.srv.webFrame(s.ac, data); err != nil {
			return &fatalClose{ClosePolicyViolation, "invalid_web_frame"}
		}
		return nil
	}

	// Relay 会校验帧方向（Agent 只能发 stdout/buffer/file）
	// 和会话归属（这个会话确实挂在这条连接上）。
	// 两者任一不符都说明对端在伪造，没有继续对话的理由。
	if err := s.srv.relay.RouteFrameToClients(s.ac, data); err != nil {
		s.srv.log.Warn("Agent 发来非法二进制帧",
			"device_id", s.ac.DeviceID, "err", err)
		return &fatalClose{ClosePolicyViolation, "invalid_frame"}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 发送与工具
// ---------------------------------------------------------------------------

// send 构造并发送一条控制消息。
func (s *agentSession) send(t protocol.Type, payload any, replyTo *protocol.Envelope) error {
	env, err := protocol.NewEnvelope(t, payload)
	if err != nil {
		return err
	}
	if replyTo != nil {
		env.ReplyTo = replyTo.RequestID
		env.SessionID = replyTo.SessionID
	}
	data, err := protocol.Encode(env)
	if err != nil {
		return err
	}
	return s.ac.TrySendText(data)
}

// sendError 给对端回一条 error 消息。
func (s *agentSession) sendError(req *protocol.Envelope, err error) {
	sendErrorEnvelope(s.ac.TrySendText, req, err)
}

// sendReady 下发运行参数（§8.1 的最后一步）。
func (s *agentSession) sendReady(req *protocol.Envelope) error {
	cfg := s.srv.cfg
	return s.send(protocol.TypeAgentReady, protocol.AgentReadyPayload{
		Protocol:          protocol.Current(),
		ServerTime:        s.srv.now().UnixMilli(),
		HeartbeatInterval: int(cfg.HeartbeatInterval.Seconds()),
		Limits: protocol.AgentLimits{
			MaxSessions:    cfg.MaxSessions,
			MaxFrameSize:   cfg.MaxFrameSize,
			MaxBufferSize:  cfg.MaxBufferSize,
			MaxMessageSize: cfg.MaxMessageSize,
		},
	}, req)
}

// dbCtx 返回一个带超时的 context。
func (s *agentSession) dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbTimeout)
}

// newNonce 生成挑战随机数。
func newNonce() (string, error) {
	buf := make([]byte, nonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// sessionMetaFromSummary 把协议层的会话快照转成存储层模型。
func sessionMetaFromSummary(deviceID, userID string, sum protocol.SessionSummary) *storage.SessionMeta {
	m := &storage.SessionMeta{
		Recovery:  sum.Recovery,
		ID:        sum.SessionID,
		DeviceID:  deviceID,
		UserID:    userID,
		Name:      sum.Name,
		Command:   sum.Command,
		Args:      sum.Args,
		Cwd:       sum.Cwd,
		Status:    sum.Status,
		PID:       sum.PID,
		ExitCode:  sum.ExitCode,
		Cols:      sum.Cols,
		Rows:      sum.Rows,
		CreatedAt: time.UnixMilli(sum.CreatedAt),
	}
	if sum.StartedAt != nil {
		t := time.UnixMilli(*sum.StartedAt)
		m.StartedAt = &t
	}
	if sum.EndedAt != nil {
		t := time.UnixMilli(*sum.EndedAt)
		m.EndedAt = &t
	}
	if sum.LastAttachedAt != nil {
		t := time.UnixMilli(*sum.LastAttachedAt)
		m.LastAttachedAt = &t
	}
	return m
}

func (s *agentSession) handleConversationBound(env *protocol.Envelope) error {
	sum, err := protocol.DecodePayload[protocol.SessionSummary](env)
	if err != nil {
		return err
	}
	ctx, cancel := s.dbCtx()
	defer cancel()
	meta, err := s.srv.store.SessionByID(ctx, sum.SessionID)
	if err != nil || meta.DeviceID != s.ac.DeviceID || meta.UserID != s.ac.UserID || meta.Cwd != sum.Cwd {
		return nil
	}
	meta.Recovery = sum.Recovery
	return s.srv.store.UpsertSession(ctx, meta)
}
