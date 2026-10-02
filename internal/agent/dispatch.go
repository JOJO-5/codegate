package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"github.com/jojo/codegate/internal/terminal"
)

// handleMessage 分发一条来自 Server 的控制消息。
//
// ★ 这里刻意**不用** protocol.DecodeFrom 做方向校验。
//
// DecodeFrom 校验的是「这条消息是否由本连接的对端身份发出」，
// 它是给 Server 用的（Server 的 agent 连接上，对端是 Agent）。
// 而 Agent 收到的是 Server **转发**的客户端请求 —— 消息的原始发出方
// 是浏览器，经手方是 Server。套用 DecodeFrom 会把所有合法请求
// 都判成方向错误。
//
// Agent 侧的等价防线是下面这个白名单 switch：只有列出来的类型会被处理，
// 其余一律记录后忽略。它比方向校验更直接 —— 它描述的是
// 「Agent 到底认哪些指令」，而这个集合应该是**可枚举且短**的。
func (a *Agent) handleMessage(env *protocol.Envelope) {
	switch env.Type {
	case protocol.TypeSessionCreate:
		a.onSessionCreate(env)
	case protocol.TypeSessionAttach:
		a.onSessionAttach(env)
	case protocol.TypeSessionDetach:
		a.onSessionDetach(env)
	case protocol.TypeSessionClose:
		a.onSessionClose(env)
	case protocol.TypeSessionResize:
		a.onSessionResize(env)
	case protocol.TypeSessionSignal:
		a.onSessionSignal(env)
	case protocol.TypeSessionList:
		a.onSessionList(env)
	case protocol.TypeSessionGet:
		a.onSessionGet(env)
	case protocol.TypeFileList:
		a.onFileList(env)
	case protocol.TypeFileStat:
		a.onFileStat(env)
	case protocol.TypeFileRead:
		a.onFileRead(env)
	case protocol.TypeFileWrite:
		a.onFileWrite(env)
	case protocol.TypeFileCancel:
		a.onFileCancel(env)
	case protocol.TypePing:
		a.reply(env, protocol.TypePong, nil)
	case protocol.TypeWebStart:
		a.onWebStart(env)
	case protocol.TypeGitRequest:
		go a.onGitRequest(env)
	case protocol.TypeRepositoryList:
		a.onRepositoryList(env)
	case protocol.TypeToolSet:
		a.onToolSet(env)
	case protocol.TypeWebStop:
		a.stopWeb()
		a.reply(env, protocol.TypeWebStopped, nil)
	case protocol.TypeWebOpen:
		a.onWebOpen(env)
	case protocol.TypeWebClose:
		a.onWebClose(env)
	case protocol.TypePong:
		// 心跳应答，无需处理 —— 收到它本身已经重置了读超时。
	default:
		a.log.Warn("收到 Agent 不处理的控制消息，已忽略", "type", env.Type)
	}
}

// handleFrame 处理入站二进制帧。
//
// ★ 方向校验在这里是**必需**的：Agent 只接受 stdin 帧。
// 没有这一步，一个被 XSS 拿到的浏览器会话就能往 Agent 发 stdout 帧，
// 而 Server 的中继会把它当成"来自 Agent 的输出"转发给其他客户端 ——
// 于是攻击者可以在别人的终端里画出任意内容。
// protocol.CanSendFrame 定义了规则，这里执行它。
func (a *Agent) handleFrame(data []byte) {
	frame, err := protocol.DecodeFrame(data)
	if err != nil {
		a.log.Warn("收到无法解析的二进制帧", "err", err, "bytes", len(data))
		return
	}
	if frame.Type == protocol.FrameWebToAgent {
		a.onWebFrame(frame)
		return
	}

	if err := protocol.ValidateFrameFrom(frame.Type, protocol.SentByClient); err != nil {
		a.log.Warn("拒绝方向非法的二进制帧", "type", frame.Type, "err", err)
		return
	}
	if frame.Type != protocol.FrameStdin {
		// file.data 等帧在 Phase 8 处理。现在明确忽略而不是报错。
		a.log.Debug("暂不处理的帧类型", "type", frame.Type)
		return
	}

	sess, ok := a.mgr.Get(frame.StreamID)
	if !ok {
		// 会话可能刚好被关闭。这是正常的竞态，不是错误。
		a.log.Debug("stdin 帧指向不存在的会话", "session", frame.StreamID)
		return
	}

	// 只有主控客户端能输入（§7.6）。viewer 是只读的 ——
	// 这不是防人（同一账号都是自己），是防"两台设备同时敲键盘"
	// 产生的字符交错，那会让命令行变成一堆乱码。
	connID := sess.ControllerID()
	if connID == "" {
		a.log.Debug("会话没有主控客户端，丢弃输入", "session", frame.StreamID)
		return
	}
	if _, err := sess.Write(connID, frame.Payload); err != nil {
		a.log.Debug("写入 PTY 失败", "session", frame.StreamID, "err", err)
	}
}

// ---------------------------------------------------------------------------
// 会话创建
// ---------------------------------------------------------------------------

func (a *Agent) onSessionCreate(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.SessionCreatePayload](env)
	if err != nil {
		a.replyError(env, err)
		return
	}

	sess, err := a.createSession(req)
	if err != nil {
		a.replyError(env, err)
		return
	}

	a.log.Info("会话已创建",
		"session", sess.ID, "command", sess.Command, "cwd", sess.Cwd, "pid", sess.PID())

	a.reply(env, protocol.TypeSessionCreated, protocol.SessionCreatedPayload{
		Session: sess.Summary(),
	})
	// Queue creation before watching Done, including commands that exit during
	// launch. This preserves Server ownership registration before exit delivery.
	go a.watchSessionExit(sess)
}

func (a *Agent) watchSessionExit(sess *session.Session) {
	<-sess.Done()
	if sess.Summary().Status != "exited" {
		return
	} // Explicit close has its own acknowledgement.
	code, _, reason, ended := sess.ExitInfo()
	if !ended {
		return
	}
	env, err := protocol.NewEnvelope(protocol.TypeSessionExit, protocol.SessionExitPayload{SessionID: sess.ID.String(), ExitCode: code, Reason: reason})
	if err == nil {
		env.SessionID = sess.ID.String()
		err = a.sendControl(env)
	}
	if err != nil {
		a.log.Warn("发送进程退出事件失败，重连后将通过会话对账同步状态", "session", sess.ID, "err", err)
	}
}

// createSession 做全部安全校验，然后创建会话。
//
// 顺序是刻意的：**先解析命令、再校验路径、最后才创建 PTY**。
// 创建 PTY 是有成本的动作（起进程、开句柄），任何一项校验失败
// 都不应该走到那一步 —— 否则一个恶意的 cwd 就能让 Agent 反复
// 起进程再杀掉，形成资源耗尽攻击。
func (a *Agent) createSession(req protocol.SessionCreatePayload) (*session.Session, error) {
	finish, err := a.beginActivity()
	if err != nil {
		return nil, err
	}
	defer finish()
	// Serialize directory selection and process creation with worktree removal.
	a.workspaceMu.Lock()
	defer a.workspaceMu.Unlock()
	// 1. 命令：只能从白名单里选（除非显式开启自定义命令）。
	cmd, err := a.commandConfig().ResolveCommand(req.CommandID, req.Command, req.Args, req.Resume)
	if err != nil {
		return nil, err
	}

	// 2. 工作目录：必须落在 allowed_roots 之内。
	//
	// ★ 这里和文件 API 用的是**同一个** Workspace 实例 ——
	// 两处各写一份校验必然漂移，而漂移出来的那个就是绕过口。
	cwd, err := a.ws.Resolve(req.Cwd)
	if err != nil {
		return nil, err
	}

	// Isolated task directories are created only after command and path validation.
	var rollback func()
	if req.Worktree {
		if req.Resume {
			return nil, fmt.Errorf("%w: 独立工作区必须新建对话", protocol.ErrInvalidPayload)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		cwd, rollback, err = createWorktree(ctx, a.ws, cwd)
		cancel()
		if err != nil {
			return nil, err
		}
	}

	// 3. 尺寸：0 值用安全默认（80x24），前端 attach 后会立刻覆盖。
	cols, rows := req.Cols, req.Rows
	if cols == 0 {
		cols = terminal.DefaultCols
	}
	if rows == 0 {
		rows = terminal.DefaultRows
	}

	// 4. 环境：白名单构造，绝不整体继承（F2 的教训，见 BuildEnv）。
	childEnv := BuildEnv(a.cfg, cols, rows)

	name := req.Name
	if name == "" {
		name = cmd.Label
	}

	sess, err := a.mgr.Create(context.Background(), session.CreateRequest{
		DeviceID: a.deviceUUID(),
		// UserID 由 Server 侧填充 —— Agent 不知道也不该知道是哪个用户在操作。
		UserID:  uuid.Nil,
		Name:    name,
		Command: cmd.Command,
		Args:    cmd.Args,
		Cwd:     cwd,
		Env:     childEnv,
		Cols:    cols,
		Rows:    rows,
	})
	if err != nil && rollback != nil {
		rollback()
	}
	return sess, err
}

// ---------------------------------------------------------------------------
// attach / detach
// ---------------------------------------------------------------------------

// connIDFor 决定这次操作属于哪条 view。
//
// ★ 约定：用信封的 RequestID 作为 ConnID。
//
// 协议里的 session.attach 没有 conn_id 字段（多客户端是 Q3 之后才确认的，
// 而 RequestID 本来就是"这次请求的唯一标识"）。Server 对每个浏览器的
// attach 用不同的 RequestID，Agent 侧就自然得到不同的 view；
// detach 时用同一个 RequestID，就能精确摘掉对应的那一条。
//
// 这不是权宜之计 —— 它让 Agent 不需要理解"客户端"这个概念，
// 只看到一串匿名的 attach 会话。多客户端聚合完全是 Server 的职责。
func connIDFor(env *protocol.Envelope) string {
	if env.RequestID != "" {
		return env.RequestID
	}
	// 没有 RequestID 时生成一个临时的。这样 attach 本身仍能工作，
	// 只是 detach 时无法精确指定（见 onSessionDetach 的处理）。
	return "anon-" + uuid.NewString()
}

func (a *Agent) onSessionAttach(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.SessionAttachPayload](env)
	if err != nil {
		a.replyError(env, err)
		return
	}

	sid, err := uuid.Parse(req.SessionID)
	if err != nil {
		a.replyError(env, fmt.Errorf("%w: session_id %q 不是合法 UUID", protocol.ErrInvalidPayload, req.SessionID))
		return
	}

	sess, err := a.mgr.MustGet(sid)
	if err != nil {
		a.replyError(env, err)
		return
	}

	connID := connIDFor(env)
	res, err := sess.Attach(session.AttachRequest{
		ConnID: connID,
		Sink:   &sessionSink{a: a, sid: sid},
		Since:  req.Since,
		Cols:   req.Cols,
		Rows:   req.Rows,
	})
	if err != nil {
		a.replyError(env, err)
		return
	}

	a.trackView(sid, connID)
	a.log.Info("会话已 attach",
		"session", sid, "conn", connID, "role", res.Role,
		"seq_from", res.SeqFrom, "seq_to", res.SeqTo, "truncated", res.Truncated)

	// ★ 必须在 Attach 返回**之后**才回 session.attached。
	//
	// Attach 内部已经按六步顺序把重放数据通过 Sink 发出去了
	// （退出备用屏幕 → 模式前导 → 清屏 → ring buffer → ?2026l → 登记）。
	// 如果先回响应再重放，前端会先收到 "attached" 然后才开始收字节 ——
	// 期间它可能已经按"没有历史"渲染了一帧，造成闪烁。
	a.reply(env, protocol.TypeSessionAttached, protocol.SessionAttachedPayload{
		Session: sess.Summary(),
		SeqFrom: res.SeqFrom,
		SeqTo:   res.SeqTo,
		Role:    string(res.Role),
	})
}

func (a *Agent) onSessionDetach(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.SessionDetachPayload](env)
	if err != nil {
		a.replyError(env, err)
		return
	}
	sid, err := uuid.Parse(req.SessionID)
	if err != nil {
		a.replyError(env, err)
		return
	}

	sess, err := a.mgr.MustGet(sid)
	if err != nil {
		a.replyError(env, err)
		return
	}

	// attach and detach have different request IDs. The Server supplies the
	// original attach ID from its authenticated connection state.
	if req.AttachID != "" {
		sess.Detach(req.AttachID)
		a.untrackView(sid, req.AttachID)
	}

	// ★ detach 只解除"谁在看"，**不终止 PTY**（不变量 I1/I2）。
	// 这正是整个项目的核心价值：关掉浏览器，Claude Code 继续跑。
	a.log.Info("会话已 detach（PTY 继续运行）", "session", sid, "reason", req.Reason)

	a.reply(env, protocol.TypeSessionDetached, protocol.SessionDetachedPayload{
		SessionID: req.SessionID,
		Reason:    req.Reason,
	})
}

// ---------------------------------------------------------------------------
// close / resize / signal
// ---------------------------------------------------------------------------

func (a *Agent) onSessionClose(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.SessionClosePayload](env)
	if err != nil {
		a.replyError(env, err)
		return
	}
	sid, err := uuid.Parse(req.SessionID)
	if err != nil {
		a.replyError(env, err)
		return
	}

	reason := "user_requested"
	if err := a.mgr.Close(sid, reason); err != nil {
		a.replyError(env, err)
		return
	}
	a.forgetViews(sid)

	a.log.Info("会话已关闭", "session", sid, "reason", reason)
	a.reply(env, protocol.TypeSessionClosed, protocol.SessionClosedPayload{
		SessionID: req.SessionID,
		Reason:    reason,
	})
}

func (a *Agent) onSessionResize(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.SessionResizePayload](env)
	if err != nil {
		a.replyError(env, err)
		return
	}
	sid, err := uuid.Parse(req.SessionID)
	if err != nil {
		a.replyError(env, err)
		return
	}

	sess, err := a.mgr.MustGet(sid)
	if err != nil {
		a.replyError(env, err)
		return
	}

	// 用当前主控客户端的身份调用。Session 会拒绝非主控的 resize
	// （多客户端下两个设备的分辨率差好几倍，都能 resize 会让 TUI
	//  在 40 列和 120 列之间反复横跳，§7.6）。
	if err := sess.Resize(sess.ControllerID(), req.Cols, req.Rows); err != nil {
		a.replyError(env, err)
		return
	}
	a.reply(env, protocol.TypeSessionInfo, protocol.SessionCreatedPayload{Session: sess.Summary()})
}

func (a *Agent) onSessionSignal(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.SessionSignalPayload](env)
	if err != nil {
		a.replyError(env, err)
		return
	}
	sid, err := uuid.Parse(req.SessionID)
	if err != nil {
		a.replyError(env, err)
		return
	}

	sig, err := terminal.ParseSignal(req.Signal)
	if err != nil {
		a.replyError(env, err)
		return
	}

	sess, err := a.mgr.MustGet(sid)
	if err != nil {
		a.replyError(env, err)
		return
	}
	if err := sess.Signal(sig); err != nil {
		a.replyError(env, err)
		return
	}

	a.log.Info("已投递信号", "session", sid, "signal", sig)
	a.reply(env, protocol.TypeSessionInfo, protocol.SessionCreatedPayload{Session: sess.Summary()})
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func (a *Agent) onSessionList(env *protocol.Envelope) {
	a.reply(env, protocol.TypeSessionListed, protocol.SessionListedPayload{
		Sessions: a.mgr.Snapshot(),
	})
}

func (a *Agent) onSessionGet(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.SessionAttachPayload](env)
	if err != nil {
		a.replyError(env, err)
		return
	}
	sid, err := uuid.Parse(req.SessionID)
	if err != nil {
		a.replyError(env, err)
		return
	}
	sess, err := a.mgr.MustGet(sid)
	if err != nil {
		a.replyError(env, err)
		return
	}
	a.reply(env, protocol.TypeSessionInfo, protocol.SessionCreatedPayload{
		Session: sess.Summary(),
	})
}

// ---------------------------------------------------------------------------
// 周期上报
// ---------------------------------------------------------------------------

// sendSessionSync 上报全量会话列表，供 Server 对账（§7.4）。
func (a *Agent) sendSessionSync() {
	env, err := protocol.NewEnvelope(protocol.TypeSessionSync, protocol.SessionSyncPayload{
		Sessions: a.mgr.Snapshot(),
	})
	if err != nil {
		a.log.Error("构造 session.sync 失败", "err", err)
		return
	}
	if err := a.sendControl(env); err != nil {
		a.log.Warn("发送 session.sync 失败", "err", err)
	}
}

// sendHeartbeat 上报心跳 + 会话摘要（§3.3）。
func (a *Agent) sendHeartbeat(context.Context) error {
	a.writeManagedHealth(true)
	summaries := a.mgr.Snapshot()
	out := make([]protocol.HeartbeatSession, 0, len(summaries))
	for _, s := range summaries {
		// 不用 uuid.MustParse：SessionSummary 的 SessionID 是字符串，
		// 理论上可能来自别处（比如 Server 对账时构造的）。为一个
		// 心跳字段冒 panic 的风险不值得。
		sid, err := uuid.Parse(s.SessionID)
		if err != nil {
			continue
		}
		out = append(out, protocol.HeartbeatSession{
			SessionID: s.SessionID,
			Status:    s.Status,
			Attached:  a.viewCount(sid),
			Cols:      s.Cols,
			Rows:      s.Rows,
			PID:       s.PID,
		})
	}

	env, err := protocol.NewEnvelope(protocol.TypeAgentHeartbeat, protocol.HeartbeatPayload{
		Sessions:      out,
		Update:        a.currentUpdateStatus(),
		Commands:      a.commandInventory(),
		DSHWebEnabled: &a.cfg.DSHWebEnabled,
		Roots:         a.ws.Roots(),
	})
	if err != nil {
		return err
	}
	return a.sendControl(env)
}

// ---------------------------------------------------------------------------
// 响应辅助
// ---------------------------------------------------------------------------

func (a *Agent) reply(req *protocol.Envelope, t protocol.Type, payload any) {
	env, err := protocol.NewReply(req, t, payload)
	if err != nil {
		a.log.Error("构造响应失败", "type", t, "err", err)
		return
	}
	if err := a.sendControl(env); err != nil {
		a.log.Warn("发送响应失败", "type", t, "err", err)
	}
}

func (a *Agent) replyError(req *protocol.Envelope, err error) {
	a.log.Warn("请求处理失败", "type", req.Type, "err", err)

	env := protocol.NewErrorEnvelope(req.RequestID, toCodeError(err))
	if serr := a.sendControl(env); serr != nil {
		a.log.Warn("发送错误响应失败", "err", serr)
	}
}

// toCodeError 把 Agent 的领域错误映射成**可以安全回传**的协议错误码。
//
// 为什么需要映射而不是直接把 err.Error() 发出去：
// 裸错误字符串里可能带本机路径、命令行的完整参数、甚至系统调用的细节。
// 那些信息对远程界面没有价值，但对攻击者是免费的侦察情报。
// 所以只有明确知道"这条消息是给用户看的"时才带上原文。
func toCodeError(err error) *protocol.CodeError {
	switch {
	case err == nil:
		return protocol.NewError(protocol.CodeInternal, "internal error")

	case errors.Is(err, ErrCommandNotAllowed), errors.Is(err, ErrCommandAmbiguous):
		return protocol.NewError(protocol.CodeCommandNotAllowed, err.Error())

	case errors.Is(err, ErrPathNotAllowed), errors.Is(err, ErrPathEmpty):
		return protocol.NewError(protocol.CodeCwdNotAllowed, err.Error())

	case errors.Is(err, session.ErrLimitReached):
		// 可重试：关掉一个旧会话再试就行。
		return protocol.NewRetryableError(protocol.CodeSessionLimit, err.Error())

	case errors.Is(err, session.ErrNotFound):
		return protocol.NewError(protocol.CodeSessionNotFound, err.Error())

	case errors.Is(err, session.ErrClosed):
		return protocol.NewError(protocol.CodeSessionNotFound, "会话已关闭")

	case errors.Is(err, session.ErrNotController), errors.Is(err, session.ErrReadOnlyViewer):
		return protocol.NewError(protocol.CodeForbidden, err.Error())

	case errors.Is(err, protocol.ErrInvalidPayload), errors.Is(err, protocol.ErrInvalidMessage):
		return protocol.NewError(protocol.CodeInvalidPayload, err.Error())

	case errors.Is(err, terminal.ErrUnsupported):
		return protocol.NewError(protocol.CodeForbidden, err.Error())

	default:
		// 兜底：不透传原文。
		return protocol.NewError(protocol.CodeInternal, "internal error")
	}
}

// ---------------------------------------------------------------------------
// view 跟踪
// ---------------------------------------------------------------------------
//
// 这份记录的唯一用途是：detach 时如果 Server 没给 RequestID，
// 能反查出该会话的全部 view 并逐个摘掉。它不参与任何业务判断 ——
// "会话还活着吗""谁能输入"这类问题一律以 session.Session 为准。

func (a *Agent) trackView(sid uuid.UUID, connID string) {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	m, ok := a.views[sid]
	if !ok {
		m = make(map[string]struct{}, 2)
		a.views[sid] = m
	}
	m[connID] = struct{}{}
}

func (a *Agent) untrackView(sid uuid.UUID, connID string) {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	m, ok := a.views[sid]
	if !ok {
		return
	}
	delete(m, connID)
	if len(m) == 0 {
		delete(a.views, sid)
	}
}

func (a *Agent) viewIDs(sid uuid.UUID) []string {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	m := a.views[sid]
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out
}

func (a *Agent) viewCount(sid uuid.UUID) int {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	return len(a.views[sid])
}

// forgetViews 在会话关闭时清掉记录，避免 map 随会话数量无限增长。
func (a *Agent) forgetViews(sid uuid.UUID) {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	delete(a.views, sid)
}
