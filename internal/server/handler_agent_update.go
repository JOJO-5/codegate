package server

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func (s *Server) handleDeviceUpdateControl(w http.ResponseWriter, r *http.Request) {
	p, ok := decodeJSON[protocol.AgentUpdateControl](w, r)
	if !ok {
		return
	}
	if p.Action != "check" && p.Action != "restart" && p.Action != "cancel_restart" && p.Action != "restart_now" {
		writeError(w, http.StatusBadRequest, "invalid_action", "更新操作无效")
		return
	}
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")
	ac, err := s.requireAgent(r.Context(), userID, deviceID)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	status := ac.UpdateStatus()
	if status == nil || !status.Managed {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "需要新版 Agent 通过 supervise 服务运行")
		return
	}
	if p.Action == "cancel_restart" && !status.CanCancelRestart {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "Agent 版本不支持取消，或已开始重启")
		return
	}
	p.OwnerID = ""
	if p.Action == "restart_now" {
		if !status.CanRestartNow {
			writeError(w, http.StatusConflict, "agent_upgrade_required", "当前 Agent 不支持立即重启，请先完成一次空闲升级")
			return
		}
		if !p.Confirmed || p.InstanceStartedAt <= 0 || p.InstanceStartedAt != status.InstanceStartedAt {
			writeError(w, http.StatusConflict, "confirmation_required", "请重新核对当前设备状态，并确认关闭任务后立即重启")
			return
		}
		p.OwnerID = userID
	}
	reqID := uuid.NewString()
	reply := make(chan error, 1)
	s.web.mu.Lock()
	s.web.toolPending[reqID] = toolWait{agent: ac, reply: reply, expected: protocol.TypeAgentUpdateAccepted}
	s.web.mu.Unlock()
	defer func() { s.web.mu.Lock(); delete(s.web.toolPending, reqID); s.web.mu.Unlock() }()
	env, err := protocol.NewRequest(reqID, protocol.TypeAgentUpdateControl, "", p)
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
	respond := func(err error) {
		if err != nil {
			writeError(w, http.StatusConflict, "update_unavailable", err.Error())
			return
		}
		s.auditRequest(r, auditEntry{UserID: userID, DeviceID: deviceID, Action: "device.update." + p.Action, Result: auditResultOK})
		writeJSON(w, http.StatusAccepted, map[string]any{"action": p.Action})
	}
	select {
	case err := <-reply:
		respond(err)
	case <-ac.Done():
		// A successful acknowledgement may precede the expected restart close.
		select {
		case err := <-reply:
			respond(err)
		default:
			writeError(w, http.StatusServiceUnavailable, "device_offline", "Agent 已断开")
		}
	case <-timer.C:
		if p.Action == "restart_now" {
			writeError(w, http.StatusGatewayTimeout, "update_outcome_unknown", "重启请求结果尚未确认，请核对设备状态，不要自动重复提交")
			return
		}
		writeError(w, http.StatusGatewayTimeout, "agent_timeout", "Agent 未响应；请确认 Agent 已更新")
	case <-r.Context().Done():
		return
	}
}
