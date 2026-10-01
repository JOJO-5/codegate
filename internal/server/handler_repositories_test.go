package server

import (
	"github.com/jojo/codegate/internal/protocol"
	"testing"
)

func TestRepositoryRepliesAreBoundToAgentConnection(t *testing.T) {
	owner := &AgentConn{DeviceID: "same-device"}
	impostor := &AgentConn{DeviceID: "same-device"}
	ch := make(chan *protocol.Envelope, 1)
	srv := &Server{repositoryPending: map[string]repositoryWait{"request": {agent: owner, reply: ch}}}
	env := &protocol.Envelope{Type: protocol.TypeRepositoryListed, ReplyTo: "request"}
	if srv.acceptRepositoryReply(impostor, env) {
		t.Fatal("accepted reply from another connection")
	}
	if len(srv.repositoryPending) != 1 {
		t.Fatal("foreign reply consumed pending request")
	}
	if !srv.acceptRepositoryReply(owner, env) {
		t.Fatal("rejected owner's reply")
	}
	if len(srv.repositoryPending) != 0 || <-ch != env {
		t.Fatal("reply was not delivered and consumed")
	}
	if srv.acceptRepositoryReply(owner, env) {
		t.Fatal("accepted duplicate reply")
	}
}

func TestRepositoryListTellsOwnerToUpgradeOldAgent(t *testing.T) {
	env := newTestEnv(t)
	token := env.signup(t, "repo-owner@example.com", "correct-horse-battery")
	deviceID, privateKey := env.seedPairedDevice(t, env.meUserID(t, token))
	env.agentHandshake(t, deviceID, privateKey)
	response := env.get(t, "/api/v1/devices/"+deviceID+"/repositories", token)
	if response.Status != 409 || response.ErrorCode(t) != "agent_upgrade_required" {
		t.Fatalf("old Agent was not identified: %d %s", response.Status, response.Body)
	}
}
