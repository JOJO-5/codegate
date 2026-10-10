package server

import (
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"testing"
	"time"
)

func TestNetworkProbeMeasuresAgentRoundTripAndLegacyAgent(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy"}[legacy], func(t *testing.T) {
			e := newTestEnv(t)
			token := e.signup(t, "owner@example.com", "correct-horse-battery")
			user := e.meUserID(t, token)
			device, priv := e.seedPairedDevice(t, user)
			agent := e.agentHandshake(t, device, priv)
			client := e.clientWS(t, token)
			sid := e.openSession(t, client, agent, device)
			local, _ := protocol.NewRequest(uuid.NewString(), protocol.TypePing, "", protocol.NetworkProbe{Enabled: true})
			sendJSON(t, client, local)
			result := recvEnvelope(t, client)
			p, err := protocol.DecodePayload[protocol.NetworkProbeResult](result)
			if err != nil || p.Version != 1 || p.ClientQueue == nil || p.ServerAgentMS != nil {
				t.Fatalf("local result: %+v %v", p, err)
			}
			req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypePing, sid, protocol.NetworkProbe{Enabled: true, SessionID: sid})
			sendJSON(t, client, req)
			fwd := agentRecv(t, agent)
			if fwd.Type != protocol.TypePing {
				t.Fatal("probe became terminal input", fwd.Type)
			}
			time.Sleep(30 * time.Millisecond)
			var payload any = protocol.NetworkProbeResult{Version: 1}
			if legacy {
				payload = nil
			}
			agentReply(t, agent, fwd, protocol.TypePong, payload)
			got := recvEnvelope(t, client)
			p, err = protocol.DecodePayload[protocol.NetworkProbeResult](got)
			if err != nil || got.ReplyTo != req.RequestID || p.Version != 1 || p.ServerAgentMS == nil || *p.ServerAgentMS < 30 || p.ClientQueue == nil || p.ServerAgentQueue == nil {
				t.Fatalf("remote result %+v %v", p, err)
			}
			stranger := e.signup(t, "stranger@example.com", "correct-horse-battery")
			other := e.clientWS(t, stranger)
			denied, _ := protocol.NewRequest(uuid.NewString(), protocol.TypePing, sid, protocol.NetworkProbe{Enabled: true, SessionID: sid})
			sendJSON(t, other, denied)
			if got := recvEnvelope(t, other); got.Type != protocol.TypeError {
				t.Fatal("unauthorized probe accepted", got.Type)
			}
		})
	}
}
func TestBulkQueueFullDoesNotBlockTerminalControl(t *testing.T) {
	agent := newAgentConn(nil, 8)
	for i := 0; i < cap(agent.bulk); i++ {
		if err := agent.TrySendText([]byte(`{"type":"file.read.result"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := agent.TrySendText([]byte(`{"type":"file.read.result"}`)); err != ErrSendQueueFull {
		t.Fatal("bulk overload unreported", err)
	}
	if err := agent.TrySendText([]byte(`{"type":"session.resize"}`)); err != nil {
		t.Fatal("bulk blocked control", err)
	}
	if len(agent.send) != 1 || len(agent.bulk) != cap(agent.bulk) {
		t.Fatal("wrong lanes")
	}
}

func TestWrongAgentCannotConsumeProbe(t *testing.T) {
	p := newPendingRegistry()
	req := &pendingReq{deviceID: "device", userID: "owner", kind: protocol.TypePing}
	p.Add("probe", req, time.Now())
	if _, ok := p.TakeFromAgent("probe", "other-device", "owner"); ok {
		t.Fatal("wrong device accepted")
	}
	if _, ok := p.TakeFromAgent("probe", "device", "other-user"); ok {
		t.Fatal("wrong user accepted")
	}
	if got, ok := p.TakeFromAgent("probe", "device", "owner"); !ok || got != req {
		t.Fatal("legitimate pending probe erased")
	}
	if _, ok := p.TakeFromAgent("probe", "device", "owner"); ok {
		t.Fatal("duplicate reply accepted")
	}
}
