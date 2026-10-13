package server

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

func taskTerminal(state string) bool {
	switch state {
	case "completed", "cancelled", "failed", "needs_review":
		return true
	}
	return false
}
func (s *Server) handleContinuations(w http.ResponseWriter, r *http.Request) {
	user, _ := userIDFromContext(r.Context())
	device := r.PathValue("id")
	if _, err := s.authorizeDevice(r.Context(), user, device); err != nil {
		writeProtoError(w, err)
		return
	}
	if r.Method == "GET" {
		rows, err := s.store.Continuations(r.Context(), user, device)
		if err != nil {
			writeProtoError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"tasks": rows})
		return
	}
	p, ok := decodeJSON[struct {
		MaxRetries       int    `json:"max_retries,omitempty"`
		RetryErrors      bool   `json:"retry_errors,omitempty"`
		QuotaWaitMinutes int    `json:"quota_wait_minutes,omitempty"`
		SessionID        string `json:"session_id"`
		NativeID         string `json:"native_id"`
		Prompt           string `json:"prompt"`
		Mode             string `json:"mode"`
		DueAt            int64  `json:"due_at"`
		MaxRunMinutes    int    `json:"max_run_minutes"`
		Nonce            string `json:"nonce"`
	}](w, r)
	if !ok {
		return
	}
	if _, err := uuid.Parse(p.Nonce); err != nil {
		writeError(w, 400, "invalid_payload", "任务提交标识无效")
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	if old, err := s.store.ContinuationByNonce(r.Context(), user, p.Nonce); err == nil {
		if old.DeviceID != device {
			writeProtoError(w, errNoAccess())
			return
		}
		writeJSON(w, 200, old)
		return
	}
	meta, err := s.authorizeSession(r.Context(), user, p.SessionID)
	if err != nil || meta.DeviceID != device {
		writeProtoError(w, errNoAccess())
		return
	}
	source := sessionSummary(meta)
	if source.Recovery == nil {
		writeError(w, 409, "native_identity_required", "此记录缺少CLI恢复配置，请先使用恢复原对话选择历史")
		return
	}
	binding := *source.Recovery
	source.Recovery = &binding
	if binding.NativeID == "" {
		binding.NativeID = p.NativeID
	} else if p.NativeID != "" && p.NativeID != binding.NativeID {
		writeError(w, 400, "invalid_payload", "不能替换已绑定的原生对话")
		return
	}
	if binding.NativeID == "" || len(binding.NativeID) > 200 || strings.ContainsAny(binding.NativeID, "/\\\x00\r\n") {
		writeError(w, 400, "invalid_payload", "请选择原生对话")
		return
	}
	if p.MaxRunMinutes == 0 {
		p.MaxRunMinutes = 120
	}
	if p.QuotaWaitMinutes == 0 {
		p.QuotaWaitMinutes = 300
	}
	if p.QuotaWaitMinutes < 1 || p.QuotaWaitMinutes > 10080 {
		writeError(w, 400, "invalid_payload", "额度等待时间应为1–10080分钟")
		return
	}
	if p.MaxRetries == 0 {
		p.MaxRetries = 3
	}
	now := s.now().UnixMilli()
	if (p.Mode != "time" && p.Mode != "quota" && p.Mode != "live_quota") || p.DueAt < now-60000 || p.DueAt > now+int64(30*24*time.Hour/time.Millisecond) || strings.TrimSpace(p.Prompt) == "" || len(p.Prompt) > 16<<10 || !utf8.ValidString(p.Prompt) || p.MaxRunMinutes < 1 || p.MaxRunMinutes > 360 || p.MaxRetries < 1 || p.MaxRetries > 10 {
		writeError(w, 400, "invalid_payload", "请检查继续时间、任务内容和运行上限")
		return
	}
	if p.Mode == "live_quota" {
		provider := liveSessionProvider(meta.Command, meta.Args)
		if !supportedLiveCommand(provider) || meta.Recovery == nil || meta.Recovery.NativeID == "" || binding.NativeID != meta.Recovery.NativeID || (meta.Status != "running" && meta.Status != "starting" && meta.Status != "detached") {
			writeError(w, 409, "unsupported_live_continuation", "自动续跑需要已绑定原生身份的运行中编程对话")
			return
		}
		ac, e := s.requireAgent(r.Context(), user, device)
		if e != nil || !supportsLiveProvider(ac.Caps(), provider) {
			writeError(w, 409, "agent_upgrade_required", "请更新并连接支持此CLI自动续跑的 Agent")
			return
		}
		p.DueAt = now
		rows, e := s.store.Continuations(r.Context(), user, device)
		if e != nil {
			writeProtoError(w, e)
			return
		}
		for _, row := range rows {
			if row.Mode == "live_quota" && !taskTerminal(row.State) && row.Source.Recovery != nil && row.Source.Recovery.NativeID == binding.NativeID {
				writeError(w, 409, "already_watching", "此原生对话已开启自动撞墙续跑，请先取消原任务")
				return
			}
		}
	}
	active, err := s.store.ActiveContinuationCount(r.Context(), user, device)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	if active >= 20 {
		writeError(w, 409, "task_limit", "每台设备最多保留20个未结束的续跑任务")
		return
	}
	task := &protocol.ContinuationTask{ID: uuid.NewString(), UserID: user, DeviceID: device, SessionID: meta.ID, Source: source, Prompt: p.Prompt, Mode: p.Mode, DueAt: p.DueAt, State: "queued", MaxAttempts: p.MaxRetries + 1, QuotaWaitMinutes: p.QuotaWaitMinutes, MaxRetries: p.MaxRetries, RetryErrors: p.RetryErrors, MaxRunMinutes: p.MaxRunMinutes, CreatedAt: now, UpdatedAt: now}
	if err = s.store.InsertContinuation(r.Context(), task, p.Nonce); err != nil {
		writeProtoError(w, err)
		return
	}
	writeJSON(w, 201, task)
}
func (s *Server) handleCancelContinuation(w http.ResponseWriter, r *http.Request) {
	user, _ := userIDFromContext(r.Context())
	device := r.PathValue("id")
	if _, err := s.authorizeDevice(r.Context(), user, device); err != nil {
		writeProtoError(w, err)
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	task, err := s.store.ContinuationByID(r.Context(), r.PathValue("task"))
	if err != nil || task.UserID != user || task.DeviceID != device {
		writeProtoError(w, errNoAccess())
		return
	}
	if !taskTerminal(task.State) {
		old := task.State
		task.State = "cancelled"
		task.Detail = "已取消等待"
		if old == "running" || old == "dispatching" || old == "cancelling" {
			task.State = "cancelling"
			task.Detail = "正在停止后台进程"
		}
		task.DueAt = s.now().UnixMilli()
		task.UpdatedAt = task.DueAt
		if _, err = s.store.UpdateContinuation(r.Context(), task, old); err != nil {
			writeProtoError(w, err)
			return
		}
	}
	writeJSON(w, 200, task)
}
func (s *Server) acceptTaskReply(ac *AgentConn, env *protocol.Envelope) bool {
	s.taskRPCMu.Lock()
	wait, ok := s.taskPending[env.ReplyTo]
	if ok && wait.agent == ac {
		delete(s.taskPending, env.ReplyTo)
	}
	s.taskRPCMu.Unlock()
	if ok && wait.agent == ac {
		wait.reply <- env
		return true
	}
	if env.Type != protocol.TypeTaskState || env.ReplyTo != "" {
		return false
	}
	p, err := protocol.DecodePayload[protocol.TaskStatePayload](env)
	if err == nil {
		s.applyTaskState(context.Background(), ac, p)
	}
	return true
}
func (s *Server) taskRPC(ctx context.Context, ac *AgentConn, typ protocol.Type, p protocol.TaskRunPayload) (protocol.TaskStatePayload, error) {
	id := uuid.NewString()
	wait := repositoryWait{agent: ac, reply: make(chan *protocol.Envelope, 1)}
	s.taskRPCMu.Lock()
	if s.taskPending == nil {
		s.taskPending = map[string]repositoryWait{}
	}
	s.taskPending[id] = wait
	s.taskRPCMu.Unlock()
	defer func() { s.taskRPCMu.Lock(); delete(s.taskPending, id); s.taskRPCMu.Unlock() }()
	env, err := protocol.NewRequest(id, typ, "", p)
	if err != nil {
		return protocol.TaskStatePayload{}, err
	}
	raw, err := protocol.Encode(env)
	if err != nil {
		return protocol.TaskStatePayload{}, err
	}
	if err = ac.TrySendText(raw); err != nil {
		return protocol.TaskStatePayload{}, err
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case reply := <-wait.reply:
		if reply.Type != protocol.TypeTaskState {
			return protocol.TaskStatePayload{}, errors.New("agent error")
		}
		return protocol.DecodePayload[protocol.TaskStatePayload](reply)
	case <-ctx.Done():
		return protocol.TaskStatePayload{}, ctx.Err()
	case <-ac.Done():
		return protocol.TaskStatePayload{}, errors.New("agent disconnected")
	case <-timer.C:
		return protocol.TaskStatePayload{}, errors.New("task reply timeout")
	}
}
func (s *Server) applyTaskState(ctx context.Context, ac *AgentConn, p protocol.TaskStatePayload) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, err := s.store.ContinuationByID(ctx, p.TaskID)
	if err != nil || t.DeviceID != ac.DeviceID || t.UserID != ac.UserID || t.Attempt != p.Attempt || taskTerminal(t.State) {
		return
	}
	if _, err = s.authorizeDevice(ctx, t.UserID, t.DeviceID); err != nil {
		return
	}
	switch p.State {
	case "running", "completed", "failed", "cancelled", "needs_review", "waiting_quota", "waiting_idle", "waiting_agent", "waiting_wall", "waiting_retry", "cancelling":
	default:
		return
	}
	if t.State == "cancelling" && p.State != "cancelled" && p.State != "completed" && p.State != "failed" && p.State != "needs_review" {
		return
	}
	old := t.State
	t.State = p.State
	t.Detail = p.Detail
	t.Output = p.Output
	t.Retry = p.Retry
	t.DueAt = s.now().Add(30 * time.Second).UnixMilli()
	if strings.HasPrefix(p.State, "waiting_") {
		if p.DueAt > t.DueAt {
			t.DueAt = p.DueAt
		}
		if t.Mode != "live_quota" && t.Attempt >= t.MaxAttempts {
			t.State = "failed"
			t.Detail = "已达到续跑尝试上限，请确认额度后重新安排"
		}
	}
	t.UpdatedAt = s.now().UnixMilli()
	_, _ = s.store.UpdateContinuation(ctx, t, old)
}
func (s *Server) pollContinuations(ctx context.Context) {
	rows, err := s.store.ReadyContinuations(ctx, s.now().UnixMilli())
	if err != nil {
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		s.dispatchContinuation(ctx, row.ID)
	}
}
func (s *Server) dispatchContinuation(ctx context.Context, id string) {
	s.tasksMu.Lock()
	t, err := s.store.ContinuationByID(ctx, id)
	if err != nil || taskTerminal(t.State) || t.DueAt > s.now().UnixMilli() {
		s.tasksMu.Unlock()
		return
	}
	old := t.State
	user, userErr := s.store.UserByID(ctx, t.UserID)
	if userErr != nil {
		t.DueAt = s.now().Add(time.Minute).UnixMilli()
		_, _ = s.store.UpdateContinuation(ctx, t, old)
		s.tasksMu.Unlock()
		return
	}
	disabled := user.Disabled
	if disabled && old != "running" && old != "dispatching" && old != "cancelling" {
		t.State = "cancelled"
		t.Detail = "账号已停用，取消后台续跑"
		_, _ = s.store.UpdateContinuation(ctx, t, old)
		s.tasksMu.Unlock()
		return
	}
	ac, err := s.requireAgent(ctx, t.UserID, t.DeviceID)
	if err != nil || !ac.Caps().BackgroundContinue {
		t.DueAt = s.now().Add(time.Minute).UnixMilli()
		if old != "running" && old != "dispatching" && old != "cancelling" {
			t.State = "waiting_agent"
		}
		t.Detail = "等待设备上线并支持后台续跑"
		_, _ = s.store.UpdateContinuation(ctx, t, old)
		s.tasksMu.Unlock()
		return
	}
	typ := protocol.TypeTaskRun
	if disabled {
		typ = protocol.TypeTaskCancel
		t.State = "cancelling"
	} else if old == "running" || old == "dispatching" {
		typ = protocol.TypeTaskInspect
	} else if old == "cancelling" {
		typ = protocol.TypeTaskCancel
	} else {
		meta, e := s.authorizeSession(ctx, t.UserID, t.SessionID)
		if e != nil || meta.DeviceID != t.DeviceID {
			t.State = "failed"
			t.Detail = "原会话记录或设备授权已不存在"
			_, _ = s.store.UpdateContinuation(ctx, t, old)
			s.tasksMu.Unlock()
			return
		}
		if t.Mode != "live_quota" && t.Attempt >= t.MaxAttempts {
			t.State = "failed"
			t.Detail = "已达到尝试上限"
			_, _ = s.store.UpdateContinuation(ctx, t, old)
			s.tasksMu.Unlock()
			return
		}
		// Waiting responses precede spawning and have no receipt, so reuse their attempt.
		if t.Attempt == 0 || old == "queued" || t.Retry {
			t.Attempt++
		}
		t.Retry = false
		t.State = "dispatching"
	}
	t.DueAt = s.now().Add(30 * time.Second).UnixMilli()
	t.UpdatedAt = s.now().UnixMilli()
	changed, e := s.store.UpdateContinuation(ctx, t, old)
	s.tasksMu.Unlock()
	if e != nil || !changed {
		return
	}
	p := protocol.TaskRunPayload{TaskID: t.ID, UserID: t.UserID, Attempt: t.Attempt, Source: t.Source, Prompt: t.Prompt, Mode: t.Mode, MaxRunMinutes: t.MaxRunMinutes, QuotaWaitMinutes: t.QuotaWaitMinutes, MaxRetries: t.MaxRetries, RetryErrors: t.RetryErrors}
	response, e := s.taskRPC(ctx, ac, typ, p)
	if e != nil {
		// Keep the existing attempt for receipt inspection after reconnect. Neither
		// a lost acknowledgement nor a planned Server restart resubmits the prompt.
		select {
		case <-s.cleanupStop:
			return
		default:
		}
		s.tasksMu.Lock()
		current, readErr := s.store.ContinuationByID(ctx, t.ID)
		if readErr == nil && current.Attempt == t.Attempt && !taskTerminal(current.State) {
			expected := current.State
			current.Detail = "等待核对执行回执；不会重新发送指令"
			current.DueAt = s.now().Add(30 * time.Second).UnixMilli()
			_, _ = s.store.UpdateContinuation(ctx, current, expected)
		}
		s.tasksMu.Unlock()
		return
	}
	if response.TaskID != t.ID || response.Attempt != t.Attempt {
		response = protocol.TaskStatePayload{TaskID: t.ID, Attempt: t.Attempt, State: "needs_review", Detail: "执行回执身份不一致"}
	}
	s.applyTaskState(ctx, ac, response)
}
func (s *Server) startTaskLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			s.pollContinuations(ctx)
			select {
			case <-ctx.Done():
				return
			case <-s.cleanupStop:
				return
			case <-ticker.C:
			}
		}
	}()
}

func supportedLiveCommand(id string) bool {
	return id == "codex" || id == "claude" || id == "opencode" || id == "dsh"
}
func supportsLiveProvider(c protocol.AgentCaps, id string) bool {
	if id == "codex" && c.CodexLiveContinue {
		return true
	}
	for _, v := range c.LiveContinueProviders {
		if v == id {
			return true
		}
	}
	return false
}

func liveSessionProvider(command string, args []string) string {
	base := func(value string) string {
		name := strings.ToLower(filepath.Base(strings.ReplaceAll(value, `\`, "/")))
		for _, suffix := range []string{".exe", ".cmd", ".bat"} {
			name = strings.TrimSuffix(name, suffix)
		}
		return name
	}
	name := base(command)
	if name == "cmd" {
		name = ""
		for i, arg := range args {
			if strings.EqualFold(arg, "/c") && i+1 < len(args) {
				name = base(args[i+1])
				break
			}
		}
	}
	return name
}
