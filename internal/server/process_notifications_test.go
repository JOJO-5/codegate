package server

import (
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"testing"
	"time"
)

func TestProcessExitReachesUnattachedOwnerOnly(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "notice-owner@example.com", "correct-horse-battery")
	other := e.signup(t, "notice-other@example.com", "correct-horse-battery")
	user := e.meUserID(t, token)
	device, key := e.seedPairedDevice(t, user)
	agent := e.agentHandshake(t, device, key)
	client := e.clientWS(t, token)
	sid := e.openSession(t, client, agent, device)
	// This second owner browser never attaches to the terminal.
	observer := e.clientWS(t, token)
	foreign := e.clientWS(t, other)
	push, _ := protocol.NewEnvelope(protocol.TypeSessionExit, protocol.SessionExitPayload{SessionID: sid, ExitCode: 7, Reason: "exited"})
	push.SessionID = sid
	sendJSON(t, agent, push)
	got := recvEnvelope(t, observer)
	payload, err := protocol.DecodePayload[protocol.SessionExitPayload](got)
	if err != nil || got.Type != protocol.TypeSessionExit || payload.ExitCode != 7 || got.ReplyTo != "" {
		t.Fatalf("observer: %+v %v", got, err)
	}
	got = recvEnvelope(t, client)
	if got.Type != protocol.TypeSessionExit {
		t.Fatal(got.Type)
	}
	foreign.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := foreign.ReadMessage(); err == nil {
		t.Fatal("another account received process exit")
	}
}
func TestForeignAgentCannotEndAnotherDevicesSession(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "notice-agent-owner@example.com", "correct-horse-battery")
	user := e.meUserID(t, token)
	device, key := e.seedPairedDevice(t, user)
	agent := e.agentHandshake(t, device, key)
	client := e.clientWS(t, token)
	sid := e.openSession(t, client, agent, device)
	foreignDevice, foreignKey := e.seedPairedDevice(t, user)
	foreignAgent := e.agentHandshake(t, foreignDevice, foreignKey)
	forged, _ := protocol.NewEnvelope(protocol.TypeSessionExit, protocol.SessionExitPayload{SessionID: sid, ExitCode: 99, Reason: "exited"})
	forged.SessionID = sid
	sendJSON(t, foreignAgent, forged)
	time.Sleep(100 * time.Millisecond)
	// Even the correct Agent cannot mix the envelope's owned session with a
	// different payload session. The legitimate event below shares its socket,
	// so it also fences processing of this malformed event.
	mixed, _ := protocol.NewEnvelope(protocol.TypeSessionExit, protocol.SessionExitPayload{SessionID: uuid.NewString(), ExitCode: 88, Reason: "exited"})
	mixed.SessionID = sid
	sendJSON(t, agent, mixed)
	actual, _ := protocol.NewEnvelope(protocol.TypeSessionExit, protocol.SessionExitPayload{SessionID: sid, ExitCode: 0, Reason: "exited"})
	actual.SessionID = sid
	sendJSON(t, agent, actual)
	got := recvEnvelope(t, client)
	payload, err := protocol.DecodePayload[protocol.SessionExitPayload](got)
	if err != nil || payload.ExitCode != 0 {
		t.Fatalf("foreign exit affected owner: %+v %v", got, err)
	}
}
