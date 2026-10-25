package server

import (
	"github.com/jojo/codegate/internal/protocol"
	"net/http"
	"testing"
)

func TestOwnerUpdateControlRoutesAndAcknowledgesCurrentAgent(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "update-control@example.com", "correct-horse-battery")
	id, key := e.seedPairedDevice(t, e.meUserID(t, token))
	ws := e.agentHandshake(t, id, key)
	defer ws.Close()
	ac, ok := e.srv.reg.Agent(id)
	if !ok {
		t.Fatal("Agent not registered")
	}
	path := "/api/v1/devices/" + id + "/update"
	old := e.request(t, http.MethodPost, path, token, map[string]string{"action": "restart"})
	if old.Status != http.StatusConflict {
		t.Fatalf("legacy Agent: %d", old.Status)
	}
	ac.SetUpdateStatus(protocol.AgentUpdateStatus{Managed: true})
	unsupported := e.request(t, http.MethodPost, path, token, map[string]string{"action": "cancel_restart"})
	if unsupported.Status != http.StatusConflict {
		t.Fatal("accepted cancellation on unsupported Agent")
	}
	ac.SetUpdateStatus(protocol.AgentUpdateStatus{Managed: true, CanCancelRestart: true})
	for _, action := range []string{"check", "restart", "cancel_restart"} {
		result := make(chan int, 1)
		go func() {
			response := e.request(t, http.MethodPost, path, token, map[string]string{"action": action})
			result <- response.Status
		}()
		req := agentRecv(t, ws)
		p, err := protocol.DecodePayload[protocol.AgentUpdateControl](req)
		if err != nil || req.Type != protocol.TypeAgentUpdateControl || p.Action != action {
			t.Fatalf("control: %+v %v", req, err)
		}
		agentReply(t, ws, req, protocol.TypeAgentUpdateAccepted, nil)
		if status := <-result; status != http.StatusAccepted {
			t.Fatalf("ack: %d", status)
		}
	}
	invalid := e.request(t, http.MethodPost, path, token, map[string]string{"action": "execute"})
	if invalid.Status != http.StatusBadRequest {
		t.Fatalf("invalid: %d", invalid.Status)
	}
}

func TestImmediateRestartRequiresConfirmationCapabilityAndCurrentInstance(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "immediate-update@example.com", "correct-horse-battery")
	id, key := e.seedPairedDevice(t, e.meUserID(t, token))
	ws := e.agentHandshake(t, id, key)
	defer ws.Close()
	ac, _ := e.srv.reg.Agent(id)
	path := "/api/v1/devices/" + id + "/update"
	ac.SetUpdateStatus(protocol.AgentUpdateStatus{Managed: true, InstanceStartedAt: 100})
	if r := e.request(t, http.MethodPost, path, "", map[string]any{"action": "restart_now", "confirmed": true, "instance_started_at": 100}); r.Status != http.StatusUnauthorized {
		t.Fatal("unauthenticated force accepted", r.Status)
	}
	other := e.signup(t, "foreign-immediate@example.com", "correct-horse-battery")
	if r := e.request(t, http.MethodPost, path, other, map[string]any{"action": "restart_now", "confirmed": true, "instance_started_at": 100}); r.Status != http.StatusNotFound && r.Status != http.StatusForbidden {
		t.Fatal("foreign owner force accepted", r.Status)
	}
	for _, p := range []map[string]any{
		{"action": "restart_now"},
		{"action": "restart_now", "confirmed": true, "instance_started_at": 100},
	} {
		if r := e.request(t, http.MethodPost, path, token, p); r.Status != http.StatusConflict {
			t.Fatal("unsupported force accepted", r.Status)
		}
	}
	ac.SetUpdateStatus(protocol.AgentUpdateStatus{Managed: true, CanRestartNow: true, InstanceStartedAt: 100})
	for _, p := range []map[string]any{
		{"action": "restart_now", "instance_started_at": 100},
		{"action": "restart_now", "confirmed": true, "instance_started_at": 99},
	} {
		if r := e.request(t, http.MethodPost, path, token, p); r.Status != http.StatusConflict {
			t.Fatal("missing/stale consent accepted", r.Status)
		}
	}
	result := make(chan int, 1)
	go func() {
		result <- e.request(t, http.MethodPost, path, token, map[string]any{"action": "restart_now", "confirmed": true, "instance_started_at": 100, "owner_id": "forged"}).Status
	}()
	req := agentRecv(t, ws)
	p, err := protocol.DecodePayload[protocol.AgentUpdateControl](req)
	if err != nil || !p.Confirmed || p.InstanceStartedAt != 100 || p.OwnerID != ac.UserID {
		t.Fatal("invalid consent forwarded", p, err)
	}
	agentReply(t, ws, req, protocol.TypeAgentUpdateAccepted, nil)
	if status := <-result; status != http.StatusAccepted {
		t.Fatal("force not acknowledged", status)
	}
}
