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
	for _, action := range []string{"check", "restart"} {
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
