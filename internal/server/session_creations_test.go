package server

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestSessionCreationIntentSurvivesLostClientAndPreventsReplay(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "create@example.com", "correct-horse-battery")
	user := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, user)
	agent := e.agentHandshake(t, device, priv, protocol.AgentCaps{Worktrees: true})
	client := e.clientWS(t, token)
	nonce := uuid.NewString()
	body := map[string]any{"device_id": device, "command_id": "shell", "cwd": `C:\`, "nonce": nonce, "worktree": true}
	first, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionCreate, "", body)
	sendJSON(t, client, first)
	forwarded := agentRecv(t, agent)
	// The browser disappears before receipt. A durable intent must still reject
	// a second dispatch, including isolated-worktree side effects.
	e.srv.pending.mu.Lock()
	origin := e.srv.pending.m[first.RequestID].client
	e.srv.pending.mu.Unlock()
	e.srv.pending.DropClient(origin)
	retry, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionCreate, "", body)
	sendJSON(t, client, retry)
	ping, _ := protocol.NewRequest(uuid.NewString(), protocol.TypePing, "", nil)
	sendJSON(t, client, ping)
	got := recvEnvelope(t, client)
	if got.Type != protocol.TypeError || got.ReplyTo != retry.RequestID {
		t.Fatalf("pending intent was replayed instead of rejected: %s", got.Type)
	}
	if recvEnvelope(t, client).Type != protocol.TypePong {
		t.Fatal("missing pong")
	}
	// A late Agent receipt is persisted even after pending routing has gone.
	sid := uuid.NewString()
	agentReply(t, agent, forwarded, protocol.TypeSessionCreated, protocol.SessionCreatedPayload{Session: protocol.SessionSummary{SessionID: sid, Cwd: `C:\`, Command: "shell", Status: "running"}})
	// Synchronize Agent messages without polling or sleeps.
	list, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionList, "", map[string]string{"device_id": device})
	sendJSON(t, client, list)
	fwd := agentRecv(t, agent)
	if fwd.Type != protocol.TypeSessionList {
		t.Fatalf("duplicate reached Agent: %s", fwd.Type)
	}
	agentReply(t, agent, fwd, protocol.TypeSessionListed, protocol.SessionListedPayload{})
	recvEnvelope(t, client)
	r := e.get(t, "/api/v1/devices/"+device+"/session-creations/"+nonce, token)
	if r.Status != 200 || r.Str(t, "state") != "resolved" {
		t.Fatalf("lost receipt not persisted: %d %s", r.Status, r.Body)
	}
	sendJSON(t, client, retry)
	got = recvEnvelope(t, client)
	created, _ := protocol.DecodePayload[protocol.SessionCreatedPayload](got)
	if got.Type != protocol.TypeSessionCreated || created.Session.SessionID != sid {
		t.Fatal("resolved intent must return its original session")
	}
	body["cwd"] = `C:\other`
	changed, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionCreate, "", body)
	sendJSON(t, client, changed)
	if recvEnvelope(t, client).Type != protocol.TypeError {
		t.Fatal("nonce reuse with changed intent accepted")
	}
	other := e.signup(t, "other-create@example.com", "correct-horse-battery")
	if e.get(t, "/api/v1/devices/"+device+"/session-creations/"+nonce, other).Status == 200 {
		t.Fatal("creation receipt leaked across accounts")
	}
}

func TestSessionCreationDistinguishesFailureFromUnknownOutcome(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "create-failures@example.com", "correct-horse-battery")
	user := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, user)
	agent := e.agentHandshake(t, device, priv)
	client := e.clientWS(t, token)
	for _, code := range []protocol.ErrorCode{protocol.CodeRequestOutcomeUnknown, protocol.CodeInvalidPayload} {
		nonce := uuid.NewString()
		req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionCreate, "", protocol.SessionCreatePayload{DeviceID: device, CommandID: "shell", Cwd: `C:\`, Nonce: nonce})
		sendJSON(t, client, req)
		forwarded := agentRecv(t, agent)
		agentReply(t, agent, forwarded, protocol.TypeError, protocol.ErrorPayload{Code: code, Message: "fixture result"})
		if recvEnvelope(t, client).Type != protocol.TypeError {
			t.Fatal("missing failure receipt")
		}
		result := e.get(t, "/api/v1/devices/"+device+"/session-creations/"+nonce, token)
		expected := "resolved"
		if code == protocol.CodeRequestOutcomeUnknown {
			expected = "pending"
		}
		if result.Status != 200 || result.Str(t, "state") != expected {
			t.Fatalf("%s: %d %s", code, result.Status, result.Body)
		}
	}
}
