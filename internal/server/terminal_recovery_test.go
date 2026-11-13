package server

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

func TestEndedSessionReleasesBrowserAttachment(t *testing.T) {
	reg := NewRegistry(16)
	client := newClientConn("browser", nil, 16)
	reg.AddClient(client)
	old, next := uuid.NewString(), uuid.NewString()
	reg.Subscribe(old, client)
	client.Attach(old)
	client.SetAttachID(old, "old-view")
	reg.setTerminalView("old-view", old, "device", client)
	reg.ForgetSession(old)
	if client.AttachedToOther(next) || client.AttachID(old) != "" {
		t.Fatal("ended session still blocks attaching another terminal")
	}
}

func TestMissingDetachReceiptReleasesOnlyConfirmedMissingView(t *testing.T) {
	for _, code := range []protocol.ErrorCode{protocol.CodeSessionNotFound, protocol.CodeDeviceOffline, protocol.CodeRequestOutcomeUnknown, protocol.CodeForbidden} {
		t.Run(string(code), func(t *testing.T) {
			e := newTestEnv(t)
			ac := newAgentConn(nil, 16)
			ac.DeviceID = "device"
			ac.UserID = "owner"
			client := newClientConn("browser", nil, 16)
			client.UserID = "owner"
			sid := uuid.NewString()
			e.srv.reg.AddClient(client)
			e.srv.reg.Subscribe(sid, client)
			client.Attach(sid)
			client.SetAttachID(sid, "old-view")
			e.srv.reg.setTerminalView("old-view", sid, "device", client)
			e.srv.pending.Add("detach", &pendingReq{client: client, userID: "owner", deviceID: "device", sessionID: sid, kind: protocol.TypeSessionDetach}, time.Now())
			s := &agentSession{srv: e.srv, ac: ac}
			env := protocol.NewErrorEnvelope("detach", protocol.NewError(code, "fixture failure"))
			if !s.routeToClient(env) {
				t.Fatal("reply not routed")
			}
			got := <-client.Send()
			reply, err := protocol.Decode(got.data)
			if err != nil {
				t.Fatal(err)
			}
			if code == protocol.CodeSessionNotFound {
				if client.IsAttached(sid) || client.AttachID(sid) != "" || e.srv.reg.SubscriberCount(sid) != 0 {
					t.Fatal("confirmed missing process left a stale browser attachment")
				}
				if reply.Type != protocol.TypeSessionDetached {
					t.Fatal("confirmed no view should acknowledge detach")
				}
			} else if !client.IsAttached(sid) || reply.Type != protocol.TypeError {
				t.Fatal("uncertain or unauthorized detach must retain its state")
			}
		})
	}
}

func TestAgentDisconnectNotifiesOnlyItsOwnTerminalViews(t *testing.T) {
	reg := NewRegistry(16)
	agent := newAgentConn(nil, 16)
	agent.DeviceID = "device"
	agent.UserID = "owner"
	reg.AddAgent(agent)
	sid := uuid.NewString()
	owner := newClientConn("owner-browser", nil, 16)
	owner.UserID = "owner"
	other := newClientConn("other-browser", nil, 16)
	other.UserID = "other"
	reg.setTerminalView("owner-view", sid, "device", owner)
	reg.setTerminalView("other-user-view", sid, "device", other)
	reg.RemoveAgent(agent)
	select {
	case data := <-owner.Send():
		env, err := protocol.Decode(data.data)
		if err != nil || env.Type != protocol.Type("session.connection") {
			t.Fatal("missing terminal connection event", err)
		}
	default:
		t.Fatal("browser stayed attached/green without learning its Agent disconnected")
	}
	select {
	case <-other.Send():
		t.Fatal("device state crossed account boundary")
	default:
	}
	newer := newAgentConn(nil, 16)
	newer.DeviceID = "device"
	newer.UserID = "owner"
	reg.AddAgent(newer)
	reg.NotifyAgentReady(newer)
	ready := <-owner.Send()
	env, _ := protocol.Decode(ready.data)
	var state struct {
		State string `json:"state"`
	}
	state, _ = protocol.DecodePayload[struct {
		State string `json:"state"`
	}](env)
	if state.State != "ready" {
		t.Fatal("new Agent sync missing its ready event")
	}
	reg.RemoveAgent(agent)
	reg.NotifyAgentReady(agent)
	select {
	case <-owner.Send():
		t.Fatal("old Agent cleanup changed newer connection state")
	default:
	}
}

func TestCloseMissingDetachThenSwitchThroughWebSocket(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "terminal-switch@example.com", "correct-horse-battery")
	user := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, user)
	agent := e.agentHandshake(t, device, priv)
	client := e.clientWS(t, token)
	sid := e.openSession(t, client, agent, device)
	closeRequest, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionClose, sid, protocol.SessionClosePayload{SessionID: sid})
	sendJSON(t, client, closeRequest)
	forwarded := agentRecv(t, agent)
	agentReply(t, agent, forwarded, protocol.TypeSessionClosed, protocol.SessionClosedPayload{SessionID: sid, Reason: "user_requested"})
	if recvEnvelope(t, client).Type != protocol.TypeSessionClosed {
		t.Fatal("close not acknowledged")
	}
	detach, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionDetach, sid, protocol.SessionDetachPayload{SessionID: sid, Reason: "client_close"})
	sendJSON(t, client, detach)
	forwarded = agentRecv(t, agent)
	agentReply(t, agent, forwarded, protocol.TypeError, protocol.ErrorPayload{Code: protocol.CodeSessionNotFound, Message: "already reaped"})
	if recvEnvelope(t, client).Type != protocol.TypeSessionDetached {
		t.Fatal("old Agent's absence not reconciled")
	}
	if e.openSession(t, client, agent, device) == sid {
		t.Fatal("did not switch to a different session")
	}
}

func TestAgentTransportRecoveryWaitsForSessionSync(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "transport-recovery@example.com", "correct-horse-battery")
	user := e.meUserID(t, token)
	device, priv := e.seedPairedDevice(t, user)
	agent := e.agentHandshake(t, device, priv, protocol.AgentCaps{TerminalViews: true})
	client := e.clientWS(t, token)
	sid := e.openSession(t, client, agent, device)
	_ = agent.Close()
	if event := recvEnvelope(t, client); event.Type != protocol.TypeSessionConnection {
		t.Fatal("browser not notified about Agent transport loss")
	}
	reconnected := e.agentHandshake(t, device, priv, protocol.AgentCaps{TerminalViews: true})
	ping, _ := protocol.NewRequest(uuid.NewString(), protocol.TypePing, "", nil)
	sendJSON(t, client, ping)
	if recvEnvelope(t, client).Type != protocol.TypePong {
		t.Fatal("transport authentication was mistaken for completed session sync")
	}
	sync, _ := protocol.NewEnvelope(protocol.TypeSessionSync, protocol.SessionSyncPayload{Sessions: []protocol.SessionSummary{{SessionID: sid, Status: "running", Command: "shell", Cwd: `C:\`}}})
	sendJSON(t, reconnected, sync)
	ready := recvEnvelope(t, client)
	state, err := protocol.DecodePayload[struct {
		State     string `json:"state"`
		SessionID string `json:"session_id"`
	}](ready)
	if err != nil || ready.Type != protocol.TypeSessionConnection || state.State != "ready" || state.SessionID != sid {
		t.Fatal("ready did not identify the original terminal", err)
	}
	e.attach(t, client, reconnected, sid)
}
