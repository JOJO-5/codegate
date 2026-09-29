package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/jojo/codegate/internal/protocol"
)

// clientSession 是一条浏览器连接的运行状态。
type clientSession struct {
	srv *Server
	c   *ClientConn
	ip  string
}

// handleWSClient 是浏览器长连接的入口。
//
// GET /api/v1/ws/client?ticket=<一次性票据>
//
// # 为什么用票据而不是 Authorization 头
//
// 浏览器的 WebSocket API **不支持自定义请求头** —— 没有地方放
// `Authorization: Bearer`。退路只有两条：把 access token 放进 query，
// 或者放进 Cookie。
//
//   - 放 query：长期凭证会进服务器访问日志、Referer、浏览器历史。
//     日志往往保留数月，等于把凭证公开存档。
//   - 放 Cookie：WebSocket 会自动带上 Cookie，但那样 CSRF 就成立了
//     （恶意页面能拿用户的 cookie 建连），得再补 Origin 校验。
//
// 票据方案规避了两者：`POST /ws-ticket`（带 Bearer）换一张 30 秒
// 一次性票据，票据进 URL 也无所谓 —— 它用完即焚，且只对**建连**
// 有效，泄露了也换不到别的东西（§8.2、决策 D11）。
func (s *Server) handleWSClient(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)

	if !s.wsLimit.Allow("client:" + ip) {
		writeError(w, http.StatusTooManyRequests,
			string(protocol.CodeRateLimited), "连接过于频繁，请稍后重试")
		return
	}

	// ★ 票据校验必须在升级**之前**。
	//
	// 升级之后就回不了 HTTP 状态码了，只能走关闭帧 —— 而前端的
	// WS 错误回调拿不到关闭码之外的信息，表现成「连不上，不知道为什么」。
	// 在这里拒绝，前端能拿到一个标准的 401 JSON，直接跳登录页。
	userID, ok := s.tickets.Consume(r.URL.Query().Get("ticket"), s.now())
	if !ok {
		// 不区分「票据不存在」和「已过期」：两者对调用方的处置完全一样
		// （重新申请一张），区分开只是给探测者多一个信号。
		writeError(w, http.StatusUnauthorized,
			string(protocol.CodeUnauthenticated), "WS 票据无效或已过期")
		return
	}

	ws, err := s.upgrade(w, r)
	if err != nil {
		return
	}

	c := newClientConn(uuid.NewString(), ws, s.cfg.SendQueueSize)
	c.UserID = userID
	s.reg.AddClient(c)
	go s.writePump(ws, c.Send(), c.Done(), "client/"+c.ID)

	s.log.Info("浏览器连接建立", "conn_id", c.ID, "user_id", userID, "ip", ip)

	sess := &clientSession{srv: s, c: c, ip: ip}
	code, reason := sess.run()

	// ---- 统一退出路径 ----
	closeWithCode(ws, code, reason)
	c.Close()
	sess.detachAll()
	s.pending.DropClient(c)
	s.reg.RemoveClient(c)

	s.log.Info("浏览器连接结束",
		"conn_id", c.ID, "user_id", userID, "code", code, "reason", reason)
}

// run 跑读循环。
func (s *clientSession) run() (int, string) {
	// 浏览器连接**已经是认证过的**（票据就是凭证），所以一开始就用
	// 正常的读超时，没有握手窗口。
	refreshReadDeadline(s.c.conn, wsPongWait)

	for {
		mt, data, err := s.c.conn.ReadMessage()
		if err != nil {
			s.srv.log.Debug("浏览器读循环结束", "conn_id", s.c.ID, "err", err)
			return CloseNormal, "closed"
		}
		s.c.Touch()
		refreshReadDeadline(s.c.conn, wsPongWait)

		var herr error
		switch mt {
		case websocket.TextMessage:
			herr = s.onText(data)
		case websocket.BinaryMessage:
			herr = s.onBinary(data)
		default:
			herr = &fatalClose{ClosePolicyViolation, "unexpected_message_type"}
		}

		if herr != nil {
			var fc *fatalClose
			if errors.As(herr, &fc) {
				return fc.code, fc.reason
			}
			s.srv.log.Debug("客户端消息处理失败（连接继续）", "conn_id", s.c.ID, "err", herr)
		}
	}
}

// onText 处理控制消息。
func (s *clientSession) onText(data []byte) error {
	// ★ DecodeFrom 会按连接侧校验发出方。
	//
	// 少了它，浏览器就能发 `agent.ready` / `session.created` 这类
	// 服务端或 Agent 专属的消息 —— 足以伪造出「我的设备上线了」
	// 或者「会话已创建」的假象，欺骗 UI 和 Server 的状态机。
	env, err := protocol.DecodeFrom(data, protocol.SideClientConn)
	if err != nil {
		s.srv.log.Warn("浏览器发来非法控制消息",
			"conn_id", s.c.ID, "user_id", s.c.UserID, "err", err)
		return &fatalClose{ClosePolicyViolation, "invalid_message"}
	}

	switch env.Type {
	case protocol.TypePing:
		s.sendEnvelope(protocol.TypePong, nil, env)
		return nil

	case protocol.TypePong:
		return nil

	case protocol.TypeSessionCreate, protocol.TypeSessionList, protocol.TypeSessionGet,
		protocol.TypeSessionAttach, protocol.TypeSessionDetach, protocol.TypeSessionClose,
		protocol.TypeSessionResize, protocol.TypeSessionSignal,
		protocol.TypeSessionClaimControl,
		protocol.TypeFileList, protocol.TypeFileStat, protocol.TypeFileRead, protocol.TypeFileWrite,
		protocol.TypeFileCancel:
		s.routeRequest(env)
		return nil


	case protocol.TypeFileAck:
		// 这两个是流控/取消通知，没有对应的实现，忽略即可。
		return nil

	default:
		// 已知类型但方向不对（例如浏览器发 `agent.hello`）已经被
		// DecodeFrom 挡掉了，走到这里的只可能是**服务端专属**的消息。
		sendErrorEnvelope(s.c.TrySendText, env,
			protocol.NewError(protocol.CodeInvalidMessage, "该消息不能由客户端发出"))
		return nil
	}
}

// onBinary 处理终端输入帧。
func (s *clientSession) onBinary(data []byte) error {
	err := s.srv.relay.RouteFrameToAgent(s.c, data)
	if err == nil {
		return nil
	}

	// 未 attach 就发 stdin：大概率是时序问题 ——
	// 前端的 `session.attached` 还在路上，用户已经敲了键。
	// 二进制流里没有回错误的地方，丢掉这一帧并记 Debug 就够；
	// 断连反而会把一次正常竞态变成「刚连上就掉线」。
	var ce *protocol.CodeError
	if errors.As(err, &ce) && ce.Code == protocol.CodeForbidden {
		s.srv.log.Debug("丢弃未 attach 会话的输入帧",
			"conn_id", s.c.ID, "bytes", len(data))
		return nil
	}

	// 帧方向非法（比如伪造 stdout）、帧头损坏、会话不存在 ——
	// 这些不是竞态能解释的，直接断连。
	s.srv.log.Warn("浏览器发来非法二进制帧",
		"conn_id", s.c.ID, "user_id", s.c.UserID, "err", err)
	return &fatalClose{ClosePolicyViolation, "invalid_frame"}
}

// ---------------------------------------------------------------------------
// 请求路由
// ---------------------------------------------------------------------------

// routeRequest 把一条客户端请求授权后转发给对应的 Agent。
//
// # 授权在这里收口（§10.5）
//
// 每一条请求都必须先经过 resolveTarget —— 它同时完成「查目标」和
// 「校验归属」两件事。分开写的话，迟早会有一个分支忘了校验，
// 而那正是 IDOR 的诞生方式。
func (s *clientSession) routeRequest(env *protocol.Envelope) {
	// ★ request_id 必填。
	//
	// 没有它就无法把 Agent 的响应路由回来（reply_to 是唯一线索），
	// 前端会永远停在 loading 上。与其收下这个注定没有响应的请求，
	// 不如立刻告诉它少了什么。
	if env.RequestID == "" {
		sendErrorEnvelope(s.c.TrySendText, env,
			protocol.NewError(protocol.CodeInvalidPayload, "请求缺少 request_id"))
		return
	}

	deviceID, sessionID, err := s.resolveTarget(env)
	if err != nil {
		sendErrorEnvelope(s.c.TrySendText, env, err)
		return
	}

	agent, ok := s.srv.reg.Agent(deviceID)
	if !ok {
		// 可重试：设备可能正在重连。
		sendErrorEnvelope(s.c.TrySendText, env,
			protocol.NewRetryableError(protocol.CodeDeviceOffline, "设备不在线"))
		return
	}
	// 双保险：注册表里的 owner 也必须匹配。
	// 设备被解绑又绑给别人时，旧连接可能还没被踢掉。
	if agent.UserID != s.c.UserID {
		s.srv.log.Warn("拒绝把请求转发给非本账号的设备",
			"conn_id", s.c.ID, "user_id", s.c.UserID,
			"device_id", deviceID, "owner_id", agent.UserID)
		sendErrorEnvelope(s.c.TrySendText, env, errNoAccess())
		return
	}

	// Replay frames precede session.attached on the Agent connection. Subscribe
	// before forwarding attach so the initial screen is not silently dropped.
	var release func()
	if env.Type == protocol.TypeSessionAttach && !s.c.IsAttached(sessionID) {
		s.srv.reg.Subscribe(sessionID, s.c)
		release = func() { s.srv.reg.Unsubscribe(sessionID, s.c) }
	}
	s.srv.pending.Add(env.RequestID, &pendingReq{
		client:    s.c,
		userID:    s.c.UserID,
		deviceID:  deviceID,
		sessionID: sessionID,
		kind:      env.Type,
		requestID: env.RequestID,
		release:   release,
	}, s.srv.now())

	if env.Type == protocol.TypeSessionDetach {
		p, err := protocol.DecodePayload[protocol.SessionDetachPayload](env)
		if err != nil {
			if pending, ok := s.srv.pending.Take(env.RequestID); ok && pending.release != nil { pending.release() }
			sendErrorEnvelope(s.c.TrySendText, env, err)
			return
		}
		// Ignore any attach_id supplied by the browser. Only this connection's
		// authenticated attach may be detached.
		p.AttachID = s.c.AttachID(sessionID)
		env.Payload, err = json.Marshal(p)
		if err != nil { return }
	}

	data, err := protocol.Encode(env)
	if err != nil {
		if pending, ok := s.srv.pending.Take(env.RequestID); ok && pending.release != nil { pending.release() }
		s.srv.log.Error("编码客户端请求失败", "type", env.Type, "err", err)
		sendErrorEnvelope(s.c.TrySendText, env,
			protocol.NewError(protocol.CodeInternal, "internal error"))
		return
	}

	if err := agent.TrySendText(data); err != nil {
		// 队列满 = 这台 Agent 的某条客户端连接堵住了。
		// ★ 必须把待回请求撤掉，否则它会一直挂到 TTL 超时，
		// 而前端在这 60 秒里什么都等不到。
		if pending, ok := s.srv.pending.Take(env.RequestID); ok && pending.release != nil { pending.release() }
		s.srv.log.Warn("转发请求失败：Agent 发送队列已满",
			"device_id", deviceID, "type", env.Type)
		sendErrorEnvelope(s.c.TrySendText, env,
			protocol.NewRetryableError(protocol.CodeInternal, "设备繁忙，请稍后重试"))
		return
	}
}

// resolveTarget 解析请求的目标设备并校验归属。
//
// 返回 (deviceID, sessionID, error)。
func (s *clientSession) resolveTarget(env *protocol.Envelope) (string, string, error) {
	ctx, cancel := s.dbCtx()
	defer cancel()

	switch env.Type {
	case protocol.TypeSessionCreate:
		p, err := protocol.DecodePayload[protocol.SessionCreatePayload](env)
		if err != nil {
			return "", "", err
		}
		if p.DeviceID == "" {
			return "", "", protocol.NewError(protocol.CodeInvalidPayload, "device_id 为空")
		}
		if _, err := s.srv.authorizeDevice(ctx, s.c.UserID, p.DeviceID); err != nil {
			return "", "", err
		}
		return p.DeviceID, "", nil

	case protocol.TypeSessionList:
		p, err := protocol.DecodePayload[protocol.SessionListPayload](env)
		if err != nil {
			return "", "", err
		}
		if p.DeviceID == "" {
			// 客户端不是设备，没有「本设备」这个概念 —— 必须显式指定。
			return "", "", protocol.NewError(protocol.CodeInvalidPayload,
				"session.list 必须指定 device_id")
		}
		if _, err := s.srv.authorizeDevice(ctx, s.c.UserID, p.DeviceID); err != nil {
			return "", "", err
		}
		return p.DeviceID, "", nil
	}

	// 其余都是会话维度的请求。
	sid := sessionIDOf(env)
	if sid == "" {
		return "", "", protocol.NewError(protocol.CodeInvalidPayload, "session_id 为空")
	}

	meta, err := s.srv.authorizeSession(ctx, s.c.UserID, sid)
	if err != nil {
		return "", "", err
	}

	// attach 的额外约束：一条连接同时只能看一个终端。
	//
	// ★ 刻意**不**自动替它 detach 上一个。
	//
	// 自动 detach 需要同时通知 Agent（它的 attached 计数要减一），
	// 而那是一次异步往返 —— 在这期间 Agent 认为旧会话还挂着、
	// 服务端认为已经摘了，两边状态分叉。分叉的后果是角色分配错乱
	// （controller 是谁两边说不一致）。
	// 让前端显式 `session.detach` 再 attach，代价是一次往返，
	// 换来的是两边状态永远一致。
	if env.Type == protocol.TypeSessionAttach && s.c.AttachedToOther(sid) {
		return "", "", protocol.NewError(protocol.CodeAlreadyAttached,
			"请先 detach 当前会话再 attach 新会话")
	}

	return meta.DeviceID, sid, nil
}

// sessionIDOf 从信封或 payload 里取 session_id。
//
// 两者都可能在：信封上的 SessionID 是「便于日志与路由」的冗余字段，
// 而 payload 里的是权威字段。不同客户端可能只填其中一个，
// 所以两个都认。
func sessionIDOf(env *protocol.Envelope) string {
	if len(env.Payload) == 0 {
		return ""
	}
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil || p.SessionID == "" {
		return ""
	}
	// The payload is forwarded unchanged to the Agent. Never authorize the
	// envelope's session and execute against a different payload session.
	if env.SessionID != "" && env.SessionID != p.SessionID {
		return ""
	}
	return p.SessionID
}

// detachAll 在客户端断开时清理它所有的 attach 关系。
//
// ★ 必须同时通知 Agent。
//
// 只清服务端的话，Agent 那边的 `attached` 计数会一直虚高：
//   - 会话页显示「2 个客户端在看」，其实早就只有一个
//   - 多客户端角色分配（§7.6）会把 controller 留给一个已经消失的连接，
//     于是新连上来的客户端永远拿不到控制权 —— 表现为「终端只读，打不了字」
func (s *clientSession) detachAll() {
	sids := s.c.AttachedSessions()
	if len(sids) == 0 {
		return
	}

	for _, sid := range sids {
		attachID := s.c.AttachID(sid)
		s.srv.reg.Unsubscribe(sid, s.c)
		s.c.Detach(sid)

		deviceID, ok := s.srv.reg.SessionOwner(sid)
		if !ok {
			continue
		}
		agent, ok := s.srv.reg.Agent(deviceID)
		if !ok {
			// 设备也断了 —— 它自己会清理所有会话，不用我们通知。
			continue
		}

		env, err := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionDetach, sid,
			protocol.SessionDetachPayload{SessionID: sid, AttachID: attachID, Reason: "client_close"})
		if err != nil {
			continue
		}
		data, err := protocol.Encode(env)
		if err != nil {
			continue
		}
		if err := agent.TrySendText(data); err != nil {
			s.srv.log.Debug("通知 Agent 客户端已断开失败（队列满）",
				"device_id", deviceID, "session_id", sid)
		}
	}
	s.srv.log.Debug("已清理客户端的所有 attach",
		"conn_id", s.c.ID, "count", len(sids))
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func (s *clientSession) sendEnvelope(t protocol.Type, payload any, replyTo *protocol.Envelope) {
	env, err := protocol.NewEnvelope(t, payload)
	if err != nil {
		return
	}
	if replyTo != nil {
		env.ReplyTo = replyTo.RequestID
	}
	data, err := protocol.Encode(env)
	if err != nil {
		return
	}
	_ = s.c.TrySendText(data)
}

func (s *clientSession) dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbTimeout)
}
