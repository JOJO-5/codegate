package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func (s *Server) handleDeviceGit(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	ac, err := s.requireAgent(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		writeProtoError(w, err)
		return
	}
	if !ac.Caps().GitReview {
		writeError(w, 409, "agent_upgrade_required", "请更新目标电脑上的 Agent 后使用 Git 面板")
		return
	}
	var req protocol.GitPayload
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || (req.Action != "status" && req.Action != "diff" && req.Action != "commit" && req.Action != "push" && req.Action != "worktrees" && req.Action != "worktree_remove") || req.Path == "" {
		writeError(w, 400, "invalid_payload", "无效 Git 请求")
		return
	}
	if req.Action == "worktrees" || req.Action == "worktree_remove" {
		if !ac.Caps().WorktreeManagement {
			writeError(w, 409, "agent_upgrade_required", "请更新 Agent 到 v0.1.12 后管理工作区")
			return
		}
		if req.Action == "worktree_remove" && (!req.Confirm || req.ExpectedHead == "" || req.Target == "") {
			writeError(w, 400, "invalid_payload", "请刷新工作区并确认目标后清理")
			return
		}
	}
	if req.Action == "commit" || req.Action == "push" {
		if !ac.Caps().GitActions {
			writeError(w, 409, "agent_upgrade_required", "请更新 Agent 后提交或推送")
			return
		}
		if !req.Confirm || req.ExpectedHead == "" {
			writeError(w, 400, "invalid_payload", "请先刷新改动并确认操作")
			return
		}
	}
	id := uuid.NewString()
	ch := make(chan *protocol.Envelope, 1)
	s.repositoryMu.Lock()
	if s.gitPending == nil {
		s.gitPending = map[string]repositoryWait{}
	}
	s.gitPending[id] = repositoryWait{agent: ac, reply: ch}
	s.repositoryMu.Unlock()
	defer func() { s.repositoryMu.Lock(); delete(s.gitPending, id); s.repositoryMu.Unlock() }()
	env, err := protocol.NewRequest(id, protocol.TypeGitRequest, "", req)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	raw, err := protocol.Encode(env)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	if err := ac.TrySendText(raw); err != nil {
		writeError(w, 503, "agent_busy", "Agent 暂时繁忙")
		return
	}
	timer := time.NewTimer(40 * time.Second)
	defer timer.Stop()
	select {
	case response := <-ch:
		if response.Type == protocol.TypeError {
			failure, err := protocol.DecodePayload[protocol.ErrorPayload](response)
			if err != nil {
				writeProtoError(w, err)
				return
			}
			writeError(w, 400, string(failure.Code), failure.Message)
			return
		}
		result, err := protocol.DecodePayload[protocol.GitResult](response)
		if err != nil {
			writeProtoError(w, err)
			return
		}
		writeJSON(w, 200, result)
	case <-ac.Done():
		writeError(w, 503, "device_offline", "Agent 已断开")
	case <-timer.C:
		writeError(w, 504, "agent_timeout", "Git 操作超时")
	case <-r.Context().Done():
		return
	}
}

func (s *Server) acceptGitReply(ac *AgentConn, env *protocol.Envelope) bool {
	if env.ReplyTo == "" {
		return false
	}
	s.repositoryMu.Lock()
	wait, ok := s.gitPending[env.ReplyTo]
	if ok && wait.agent == ac {
		delete(s.gitPending, env.ReplyTo)
	}
	s.repositoryMu.Unlock()
	if !ok || wait.agent != ac {
		return false
	}
	wait.reply <- env
	return true
}
