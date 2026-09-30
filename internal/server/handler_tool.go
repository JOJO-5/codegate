package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

type toolSetRequest struct { Enabled *bool `json:"enabled"` }

// Only the paired owner can change one of four fixed, locally validated CLI
// approvals. No command path or arguments are accepted from HTTP.
func (s *Server) handleDeviceToolSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("tool")
	switch id { case "claude", "codex", "opencode", "dsh": default:
		writeError(w, http.StatusBadRequest, "invalid_tool", "只能授权已识别的 CLI")
		return
	}
	p, ok := decodeJSON[toolSetRequest](w, r)
	if !ok { return }
	if p.Enabled == nil { writeError(w, http.StatusBadRequest, "invalid_payload", "缺少 enabled"); return }
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")
	ac, err := s.requireAgent(r.Context(), userID, deviceID)
	if err != nil { writeProtoError(w, err); return }
	reqID := uuid.NewString()
	reply := make(chan error, 1)
	s.web.mu.Lock()
	s.web.toolPending[reqID] = toolWait{agent: ac, reply: reply}
	s.web.mu.Unlock()
	defer func() { s.web.mu.Lock(); delete(s.web.toolPending, reqID); s.web.mu.Unlock() }()
	env, err := protocol.NewRequest(reqID, protocol.TypeToolSet, "", protocol.ToolSetPayload{ID: id, Enabled: *p.Enabled})
	if err != nil { writeProtoError(w, err); return }
	raw, err := protocol.Encode(env)
	if err != nil { writeProtoError(w, err); return }
	if err := ac.TrySendText(raw); err != nil { writeError(w, http.StatusServiceUnavailable, "agent_busy", "Agent 暂时繁忙"); return }
	timer := time.NewTimer(12*time.Second)
	defer timer.Stop()
	select {
	case err := <-reply:
		if err != nil { writeError(w, http.StatusConflict, "tool_unavailable", err.Error()); return }
		s.auditRequest(r, auditEntry{UserID: userID, DeviceID: deviceID, Action: auditDeviceToolSet, Result: auditResultOK, Meta: map[string]any{"tool": id, "enabled": *p.Enabled}})
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": *p.Enabled})
	case <-ac.Done():
		writeError(w, http.StatusServiceUnavailable, "device_offline", "Agent 已断开")
	case <-timer.C:
		writeError(w, http.StatusGatewayTimeout, "agent_timeout", "Agent 未响应；请确认 Agent 已更新")
	case <-r.Context().Done():
		return
	}
}

func (s *Server) acceptToolReply(ac *AgentConn, env *protocol.Envelope) bool {
	if env.ReplyTo == "" { return false }
	s.web.mu.Lock()
	wait, ok := s.web.toolPending[env.ReplyTo]
	if ok && wait.agent == ac { delete(s.web.toolPending, env.ReplyTo) }
	s.web.mu.Unlock()
	if !ok || wait.agent != ac { return false }
	var err error
	if env.Type == protocol.TypeError {
		p, decodeErr := protocol.DecodePayload[protocol.ErrorPayload](env)
		if decodeErr != nil { err = decodeErr } else { err = errors.New(p.Message) }
	}
	wait.reply <- err
	return true
}
