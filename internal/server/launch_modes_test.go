package server

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestNativeLaunchModeServerGate(t *testing.T) {
	caps := protocol.AgentCaps{NativeLaunchModes: true}
	commands := []protocol.CommandAvailability{{ID: "codex", Installed: true, Allowed: true, NativeLaunchModes: []protocol.NativeLaunchMode{{ID: "codex.full-access"}}}}
	req := protocol.SessionCreatePayload{CommandID: "codex", LaunchMode: "codex.full-access"}
	if err := validateNativeLaunchMode(caps, commands, req); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*protocol.AgentCaps, *protocol.SessionCreatePayload){
		func(c *protocol.AgentCaps, _ *protocol.SessionCreatePayload) { c.NativeLaunchModes = false },
		func(_ *protocol.AgentCaps, r *protocol.SessionCreatePayload) { r.LaunchMode = "claude.full-access" },
		func(_ *protocol.AgentCaps, r *protocol.SessionCreatePayload) { r.CommandID = "other" },
		func(_ *protocol.AgentCaps, r *protocol.SessionCreatePayload) { r.Args = []string{"browser-controlled"} },
		func(_ *protocol.AgentCaps, r *protocol.SessionCreatePayload) { r.Resume = true },
	} {
		c, r := caps, req
		change(&c, &r)
		if validateNativeLaunchMode(c, commands, r) == nil {
			t.Fatal("invalid native mode selection accepted")
		}
	}
	if err := validateNativeLaunchMode(protocol.AgentCaps{}, nil, protocol.SessionCreatePayload{}); err != nil {
		t.Fatal("inherit broke old Agent")
	}
}

func TestNativeLaunchModeWebSocketCompatibility(t *testing.T) {
	for _, supported := range []bool{false, true} {
		name := "old Agent"
		if supported {
			name = "current Agent"
		}
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			token := e.signup(t, "native-mode@example.test", "correct-horse-battery")
			owner := e.meUserID(t, token)
			device, priv := e.seedPairedDevice(t, owner)
			agent := e.agentHandshake(t, device, priv, protocol.AgentCaps{NativeLaunchModes: supported})
			client := e.clientWS(t, token)
			ac, ok := e.srv.reg.Agent(device)
			if !ok {
				t.Fatal("authenticated Agent missing")
			}
			ac.SetCommands([]protocol.CommandAvailability{{ID: "codex", Installed: true, Allowed: true, NativeLaunchModes: []protocol.NativeLaunchMode{{ID: "codex.full-access"}}}})
			payload := protocol.SessionCreatePayload{DeviceID: device, CommandID: "codex", Cwd: "/allowed", LaunchMode: "codex.full-access"}
			request, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionCreate, "", payload)
			sendJSON(t, client, request)
			if !supported {
				if recvEnvelope(t, client).Type != protocol.TypeError {
					t.Fatal("old Agent silently ignored native mode")
				}
				payload.LaunchMode = ""
				request, _ = protocol.NewRequest(uuid.NewString(), protocol.TypeSessionCreate, "", payload)
				sendJSON(t, client, request)
			}
			forwarded := agentRecv(t, agent)
			actual, err := protocol.DecodePayload[protocol.SessionCreatePayload](forwarded)
			if err != nil || actual.LaunchMode != payload.LaunchMode {
				t.Fatal("native choice lost in forwarding", err)
			}
		})
	}
}
