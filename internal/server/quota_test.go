package server

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestQuotaRequiresSessionOwnershipAndCapability(t *testing.T) {
	e := newTestEnv(t)
	owner := e.signup(t, "quota-owner@example.com", "correct-horse-battery")
	other := e.signup(t, "quota-other@example.com", "correct-horse-battery")
	device, priv := e.seedPairedDevice(t, e.meUserID(t, owner))
	native := e.agentHandshake(t, device, priv)
	browser := e.clientWS(t, owner)
	sid := e.openSession(t, browser, native, device)
	req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeQuotaRead, sid, map[string]string{"session_id": sid})
	sendJSON(t, browser, req)
	response := recvEnvelope(t, browser)
	if response.Type != protocol.TypeError || !strings.Contains(string(response.Payload), "v0.1.15") {
		t.Fatalf("old Agent must fail immediately: %+v", response)
	}
	outsider := e.clientWS(t, other)
	req, _ = protocol.NewRequest(uuid.NewString(), protocol.TypeQuotaRead, sid, map[string]string{"session_id": sid})
	sendJSON(t, outsider, req)
	response = recvEnvelope(t, outsider)
	if response.Type != protocol.TypeError || strings.Contains(string(response.Payload), "v0.1.15") {
		t.Fatalf("ownership must be checked before Agent capability: %+v", response)
	}
}
