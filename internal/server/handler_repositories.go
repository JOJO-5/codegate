package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

type repositoryWait struct {
	agent *AgentConn
	reply chan *protocol.Envelope
}

func (s *Server) handleDeviceRepositories(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	ac, err := s.requireAgent(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		writeProtoError(w, err)
		return
	}
	if !ac.Caps().RepositoryScan {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "该 Agent 尚不支持仓库扫描，请更新目标电脑上的 Agent")
		return
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "invalid_payload", "无效仓库分页参数")
			return
		}
	}
	reqID := uuid.NewString()
	reply := make(chan *protocol.Envelope, 1)
	s.repositoryMu.Lock()
	if s.repositoryPending == nil {
		s.repositoryPending = make(map[string]repositoryWait)
	}
	s.repositoryPending[reqID] = repositoryWait{agent: ac, reply: reply}
	s.repositoryMu.Unlock()
	defer func() { s.repositoryMu.Lock(); delete(s.repositoryPending, reqID); s.repositoryMu.Unlock() }()
	env, err := protocol.NewRequest(reqID, protocol.TypeRepositoryList, "", protocol.RepositoryListPayload{Offset: offset, Limit: 50, Refresh: r.URL.Query().Get("refresh") == "true"})
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
		writeError(w, http.StatusServiceUnavailable, "agent_busy", "Agent 暂时繁忙")
		return
	}
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	select {
	case response := <-reply:
		if response.Type == protocol.TypeError {
			writeError(w, http.StatusBadGateway, "repository_scan_failed", "Agent 无法读取仓库清单")
			return
		}
		result, err := protocol.DecodePayload[protocol.RepositoryListResult](response)
		if err != nil {
			writeProtoError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case <-ac.Done():
		writeError(w, http.StatusServiceUnavailable, "device_offline", "Agent 已断开")
	case <-timer.C:
		writeError(w, http.StatusGatewayTimeout, "agent_timeout", "Agent 未响应仓库清单请求")
	case <-r.Context().Done():
		return
	}
}

func (s *Server) acceptRepositoryReply(ac *AgentConn, env *protocol.Envelope) bool {
	if env.ReplyTo == "" {
		return false
	}
	s.repositoryMu.Lock()
	wait, ok := s.repositoryPending[env.ReplyTo]
	if ok && wait.agent == ac {
		delete(s.repositoryPending, env.ReplyTo)
	}
	s.repositoryMu.Unlock()
	if !ok || wait.agent != ac {
		return false
	}
	wait.reply <- env
	return true
}
