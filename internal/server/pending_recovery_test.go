package server

import (
	"testing"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

func TestPendingDisconnectReportsUnconfirmedOutcome(t *testing.T) {
	p := newPendingRegistry()
	c := newClientConn("browser", nil, 4)
	released := 0
	p.Add("restore-request", &pendingReq{client: c, deviceID: "device", kind: protocol.TypeConversationRestore, release: func() { released++ }}, time.Now())
	p.Add("other-request", &pendingReq{client: c, deviceID: "other", kind: protocol.TypeConversationRestore}, time.Now())
	if p.DropDevice("device") != 1 || released != 1 || p.Len() != 1 {
		t.Fatal("disconnect must release only this device's request")
	}
	assertUnconfirmedReply(t, c, "restore-request")
	if p.DropDevice("device") != 0 {
		t.Fatal("duplicate disconnect consumed another request")
	}
	select {
	case <-c.Send():
		t.Fatal("disconnect notified twice")
	default:
	}
}

func TestPendingExpiryCorrelatesUnconfirmedReply(t *testing.T) {
	p := newPendingRegistry()
	c := newClientConn("browser", nil, 4)
	now := time.Now()
	p.Add("expired-restore", &pendingReq{client: c, kind: protocol.TypeConversationRestore}, now)
	p.sweep(now.Add(pendingTTL + time.Second))
	assertUnconfirmedReply(t, c, "expired-restore")
}

func assertUnconfirmedReply(t *testing.T, c *ClientConn, requestID string) {
	t.Helper()
	select {
	case frame := <-c.Send():
		env, err := protocol.Decode(frame.data)
		if err != nil || env.ReplyTo != requestID || env.Type != protocol.TypeError {
			t.Fatalf("missing correlated error for %s: %+v %v", requestID, env, err)
		}
		failure, err := protocol.DecodePayload[protocol.CodeError](env)
		if err != nil || failure.Code != "request_outcome_unknown" || failure.Retryable {
			t.Fatalf("dispatched operation must not be labelled safely retryable: %+v %v", failure, err)
		}
	default:
		t.Fatal("browser was left waiting after Agent disconnected or request expired")
	}
}
