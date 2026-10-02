package server

import (
	"github.com/jojo/codegate/internal/protocol"
	"testing"
)

func TestGitRepliesAreBoundToAgentConnection(t *testing.T) {
	owner := &AgentConn{DeviceID: "same-device"}
	impostor := &AgentConn{DeviceID: "same-device"}
	ch := make(chan *protocol.Envelope, 1)
	srv := &Server{gitPending: map[string]repositoryWait{"request": {agent: owner, reply: ch}}}
	env := &protocol.Envelope{Type: protocol.TypeGitResult, ReplyTo: "request"}
	if srv.acceptGitReply(impostor, env) {
		t.Fatal("accepted reply from another connection")
	}
	if len(srv.gitPending) != 1 {
		t.Fatal("foreign reply consumed pending request")
	}
	if !srv.acceptGitReply(owner, env) {
		t.Fatal("rejected owner's reply")
	}
	if len(srv.gitPending) != 0 || <-ch != env {
		t.Fatal("reply was not delivered and consumed")
	}
	if srv.acceptGitReply(owner, env) {
		t.Fatal("accepted duplicate reply")
	}
}

func TestGitEndpointRequiresDeviceOwnership(t *testing.T) {
	env := newTestEnv(t)
	token := env.signup(t, "git-owner@example.com", "correct-horse-battery")
	other := env.signup(t, "git-other@example.com", "correct-horse-battery")
	deviceID, key := env.seedPairedDevice(t, env.meUserID(t, token))
	env.agentHandshake(t, deviceID, key)
	response := env.post(t, "/api/v1/devices/"+deviceID+"/git", token, map[string]any{"path": "/tmp", "action": "status"})
	if response.Status != 409 {
		t.Fatalf("old Agent: %d %s", response.Status, response.Body)
	}
	response = env.post(t, "/api/v1/devices/"+deviceID+"/git", other, map[string]any{"path": "/tmp", "action": "status"})
	if response.Status != 404 {
		t.Fatalf("foreign device: %d %s", response.Status, response.Body)
	}
}
