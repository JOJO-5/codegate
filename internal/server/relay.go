package server

import (
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"sync/atomic"

	"github.com/jojo/codegate/internal/protocol"
)

// Relay 是**唯一**的跨连接数据通路（§4.2）。
//
// # 它只做三件事
//
//  1. 从已认证的连接上取「这条连接是谁」
//  2. 校验目标属于当前调用者 —— **每次转发都校验，不缓存授权结论**
//  3. 投递到目标连接的有界发送队列
//
// # 它刻意不做的事
//
//   - 不认识 payload 结构。二进制帧在 Relay 里是 []byte，只读前 20 字节头
//     （因为 session_id 在头里）。
//   - 不碰终端内容。终端字节可能是密钥、密码、私有代码 ——
//     一个「顺手记个日志」就会把它们落到磁盘上。
//
// # 为什么授权必须每次重算
//
// 缓存授权结论的诱惑很大（每帧一次 map 查找看着很浪费），但缓存就必须处理
// 失效：客户端 detach 了、设备被解绑了、用户被禁用了……任何一条路径漏了
// 失效通知，就留下一个「已经不该有权限的连接还在收数据」的洞。
// 重新校验的成本是两次 map 查找，不值得为此引入失效逻辑。
type Relay struct {
	reg *Registry
	log *slog.Logger

	droppedFrames atomic.Int64
	routedFrames  atomic.Int64
}

// NewRelay 构造转发器。
func NewRelay(reg *Registry, log *slog.Logger) *Relay {
	if log == nil {
		log = slog.Default()
	}
	return &Relay{reg: reg, log: log}
}

// Stats 返回转发统计快照。
type RelayStats struct {
	RoutedFrames  int64
	DroppedFrames int64
}

// Stats 返回累计计数，供 /readyz 与诊断使用。
func (r *Relay) Stats() RelayStats {
	return RelayStats{
		RoutedFrames:  r.routedFrames.Load(),
		DroppedFrames: r.droppedFrames.Load(),
	}
}

// RouteFrameToAgent 把浏览器发来的终端帧转发给它 attach 的会话所属的 Agent。
//
// 校验顺序刻意从廉价到昂贵，且**每一步失败都直接返回**：
// 宽松处理一次，后面就要到处打补丁。
func (r *Relay) RouteFrameToAgent(c *ClientConn, frame []byte) error {
	// 1. 只解 20 字节头。不做完整 DecodeFrame —— 那会校验 payload 边界，
	//    而 Relay 根本不需要看 payload。
	typ, _, streamID, err := protocol.PeekFrameHeader(frame)
	if err != nil {
		return fmt.Errorf("%w: %v", protocol.ErrInvalidMessage, err)
	}

	// 2. 方向校验：浏览器只能发 stdin（和文件帧）。
	//    没有这一步，客户端就能伪造 stdout 注入到别人的终端里。
	if err := protocol.ValidateFrameFrom(typ, protocol.SentByClient); err != nil {
		return err
	}

	sessionID := streamID.String()

	// 3. 这条连接确实 attach 到这个会话了吗。
	if !c.IsAttached(sessionID) {
		return protocol.NewError(protocol.CodeForbidden, "未 attach 到该会话")
	}

	// 4. 会话属于哪台设备。
	deviceID, ok := r.reg.SessionOwner(sessionID)
	if !ok {
		return protocol.NewError(protocol.CodeSessionNotFound, "会话不存在")
	}

	// 5. 那台设备在线吗。
	agent, ok := r.reg.Agent(deviceID)
	if !ok {
		return protocol.NewError(protocol.CodeDeviceOffline, "设备不在线")
	}

	// 6. ★ 设备属于这个用户吗。
	//
	//    用 AgentConn.UserID 做 O(1) 判断，而不是查库 ——
	//    这是热路径（每个按键一帧），一次 DB 往返是不可接受的。
	//    这个字段在连接认证时确定，并且设备解绑时会被强制断开连接，
	//    所以它在连接生命周期内始终有效。
	if agent.UserID != c.UserID {
		r.log.Warn("拒绝跨账号转发终端输入",
			"conn_id", c.ID, "user_id", c.UserID,
			"device_id", deviceID, "owner_id", agent.UserID)
		return protocol.NewError(protocol.CodeForbidden, "无权访问该会话")
	}

	if agent.Caps().TerminalViews {
		target, ok := r.reg.terminalView(protocol.TerminalViewID(c.AttachID(sessionID)).String())
		if !ok || target.client != c || target.role != "controller" {
			return protocol.NewError(protocol.CodeForbidden, "当前是只读视图，请先接管控制")
		}
		if typ == protocol.FrameStdin {
			id := protocol.TerminalViewID(c.AttachID(sessionID))
			data := append([]byte(nil), frame...)
			copy(data[4:20], id[:])
			frame = data
		}
	}
	// 7. 投递。
	if err := agent.TrySendBinary(frame); err != nil {
		// 队列满是**客户端发太快**或 Agent 侧卡住了。
		// 对 stdin 而言丢帧意味着用户输入丢失，不能静默 —— 记警告。
		r.droppedFrames.Add(1)
		r.log.Warn("Agent 发送队列已满，丢弃一帧输入",
			"device_id", deviceID, "session_id", sessionID, "bytes", len(frame))
		return err
	}
	r.routedFrames.Add(1)
	return nil
}

// RouteFrameToClients 把 Agent 的输出帧广播给该会话的所有订阅者。
//
// 投递失败（队列满）只记计数与警告，**不返回错误**：
// 一个慢客户端不该影响其他客户端，也不该让 Agent 侧看到错误。
// 客户端会在下一次 attach 时用 `since` 补齐（§15.3）。
func (r *Relay) RouteFrameToClients(agent *AgentConn, frame []byte) error {
	typ, _, streamID, err := protocol.PeekFrameHeader(frame)
	if err != nil {
		return fmt.Errorf("%w: %v", protocol.ErrInvalidMessage, err)
	}

	// Agent 只能发 stdout / buffer / 文件帧。
	if err := protocol.ValidateFrameFrom(typ, protocol.SentByAgent); err != nil {
		return err
	}

	sessionID := streamID.String()
	if target, ok := r.reg.terminalView(sessionID); ok {
		if target.deviceID != agent.DeviceID || target.client.UserID != agent.UserID || !agent.HasSession(target.sessionID) {
			return protocol.NewError(protocol.CodeForbidden, "终端视图不属于该设备")
		}
		sid, err := uuid.Parse(target.sessionID)
		if err != nil {
			return err
		}
		data := append([]byte(nil), frame...)
		copy(data[4:20], sid[:])
		if target.client.TrySendBinary(data) != nil {
			r.droppedFrames.Add(1)
		} else {
			r.routedFrames.Add(1)
		}
		return nil
	}
	// 这个会话确实是这条 Agent 连接持有的吗 —— 防止 Agent 伪造其他会话的输出。
	if !agent.HasSession(sessionID) {
		// Detach can race already queued per-view output. Unknown view IDs are
		// discarded, never broadcast; known cross-device targets are rejected above.
		if agent.Caps().TerminalViews && (typ == protocol.FrameStdout || typ == protocol.FrameBuffer) {
			return nil
		}
		r.log.Warn("Agent 发来不属于它的会话的数据",
			"device_id", agent.DeviceID, "session_id", sessionID)
		return protocol.NewError(protocol.CodeForbidden, "会话不属于该设备")
	}

	// 同一份 []byte 投给多个客户端：各客户端的 writePump 只读它，不会改写，
	// 所以共享底层数组是安全的，也省掉了 N 次拷贝。
	for _, c := range r.reg.Subscribers(sessionID) {
		if err := c.TrySendBinary(frame); err != nil {
			r.droppedFrames.Add(1)
			r.log.Warn("客户端发送队列已满，丢弃一帧输出",
				"conn_id", c.ID, "session_id", sessionID, "bytes", len(frame))
		}
	}
	r.routedFrames.Add(1)
	return nil
}

// SendToAgent 把一条控制消息发给某设备。
//
// 用于 Server 主动下发的消息（session.attached 的转发、错误通知等）。
func (r *Relay) SendToAgent(deviceID string, env *protocol.Envelope) error {
	agent, ok := r.reg.Agent(deviceID)
	if !ok {
		return protocol.NewError(protocol.CodeDeviceOffline, "设备不在线")
	}
	return r.sendEnvelope(agent.TrySendText, env, "agent", deviceID)
}

// SendToClient 把一条控制消息发给某个浏览器连接。
func (r *Relay) SendToClient(c *ClientConn, env *protocol.Envelope) error {
	return r.sendEnvelope(c.TrySendText, env, "client", c.ID)
}

// BroadcastToSession 把一条控制消息广播给某会话的所有订阅者。
//
// except 非 nil 时跳过它 —— 用于「这条消息是对它的请求的响应，
// 已经单独转发过一次」的场景（例如 session.closed）。不跳的话，
// 发起关闭的客户端会收到两条一模一样的通知，前端要么重复弹提示，
// 要么得自己写去重逻辑。
//
// 返回成功投递的数量。**不因个别失败而中断**：一条连接队列满
// 不该让其他客户端收不到会话已结束这类关键通知。
func (r *Relay) BroadcastToSession(sessionID string, env *protocol.Envelope, except *ClientConn) int {
	data, err := protocol.Encode(env)
	if err != nil {
		r.log.Error("编码广播消息失败", "type", env.Type, "err", err)
		return 0
	}

	sent := 0
	for _, c := range r.reg.Subscribers(sessionID) {
		if except != nil && c == except {
			continue
		}
		if err := c.TrySendText(data); err != nil {
			r.droppedFrames.Add(1)
			continue
		}
		sent++
	}
	return sent
}

// BroadcastToUser 把一条控制消息发给某用户的所有浏览器连接。
//
// 用于设备上下线这类与具体会话无关的通知。
func (r *Relay) BroadcastToUser(userID string, env *protocol.Envelope) int {
	data, err := protocol.Encode(env)
	if err != nil {
		r.log.Error("编码广播消息失败", "type", env.Type, "err", err)
		return 0
	}

	// 拿一份全部客户端的快照。
	r.reg.mu.RLock()
	all := make([]*ClientConn, 0, len(r.reg.clients))
	for _, c := range r.reg.clients {
		all = append(all, c)
	}
	r.reg.mu.RUnlock()

	sent := 0
	for _, c := range all {
		if c.UserID != userID {
			continue
		}
		if err := c.TrySendText(data); err != nil {
			r.droppedFrames.Add(1)
			continue
		}
		sent++
	}
	return sent
}

// sendEnvelope 是「编码 + 投递」的公共部分。
func (r *Relay) sendEnvelope(
	send func([]byte) error,
	env *protocol.Envelope,
	side, id string,
) error {
	data, err := protocol.Encode(env)
	if err != nil {
		// 编码失败是本端的 bug（比如 payload 里塞了不可序列化的东西），
		// 不该把它伪装成「对方不可达」。
		r.log.Error("编码控制消息失败", "type", env.Type, "err", err)
		return err
	}
	if err := send(data); err != nil {
		r.log.Warn("投递控制消息失败（队列满）", "side", side, "id", id, "type", env.Type)
		return err
	}
	return nil
}
