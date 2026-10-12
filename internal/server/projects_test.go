package server

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"testing"
)

func TestProjectCreateRequiresDeviceOwnerAndAgentCapability(t *testing.T) {
	e := newTestEnv(t)
	owner := e.signup(t, "project-owner@example.com", "correct-horse-battery")
	user := e.meUserID(t, owner)
	device, priv := e.seedPairedDevice(t, user)
	agent := e.agentHandshake(t, device, priv)
	client := e.clientWS(t, owner)
	request, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeProjectCreate, "", protocol.ProjectCreatePayload{DeviceID: device, Parent: "/allowed", Name: "project"})
	sendJSON(t, client, request)
	if got := recvEnvelope(t, client); got.Type != protocol.TypeError {
		t.Fatal("old Agent capability ignored")
	}
	agent = e.agentHandshake(t, device, priv, protocol.AgentCaps{ProjectCreate: true})
	stranger := e.signup(t, "project-stranger@example.com", "correct-horse-battery")
	other := e.clientWS(t, stranger)
	request.RequestID = uuid.NewString()
	sendJSON(t, other, request)
	if got := recvEnvelope(t, other); got.Type != protocol.TypeError {
		t.Fatal("cross-user create authorized")
	}
	request.RequestID = uuid.NewString()
	sendJSON(t, client, request)
	fwd := agentRecv(t, agent)
	payload, err := protocol.DecodePayload[protocol.ProjectCreatePayload](fwd)
	if err != nil || payload.DeviceID != device || payload.Parent != "/allowed" {
		t.Fatal("request routing changed", err)
	}
	agentReply(t, agent, fwd, protocol.TypeProjectCreated, protocol.ProjectCreatedPayload{Path: "/allowed/project"})
	got := recvEnvelope(t, client)
	if got.Type != protocol.TypeProjectCreated || got.ReplyTo != request.RequestID {
		t.Fatal("project response lost")
	}
	// No terminal session is created by creating a directory/file.
	rows, err := e.store.SessionsByDevice(context.Background(), device)
	if err != nil || len(rows) != 0 {
		b, _ := json.Marshal(rows)
		t.Fatalf("unexpected sessions %s %v", b, err)
	}
}
