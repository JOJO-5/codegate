package agent

// Live Codex continuation uses the existing native daemon and structured turn
// state. It never scrapes terminal output, types into the PTY, starts another
// model process, spends reset credits, or changes approval/model settings.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
)

type liveQuotaReceipt struct {
	Detached        bool   `json:"detached,omitempty"`
	ControlFailures int    `json:"control_failures,omitempty"`
	TurnID          string `json:"turn_id"`
	InputRevision   uint64 `json:"input_revision"`
	Runs            int    `json:"runs"`
	OwnTurnID       string `json:"own_turn_id,omitempty"`
	StartedAt       int64  `json:"started_at,omitempty"`
	Preparing       bool   `json:"preparing,omitempty"`
	RetryAt         int64  `json:"retry_at,omitempty"`
	NextQuotaAt     int64  `json:"next_quota_at,omitempty"`
}
type nativeTurn struct {
	RetryAt int64  `json:"retryAt,omitempty"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Error   *struct {
		Info json.RawMessage `json:"codexErrorInfo"`
	} `json:"error"`
}
type nativeThread struct {
	ID     string `json:"id"`
	Cwd    string `json:"cwd"`
	Status struct {
		Type string `json:"type"`
	} `json:"status"`
}
type codexControl struct {
	conn *websocket.Conn
	next int
	stop func() bool
}

type codexControlUnavailable struct{ cause error }

func (e *codexControlUnavailable) Error() string { return "Codex 本机控制服务暂时不可连接" }
func (e *codexControlUnavailable) Unwrap() error { return e.cause }

func openCodexControl(ctx context.Context, home string) (*codexControl, error) {
	// The path is derived from the approved CLI environment. No remote endpoint
	// or daemon startup is accepted from a web request.
	path := filepath.Join(home, "app-server-control", "app-server-control.sock")
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return nil, &codexControlUnavailable{cause: e}
	}
	if e != nil || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Codex本机守护进程不可用，请更新Codex并使用默认守护进程启动对话")
	}
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	ws, _, e := d.DialContext(ctx, "ws://localhost/", nil)
	if e != nil {
		return nil, &codexControlUnavailable{cause: e}
	}
	ws.SetReadLimit(4 << 20)
	deadline, _ := ctx.Deadline()
	_ = ws.SetReadDeadline(deadline)
	_ = ws.SetWriteDeadline(deadline)
	client := &codexControl{conn: ws, stop: context.AfterFunc(ctx, func() { ws.Close() })}
	var result json.RawMessage
	if e = client.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codegate_live_quota", "version": Version}, "capabilities": map[string]any{"experimentalApi": true}}, &result); e != nil {
		ws.Close()
		return nil, e
	}
	if e = ws.WriteJSON(map[string]any{"method": "initialized"}); e != nil {
		ws.Close()
		return nil, e
	}
	return client, nil
}
func (c *codexControl) close() {
	if c.stop != nil {
		c.stop()
	}
	c.conn.Close()
}
func (c *codexControl) call(method string, params any, out any) error {
	c.next++
	id := c.next
	if e := c.conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); e != nil {
		return e
	}
	for n := 0; n < 256; n++ {
		var frame struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if e := c.conn.ReadJSON(&frame); e != nil {
			return e
		}
		if frame.ID != id {
			continue
		}
		if len(frame.Error) > 0 && string(frame.Error) != "null" {
			return fmt.Errorf("Codex本机接口不支持或拒绝操作：%s", method)
		}
		if len(frame.Result) == 0 {
			return errors.New("Codex返回了空响应")
		}
		return json.Unmarshal(frame.Result, out)
	}
	return errors.New("Codex事件过多，稍后核对")
}
func (c *codexControl) readThread(id string) (nativeThread, nativeTurn, error) {
	thread, e := c.readThreadStatus(id)
	if e != nil || thread.Status.Type == "notLoaded" {
		return thread, nativeTurn{}, e
	}
	var turns struct {
		Data []nativeTurn `json:"data"`
	}
	e = c.call("thread/turns/list", map[string]any{"threadId": id, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &turns)
	if e != nil {
		return thread, nativeTurn{}, e
	}
	if len(turns.Data) != 1 || turns.Data[0].ID == "" {
		return thread, nativeTurn{}, errors.New("尚无可监测的Codex回合，请先开始任务")
	}
	return thread, turns.Data[0], nil
}

func (c *codexControl) readThreadStatus(id string) (nativeThread, error) {
	var read struct {
		Thread nativeThread `json:"thread"`
	}
	e := c.call("thread/read", map[string]any{"threadId": id, "includeTurns": false}, &read)
	return read.Thread, e
}

// A closed TUI can leave an active writer in the shared daemon. When the
// daemon already owns this thread, submit there instead of opening a writer.
func (a *Agent) codexOwnsScheduledThread(p protocol.TaskRunPayload, env []string) bool {
	if checkCodexLiveArgs(p.Source.Args) != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(a.operationContext(), 4*time.Second)
	defer cancel()
	control, err := openCodexControl(ctx, codexHome(env))
	if err != nil {
		return false
	}
	defer control.close()
	thread, err := control.readThreadStatus(p.Source.Recovery.NativeID)
	return err == nil && thread.ID == p.Source.Recovery.NativeID && sameDirectory(thread.Cwd, p.Source.Cwd) && thread.Status.Type != "notLoaded"
}
func usageWall(t nativeTurn) bool {
	return t.Status == "failed" && t.Error != nil && (string(t.Error.Info) == `"usageLimitExceeded"` || string(t.Error.Info) == `"usage_limit_exceeded"`)
}

type codexLiveQuota struct {
	Allowed    *bool `json:"ordinaryUsageAllowed"`
	RateLimits struct {
		Primary   *codexQuotaLane `json:"primary"`
		Secondary *codexQuotaLane `json:"secondary"`
	} `json:"rateLimits"`
}

func (c *codexControl) readUsage() (codexLiveQuota, error) {
	var q codexLiveQuota
	err := c.call("account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true}, &q)
	return q, err
}

func (c *codexControl) usageAllowed() (bool, error) {
	q, e := c.readUsage()
	if e != nil {
		return false, e
	}
	// Reset timestamps and low percentages alone are not proof of entitlement.
	if q.Allowed == nil {
		return false, errors.New("Codex未提供可靠的额度恢复许可，请升级CLI；不会自动消费额外额度")
	}
	return *q.Allowed, nil
}

func scheduledLiveQuota(native liveControl) (allowed bool, dueAt int64, err error) {
	var windows []protocol.QuotaWindow
	allowed = true
	switch control := native.(type) {
	case *codexControl:
		usage, err := control.readUsage()
		if err != nil || usage.Allowed == nil {
			return false, 0, errors.New("无法确认 Codex 订阅额度，请检查本机登录和 CLI 后重新安排")
		}
		allowed = *usage.Allowed
		for _, lane := range []*codexQuotaLane{usage.RateLimits.Primary, usage.RateLimits.Secondary} {
			if lane != nil {
				if window, ok := quotaWindow("", lane.Used, lane.Reset); ok {
					windows = append(windows, window)
				}
			}
		}
	case *eventControl:
		windows, err = control.subscriptionWindows()
	case *openCodeV2Control:
		s, e := control.client.session(control.ctx, control.id)
		if e != nil {
			return false, 0, errors.New("无法核对 OpenCode 当前会话的模型")
		}
		event := &eventControl{a: control.a, ctx: control.ctx, tool: "opencode"}
		event.last.Model.ProviderID = s.Model.ProviderID
		windows, err = event.subscriptionWindows()
	}
	if err != nil {
		return false, 0, err
	}
	if _, codex := native.(*codexControl); !codex && len(windows) == 0 {
		return false, 0, errors.New("此会话无法可靠查询订阅额度，可重新选择按时间继续")
	}
	q := protocol.QuotaResult{Providers: []protocol.ProviderQuota{{Status: "ok", Windows: windows}}}
	dueAt = blockedUntil(q, time.Now()) * 1000
	if !allowed && dueAt == 0 {
		dueAt = time.Now().Add(time.Minute).UnixMilli()
	}
	return allowed && dueAt == 0, dueAt, nil
}

func (a *Agent) onLiveContinuation(req *protocol.Envelope, p protocol.TaskRunPayload, cancel, inspect bool, openSessionID string) {
	a.liveQuotaMu.Lock()
	defer a.liveQuotaMu.Unlock()
	result := protocol.TaskStatePayload{TaskID: p.TaskID, Attempt: p.Attempt, State: "failed"}
	reply := func() { a.reply(req, protocol.TypeTaskState, result) }
	fail := func(s string) { result.State = "needs_review"; result.Detail = s; reply() }
	scheduled := p.PreferOpenSession && (p.Mode == "time" || p.Mode == "quota")
	if (!scheduled && p.Mode != "live_quota") || p.Source.Recovery == nil || p.Source.Recovery.NativeID == "" || p.UserID == "" {
		fail("自动续跑配置无效")
		return
	}
	receipt, e := a.taskReceipt(p.TaskID, p.Attempt, p.UserID)
	exists := e == nil
	if e != nil && !os.IsNotExist(e) {
		fail("不能核对自动续跑回执")
		return
	}
	if exists && (receipt.Hash != continuationHash(p) || receipt.Live == nil) {
		fail("任务内容与已保存回执不一致")
		return
	}
	if exists {
		result = receipt.Result
		if result.State == "completed" || result.State == "failed" || result.State == "cancelled" || result.State == "needs_review" {
			reply()
			return
		}
	}
	if inspect && !exists && !cancel {
		fail("没有可核对的回执，不会重复发送继续指令")
		return
	}
	save := func() bool {
		receipt.Result = result
		if e := a.saveTaskReceipt(receipt); e != nil {
			fail("不能保存自动续跑状态，未重复执行")
			return false
		}
		if receipt.Live.Detached && (result.State == "completed" || result.State == "failed" || result.State == "cancelled") {
			a.releaseDetachedRun(p)
		}
		return true
	}
	// Cancellation without an armed receipt has no side effect to undo.
	if cancel && (!exists || !receipt.Live.Preparing && receipt.Live.OwnTurnID == "") {
		result.State = "cancelled"
		result.Detail = "已关闭自动续跑监测，保留原终端和对话"
		if exists && !save() {
			return
		}
		reply()
		return
	}
	sessionID := p.Source.SessionID
	detached := scheduled && !inspect && openSessionID == ""
	if scheduled {
		sessionID = openSessionID
		if exists {
			sessionID = receipt.Source.SessionID
			detached = receipt.Live.Detached
		}
	}
	var sess *session.Session
	current := p.Source
	if !detached {
		sess, _ = a.mgr.Get(parseUUID(sessionID))
	}
	if !detached && (sess == nil || !isRecoverableLive(sess.Summary())) {
		if scheduled && !exists {
			result.State = "waiting_idle"
			result.Detail = "原终端刚结束，等待后台恢复原对话"
			reply()
		} else if scheduled {
			fail("原终端已结束，请核对原对话中的续跑结果")
		} else if exists {
			result.State = "cancelled"
			result.Detail = "原终端已结束，自动续跑已关闭"
			if !save() {
				return
			}
			reply()
		} else {
			fail("原终端不存在或已经结束")
		}
		return
	}
	inputRevision := func() uint64 {
		if sess == nil {
			return 0
		}
		return sess.InputRevision()
	}
	if sess != nil {
		current = sess.Summary()
	}
	initialRevision := inputRevision()
	// Production terminals leave UserID unset: the authenticated Server owns
	// device/session authorization. Enforce a local owner only when present.
	if (sess != nil && sess.UserID != uuid.Nil && sess.UserID.String() != p.UserID) || !sameNativeTask(current, p.Source) || !supportedLiveTool(current.Command, current.Args) {
		fail("原终端与Codex原生对话身份不一致")
		return
	}
	if exists && receipt.Live.Preparing {
		fail("上次提交结果无法确认，请核对原对话；不会再次提交")
		return
	}
	if exists && inputRevision() != receipt.Live.InputRevision {
		result.State = "cancelled"
		result.Detail = "检测到手动输入，已关闭自动续跑"
		if !save() {
			return
		}
		reply()
		return
	}
	commandID, command, e := a.recoveryCommand(current)
	if e != nil {
		fail("原Codex命令授权已变更")
		return
	}
	if scheduled {
		sourceID, _, err := a.recoveryCommand(p.Source)
		if err != nil || commandID != sourceID || !sameDirectory(current.Cwd, p.Source.Cwd) {
			fail("当前原对话的工作目录或 CLI 配置与预约不一致")
			return
		}
		if quotaProvider(current.Command, current.Args) == "codex" {
			if err := checkCodexLiveArgs(current.Args); err != nil {
				fail(err.Error())
				return
			}
		}
	}
	if _, e = a.ws.Resolve(current.Cwd); e != nil {
		fail("工作目录已不在授权范围")
		return
	}
	env := BuildEnv(a.cfg, 80, 24)
	finish, e := a.beginActivity()
	if e != nil {
		fail("Agent正在更新，未自动提交")
		return
	}
	defer finish()
	a.quotaMu.Lock()
	parent := a.quotaCtx
	closing := a.quotaClosing
	a.quotaMu.Unlock()
	if closing {
		fail("Agent正在退出，未自动提交")
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := context.WithTimeout(parent, 10*time.Second)
	defer stop()
	native, e := a.openLiveControl(ctx, current, env, p)
	if e != nil {
		var unavailable *codexControlUnavailable
		if !scheduled && !cancel && (!exists || receipt.Live.OwnTurnID == "") && errors.As(e, &unavailable) {
			if !exists {
				receipt = taskReceipt{UserID: p.UserID, Hash: continuationHash(p), Source: p.Source, Live: &liveQuotaReceipt{InputRevision: inputRevision()}}
			}
			receipt.Live.ControlFailures++
			result.State = "waiting_retry"
			result.DueAt = time.Now().Add(30 * time.Second).UnixMilli()
			result.Detail = "Codex 本机控制服务暂不可连接，30秒后重新核对；未提交继续指令"
			if receipt.Live.ControlFailures > 3 {
				result.State = "needs_review"
				result.Detail = "连续4次无法连接 Codex 本机控制服务，请检查守护进程并从原终端恢复对话后重新开启监测"
			}
			if save() {
				reply()
			}
			return
		}
		fail(e.Error())
		return
	}
	defer native.close()
	thread, turn, e := native.readThread(current.Recovery.NativeID)
	if e != nil {
		fail(e.Error())
		return
	}
	if detached && !exists && thread.Status.Type == "notLoaded" {
		result.State = "waiting_idle"
		result.Detail = "Codex 已释放原对话，等待后台恢复"
		reply()
		return
	}
	if thread.ID != current.Recovery.NativeID || !sameDirectory(thread.Cwd, current.Cwd) || thread.Status.Type == "notLoaded" {
		fail("原对话未在当前Codex守护进程中加载，未恢复或新建其他对话")
		return
	}
	if !exists || receipt.Live.TurnID == "" {
		receipt = taskReceipt{UserID: p.UserID, Hash: continuationHash(p), Source: p.Source, Live: &liveQuotaReceipt{TurnID: turn.ID, InputRevision: inputRevision(), Detached: detached}}
		if scheduled {
			receipt.Source = current
			receipt.Live.InputRevision = initialRevision
		}
	}
	receipt.Live.ControlFailures = 0
	live := receipt.Live
	if cancel {
		if live.OwnTurnID == turn.ID && turn.Status == "inProgress" {
			var out json.RawMessage
			if e = native.call("turn/interrupt", map[string]any{"threadId": thread.ID, "turnId": turn.ID}, &out); e != nil {
				fail("已请求关闭自动续跑，但无法确认本次自动回合停止；请在原终端核对")
				return
			}
		}
		result.State = "cancelled"
		result.Detail = "已关闭自动续跑，保留原终端和对话"
		if !save() {
			return
		}
		reply()
		return
	}
	if turn.ID != live.TurnID {
		result.State = "cancelled"
		result.Detail = "原对话已产生新回合，已让出控制，不再自动提交"
		if !save() {
			return
		}
		reply()
		return
	}
	if scheduled && !exists {
		if turn.Status == "inProgress" || thread.Status.Type == "active" {
			result.State = "waiting_idle"
			result.Detail = "原对话正在执行，空闲后继续"
			reply()
			return
		}
		if !scheduledTurnReady(thread, turn) {
			fail("无法确认原对话已空闲，请核对原终端")
			return
		}
		if p.Mode == "quota" {
			if !continuationQuotaEligible(command) {
				fail("此 CLI 配置无法可靠确认订阅额度，请重新按时间安排")
				return
			}
			allowed, due, err := scheduledLiveQuota(native)
			if err != nil {
				fail(err.Error())
				return
			}
			if !allowed {
				result.State = "waiting_quota"
				result.DueAt = due
				result.Detail = "等待原对话的订阅额度恢复"
				reply()
				return
			}
		}
	} else if turn.Status == "inProgress" {
		result.State = "waiting_wall"
		result.Detail = "已开启额度墙监测，原任务继续运行"
		if live.OwnTurnID == turn.ID {
			result.State = "running"
			result.Detail = "正在原终端自动继续"
			if time.Now().UnixMilli() > live.StartedAt+int64(p.MaxRunMinutes)*60000 {
				var out json.RawMessage
				if e = native.call("turn/interrupt", map[string]any{"threadId": thread.ID, "turnId": turn.ID}, &out); e != nil {
					fail("本次运行超时，无法确认停止，请检查原终端")
					return
				}
				result.State = "failed"
				result.Detail = "本次自动续跑达到运行上限，已停止回合，保留终端"
			}
		}
		if !save() {
			return
		}
		reply()
		return
	}
	transient := p.RetryErrors && retryableNativeError(turn)
	if scheduled && live.OwnTurnID != "" || !scheduled && !usageWall(turn) && !transient {
		result.State = "completed"
		result.Detail = "当前回合已结束，自动续跑已关闭"
		if turn.Status == "failed" {
			result.State = "failed"
			result.Detail = "当前回合失败且不属于可自动重试的错误，请核对原对话"
		}
		if scheduled {
			result.Detail = "本次预约回合已结束，请核对原对话"
			if turn.Status == "failed" {
				result.Detail = "本次预约回合执行失败，请核对原对话"
			} else if turn.Status == "interrupted" {
				result.Detail = "本次预约回合已中断"
			}
		}
		if turn.Status == "interrupted" {
			result.State = "cancelled"
			result.Detail = "原回合已中断，自动续跑已关闭"
		}
		if !save() {
			return
		}
		reply()
		return
	}
	limit := p.MaxRetries
	if limit == 0 {
		limit = 3
	}
	if live.Runs >= limit {
		result.State = "failed"
		result.Detail = fmt.Sprintf("已达到%d次自动继续上限，请核对原任务", limit)
		if !save() {
			return
		}
		reply()
		return
	}
	if !scheduled {
		result.State = "waiting_quota"
		result.Detail = "已确认订阅额度耗尽，等待可靠恢复许可；保留原终端"
		if !save() {
			return
		}
		if transient {
			if live.RetryAt == 0 {
				live.RetryAt = max(turn.RetryAt, time.Now().Add(retryDelay(live.Runs+1)).UnixMilli())
			}
			result.State = "waiting_retry"
			result.Detail = "临时网络、限流或服务端故障，等待退避后重试"
			if !save() {
				return
			}
			if time.Now().UnixMilli() < live.RetryAt {
				reply()
				return
			}
		} else {
			if time.Now().UnixMilli() < live.NextQuotaAt {
				reply()
				return
			}
			allowed, e := native.usageAllowed()
			if e != nil {
				live.NextQuotaAt = time.Now().Add(time.Minute).UnixMilli()
				result.Detail = e.Error() + "；继续等待，不提交指令"
				if !save() {
					return
				}
				reply()
				return
			}
			if !allowed {
				live.NextQuotaAt = time.Now().Add(time.Minute).UnixMilli()
				if !save() {
					return
				}
				reply()
				return
			}
		}
	}
	// Re-read immediately before submitting: a new manual/native turn always wins.
	thread, latest, e := native.readThread(thread.ID)
	ready := usageWall(latest) || p.RetryErrors && retryableNativeError(latest)
	if scheduled {
		ready = scheduledTurnReady(thread, latest)
	}
	if e != nil || latest.ID != turn.ID || !ready || thread.ID != current.Recovery.NativeID || !sameDirectory(thread.Cwd, current.Cwd) || (thread.Status.Type != "idle" && thread.Status.Type != "systemError") || inputRevision() != live.InputRevision || sess != nil && !isRecoverableLive(sess.Summary()) {
		result.State = "cancelled"
		result.Detail = "原对话状态已变化，已取消自动提交"
		if !save() {
			return
		}
		reply()
		return
	}
	live.Preparing = true
	result.State = "dispatching"
	result.Detail = "正在向同一原生对话提交继续指令"
	if !save() {
		return
	}
	var started struct {
		Turn nativeTurn `json:"turn"`
	}
	e = native.call("turn/start", map[string]any{"threadId": thread.ID, "clientUserMessageId": p.TaskID + "-" + itoa(live.Runs+1), "input": []map[string]any{{"type": "text", "text": p.Prompt, "textElements": []any{}}}}, &started)
	if e != nil || started.Turn.ID == "" {
		result.State = "needs_review"
		result.Detail = "提交结果无法确认，请检查原对话；不会重复提交"
		if !save() {
			return
		}
		reply()
		return
	}
	live.Preparing = false
	live.RetryAt = 0
	live.NextQuotaAt = 0
	live.Runs++
	live.OwnTurnID = started.Turn.ID
	live.TurnID = started.Turn.ID
	live.StartedAt = time.Now().UnixMilli()
	result.State = "running"
	result.Detail = "已在原对话自动继续，终端与模型设置保留"
	if scheduled {
		result.Detail = "已在打开的原对话提交预约指令"
		if detached {
			result.Detail = "已通过 Codex 本机服务在后台继续原对话"
		}
	}
	if !save() {
		return
	}
	a.trackLiveRun(p, receipt)
	reply()
}

func scheduledTurnReady(thread nativeThread, turn nativeTurn) bool {
	return thread.Status.Type == "idle" && (turn.Status == "completed" || turn.Status == "failed" || turn.Status == "interrupted") ||
		thread.Status.Type == "systemError" && usageWall(turn)
}

func retryDelay(attempt int) time.Duration {
	delays := []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute}
	return delays[min(max(attempt-1, 0), len(delays)-1)]
}
func retryableNativeError(t nativeTurn) bool {
	if t.Status != "failed" || t.Error == nil {
		return false
	}
	var name string
	if json.Unmarshal(t.Error.Info, &name) == nil {
		switch name {
		case "rateLimitExceeded", "serverOverloaded", "responseStreamDisconnected", "responseStreamConnectionFailed", "responseTooManyFailedAttempts":
			return true
		}
		return false
	}
	var variants map[string]struct {
		HTTPStatus int `json:"httpStatusCode"`
	}
	if json.Unmarshal(t.Error.Info, &variants) != nil {
		return false
	}
	for name, v := range variants {
		switch name {
		case "httpConnectionFailed", "responseStreamDisconnected", "responseStreamConnectionFailed", "responseTooManyFailedAttempts":
			return v.HTTPStatus == 0 || v.HTTPStatus == 408 || v.HTTPStatus == 429 || v.HTTPStatus >= 500 && v.HTTPStatus <= 599
		}
	}
	return false
}

func supportedLiveTool(cmd string, args []string) bool {
	tool := quotaProvider(cmd, args)
	return tool != "" || nativeTool(ResolvedCommand{Command: cmd, Args: args}) == "dsh"
}
