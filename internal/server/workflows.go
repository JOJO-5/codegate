package server

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Server) workflowRPC(ctx context.Context, ac *AgentConn, p protocol.WorkflowControl) (protocol.WorkflowReply, error) {
	id := uuid.NewString()
	wait := repositoryWait{agent: ac, reply: make(chan *protocol.Envelope, 1)}
	s.taskRPCMu.Lock()
	if s.workflowPending == nil {
		s.workflowPending = map[string]repositoryWait{}
	}
	s.workflowPending[id] = wait
	s.taskRPCMu.Unlock()
	defer func() { s.taskRPCMu.Lock(); delete(s.workflowPending, id); s.taskRPCMu.Unlock() }()
	env, err := protocol.NewRequest(id, protocol.TypeWorkflowControl, "", p)
	if err != nil {
		return protocol.WorkflowReply{}, err
	}
	raw, err := protocol.Encode(env)
	if err != nil {
		return protocol.WorkflowReply{}, err
	}
	if err = ac.TrySendText(raw); err != nil {
		return protocol.WorkflowReply{}, err
	}
	timer := time.NewTimer(40 * time.Second)
	defer timer.Stop()
	select {
	case reply := <-wait.reply:
		if reply.Type != protocol.TypeWorkflowState {
			return protocol.WorkflowReply{}, errors.New("Agent 无法处理调度请求，请检查本机配置")
		}
		return protocol.DecodePayload[protocol.WorkflowReply](reply)
	case <-ctx.Done():
		return protocol.WorkflowReply{}, ctx.Err()
	case <-ac.Done():
		return protocol.WorkflowReply{}, errors.New("Agent 已离线")
	case <-timer.C:
		return protocol.WorkflowReply{}, errors.New("Agent 响应超时")
	}
}
func (s *Server) acceptWorkflowReply(ac *AgentConn, env *protocol.Envelope) bool {
	if env.ReplyTo != "" {
		s.taskRPCMu.Lock()
		wait, ok := s.workflowPending[env.ReplyTo]
		if ok && wait.agent == ac {
			delete(s.workflowPending, env.ReplyTo)
		}
		s.taskRPCMu.Unlock()
		if !ok || wait.agent != ac {
			return false
		}
		wait.reply <- env
		return true
	}
	if env.Type != protocol.TypeWorkflowState {
		return false
	}
	p, err := protocol.DecodePayload[protocol.WorkflowReply](env)
	if err == nil && p.Workflow != nil {
		s.applyWorkflow(ac, p.Workflow)
	}
	return true
}
func (s *Server) handleWorkflowProfiles(w http.ResponseWriter, r *http.Request) {
	user, _ := userIDFromContext(r.Context())
	ac, err := s.requireAgent(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeProtoError(w, err)
		return
	}
	if !ac.Caps().WorkflowV1 {
		writeError(w, 409, "agent_upgrade_required", "请更新 Agent 后使用主 Agent 调度")
		return
	}
	response, err := s.workflowRPC(r.Context(), ac, protocol.WorkflowControl{Action: "catalog", UserID: user})
	if err != nil {
		writeError(w, 503, "workflow_catalog_unavailable", err.Error())
		return
	}
	writeJSON(w, 200, response)
}
func (s *Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	user, _ := userIDFromContext(r.Context())
	device := r.PathValue("id")
	if _, err := s.authorizeDevice(r.Context(), user, device); err != nil {
		writeProtoError(w, err)
		return
	}
	if r.Method == "GET" {
		rows, err := s.store.Workflows(r.Context(), user, device)
		if err != nil {
			writeProtoError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"workflows": rows})
		return
	}
	p, ok := decodeJSON[struct {
		Nonce       string `json:"nonce"`
		Goal        string `json:"goal"`
		Cwd         string `json:"cwd"`
		MasterID    string `json:"master_id"`
		FreeWorkers *bool  `json:"free_workers"`
		MaxTurns    int    `json:"max_turns"`
		MaxMinutes  int    `json:"max_minutes"`
	}](w, r)
	if !ok {
		return
	}
	if _, err := uuid.Parse(p.Nonce); err != nil {
		writeError(w, 400, "invalid_payload", "调度提交标识无效")
		return
	}
	if p.MaxTurns == 0 {
		p.MaxTurns = 12
	}
	if p.MaxMinutes == 0 {
		p.MaxMinutes = 30
	}
	if strings.TrimSpace(p.Goal) == "" || len(p.Goal) > 8000 || !utf8.ValidString(p.Goal) || p.Cwd == "" || len(p.Cwd) > 4096 || len(p.MasterID) == 0 || len(p.MasterID) > 200 || p.MaxTurns < 2 || p.MaxTurns > 32 || p.MaxMinutes < 1 || p.MaxMinutes > 180 {
		writeError(w, 400, "invalid_payload", "请检查目标、项目、主 Agent 和运行上限")
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	free := true
	if p.FreeWorkers != nil {
		free = *p.FreeWorkers
	}
	if old, err := s.store.WorkflowByNonce(r.Context(), user, p.Nonce); err == nil {
		if old.DeviceID != device {
			writeProtoError(w, errNoAccess())
			return
		}
		if old.Goal != strings.TrimSpace(p.Goal) || old.Cwd != p.Cwd || old.MasterID != p.MasterID || old.FreeWorkers != free || old.MaxTurns != p.MaxTurns || old.MaxMinutes != p.MaxMinutes {
			writeError(w, 409, "workflow_request_changed", "本次提交的目标或运行范围已改变，请重新核对后提交")
			return
		}
		writeJSON(w, 200, old)
		return
	} else if !errors.Is(err, storage.ErrNotFound) {
		writeProtoError(w, err)
		return
	}
	rows, err := s.store.Workflows(r.Context(), user, device)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	count := 0
	for _, row := range rows {
		if !protocol.WorkflowTerminal(row.State) {
			count++
		}
	}
	if count >= 5 {
		writeError(w, 409, "workflow_limit", "每台设备最多保留5个未结束的调度，请先取消或完成已有任务")
		return
	}
	row := &protocol.Workflow{ID: uuid.NewString(), UserID: user, DeviceID: device, Goal: strings.TrimSpace(p.Goal), Cwd: p.Cwd, MasterID: p.MasterID, FreeWorkers: free, MaxTurns: p.MaxTurns, MaxMinutes: p.MaxMinutes, State: "queued", Detail: "等待主 Agent 开始调度", Revision: 1, Steps: []protocol.WorkflowStep{}, CreatedAt: s.now().UnixMilli(), UpdatedAt: s.now().UnixMilli()}
	if err = s.store.InsertWorkflow(r.Context(), row, p.Nonce); err != nil {
		writeProtoError(w, err)
		return
	}
	writeJSON(w, 201, row)
}
func (s *Server) handleWorkflowCancel(w http.ResponseWriter, r *http.Request) {
	user, _ := userIDFromContext(r.Context())
	device := r.PathValue("id")
	if _, err := s.authorizeDevice(r.Context(), user, device); err != nil {
		writeProtoError(w, err)
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	row, err := s.store.Workflow(r.Context(), r.PathValue("workflow"))
	if err != nil || row.UserID != user || row.DeviceID != device {
		writeProtoError(w, errNoAccess())
		return
	}
	if protocol.WorkflowTerminal(row.State) {
		writeJSON(w, 200, row)
		return
	}
	expected := row.Revision
	row.Revision++
	row.UpdatedAt = s.now().UnixMilli()
	if row.State == "queued" || row.State == "waiting_agent" || row.State == "waiting_idle" {
		row.State = "cancelled"
		row.Detail = "已取消尚未派发的调度"
	} else {
		row.State = "cancelling"
		row.Detail = "等待 Agent 确认执行停止；已有代码保留"
	}
	if _, err = s.store.UpdateWorkflow(r.Context(), row, expected); err != nil {
		writeProtoError(w, err)
		return
	}
	writeJSON(w, 200, row)
}
func (s *Server) applyWorkflow(ac *AgentConn, result *protocol.Workflow) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	ctx := context.Background()
	row, err := s.store.Workflow(ctx, result.ID)
	if err != nil || row.UserID != ac.UserID || row.DeviceID != ac.DeviceID || protocol.WorkflowTerminal(row.State) || result.Revision <= row.AgentRevision {
		return
	}
	if _, err = s.authorizeDevice(ctx, row.UserID, row.DeviceID); err != nil {
		return
	}
	switch result.State {
	case "waiting_idle", "running", "delegating", "checking", "cancelling", "ready_for_review", "cancelled", "failed", "needs_review":
	default:
		return
	}
	if row.State == "cancelling" && !protocol.WorkflowTerminal(result.State) {
		return
	}
	if len(result.Steps) > 64 || len(result.Diff) > 24<<10 || len(result.Detail) > 4096 {
		return
	}
	expected := row.Revision
	row.State = result.State
	row.Detail = result.Detail
	row.MasterNativeID = result.MasterNativeID
	row.MasterLabel = result.MasterLabel
	row.Worktree = result.Worktree
	row.Branch = result.Branch
	row.BaseCommit = result.BaseCommit
	row.Steps = result.Steps
	row.Diff = result.Diff
	row.DiffTruncated = result.DiffTruncated
	row.AgentRevision = result.Revision
	row.Revision++
	row.UpdatedAt = s.now().UnixMilli()
	_, _ = s.store.UpdateWorkflow(ctx, row, expected)
}
func (s *Server) pollWorkflows(ctx context.Context) {
	rows, err := s.store.Workflows(ctx, "", "")
	if err != nil {
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		if protocol.WorkflowTerminal(row.State) {
			continue
		}
		s.tasksMu.Lock()
		current, err := s.store.Workflow(ctx, row.ID)
		if err != nil || protocol.WorkflowTerminal(current.State) {
			s.tasksMu.Unlock()
			continue
		}
		ac, err := s.requireAgent(ctx, current.UserID, current.DeviceID)
		if err != nil || !ac.Caps().WorkflowV1 {
			if current.State == "queued" || current.State == "waiting_agent" || current.State == "waiting_idle" {
				expected := current.Revision
				current.Revision++
				current.State = "waiting_agent"
				current.Detail = "等待设备上线并使用支持调度的 Agent"
				current.UpdatedAt = s.now().UnixMilli()
				_, _ = s.store.UpdateWorkflow(ctx, current, expected)
			}
			s.tasksMu.Unlock()
			continue
		}
		user, err := s.store.UserByID(ctx, current.UserID)
		action := "inspect"
		if err != nil || user.Disabled || current.State == "cancelling" {
			action = "cancel"
		} else if current.State == "queued" || current.State == "waiting_agent" || current.State == "waiting_idle" {
			action = "start"
		}
		if action == "start" {
			expected := current.Revision
			current.State = "dispatching"
			current.Detail = "正在把目标交给主 Agent"
			current.Revision++
			current.UpdatedAt = s.now().UnixMilli()
			ok, e := s.store.UpdateWorkflow(ctx, current, expected)
			if e != nil || !ok {
				s.tasksMu.Unlock()
				continue
			}
		}
		s.tasksMu.Unlock()
		response, err := s.workflowRPC(ctx, ac, protocol.WorkflowControl{Action: action, ID: current.ID, UserID: current.UserID, Workflow: current})
		if err == nil && response.Workflow != nil && response.Workflow.ID == current.ID {
			s.applyWorkflow(ac, response.Workflow)
		}
	}
}
func (s *Server) startWorkflowLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			s.pollWorkflows(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
