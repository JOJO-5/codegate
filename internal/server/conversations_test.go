package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

func TestConversationRecoveryAuthorizationAndTrustedSource(t *testing.T) {
	e := newTestEnv(t)
	alice := e.signup(t, "recover-alice@example.com", "correct-horse-battery")
	aliceID := e.meUserID(t, alice)
	bob := e.signup(t, "recover-bob@example.com", "correct-horse-battery")
	device, priv := e.seedPairedDevice(t, aliceID)
	agent, _ := e.wsDial(t, "/api/v1/ws/agent", nil)
	if agent == nil {
		t.Fatal("dial")
	}
	defer agent.Close()
	hello := helloEnvelope(device)
	payload, _ := protocol.DecodePayload[protocol.AgentHelloPayload](hello)
	payload.Caps.ConversationRecovery = true
	hello.Payload, _ = json.Marshal(payload)
	sendJSON(t, agent, hello)
	cp, _ := protocol.DecodePayload[protocol.AgentChallengePayload](recvEnvelope(t, agent))
	data, _ := protocol.SigningPayload(cp.Nonce, device, cp.ServerTime)
	auth, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeAgentAuth, "", protocol.AgentAuthPayload{Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))})
	sendJSON(t, agent, auth)
	if recvEnvelope(t, agent).Type != protocol.TypeAgentReady {
		t.Fatal("auth")
	}
	sid := uuid.NewString()
	native := uuid.NewString()
	meta := &storage.SessionMeta{ID: sid, DeviceID: device, UserID: aliceID, Command: "codex", Cwd: "/allowed/original", Status: "terminated", Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: native}, CreatedAt: e.clock.Now()}
	if err := e.store.UpsertSession(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	request := func(ws *websocket.Conn, typ protocol.Type) {
		env, _ := protocol.NewRequest(uuid.NewString(), typ, sid, protocol.ConversationRequest{SessionID: sid, Source: protocol.SessionSummary{Cwd: "/outside", Command: "evil"}})
		sendJSON(t, ws, env)
	}
	bc := e.clientWS(t, bob)
	request(bc, protocol.TypeConversationRestore)
	if recvEnvelope(t, bc).Type != protocol.TypeError {
		t.Fatal("cross-account restore accepted")
	}
	ac := e.clientWS(t, alice)
	request(ac, protocol.TypeConversationList)
	fwd := agentRecv(t, agent)
	source, err := protocol.DecodePayload[protocol.ConversationRequest](fwd)
	if err != nil {
		t.Fatal(err)
	}
	if source.Source.Cwd != meta.Cwd || source.Source.Command != meta.Command || source.Source.Recovery.NativeID != native {
		t.Fatalf("browser substituted source: %+v", source.Source)
	}
	agentReply(t, agent, fwd, protocol.TypeConversationListed, protocol.ConversationListed{Conversations: []protocol.NativeConversation{{ID: native, Title: "original"}}})
	if recvEnvelope(t, ac).Type != protocol.TypeConversationListed {
		t.Fatal("history response lost")
	}
	request(ac, protocol.TypeConversationRestore)
	fwd = agentRecv(t, agent)
	newID := uuid.NewString()
	agentReply(t, agent, fwd, protocol.TypeSessionCreated, protocol.SessionCreatedPayload{Session: protocol.SessionSummary{SessionID: newID, Cwd: meta.Cwd, Command: "codex", Status: "running", Recovery: &protocol.ConversationBinding{CommandID: "codex", NativeID: native, SourceID: sid}}})
	if recvEnvelope(t, ac).Type != protocol.TypeSessionCreated {
		t.Fatal("restore response lost")
	}
	saved, err := e.store.SessionByID(context.Background(), newID)
	if err != nil || saved.Recovery.NativeID != native {
		t.Fatal("native identity not durable", err)
	}
}
