package server

import (
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"io"
	"log/slog"
	"testing"
)

func TestTargetedTerminalReplayAndOutputDoNotBroadcast(t *testing.T) {
	reg := NewRegistry(16)
	owner := newAgentConn(nil, 16)
	owner.UserID = "owner"
	owner.DeviceID = "device"
	owner.caps.TerminalViews = true
	sid := uuid.NewString()
	owner.AddSession(sid)
	desktop := newClientConn("desktop", nil, 16)
	desktop.UserID = "owner"
	mobile := newClientConn("mobile", nil, 16)
	mobile.UserID = "owner"
	reg.Subscribe(sid, desktop)
	reg.Subscribe(sid, mobile)
	reg.setTerminalView("view-desktop", sid, "device", desktop)
	reg.setTerminalView("view-mobile", sid, "device", mobile)
	reg.AddAgent(owner)
	reg.SetSessionOwner(sid, owner.DeviceID)
	desktop.Attach(sid)
	desktop.SetAttachID(sid, "view-desktop")
	mobile.Attach(sid)
	mobile.SetAttachID(sid, "view-mobile")
	reg.setTerminalRole(protocol.TerminalViewID("view-mobile").String(), "controller")
	relay := NewRelay(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, typ := range []protocol.FrameType{protocol.FrameBuffer, protocol.FrameStdout} {
		data, _ := protocol.EncodeFrame(typ, 0, protocol.TerminalViewID("view-mobile"), []byte("once"))
		if err := relay.RouteFrameToClients(owner, data); err != nil {
			t.Fatal(err)
		}
		got := <-mobile.Send()
		_, _, stream, err := protocol.PeekFrameHeader(got.data)
		if err != nil || stream.String() != sid {
			t.Fatal("browser stream ID was not rewritten")
		}
		select {
		case <-desktop.Send():
			t.Fatal("mobile replay/output polluted desktop")
		default:
		}
	}
	input, _ := protocol.EncodeFrame(protocol.FrameStdin, 0, uuid.MustParse(sid), []byte("key"))
	if err := relay.RouteFrameToAgent(desktop, input); err == nil {
		t.Fatal("viewer input accepted")
	}
	if err := relay.RouteFrameToAgent(mobile, input); err != nil {
		t.Fatal(err)
	}
	nativeInput := <-owner.Send()
	_, _, id, err := protocol.PeekFrameHeader(nativeInput.data)
	if err != nil || id != protocol.TerminalViewID("view-mobile") {
		t.Fatal("input lost authenticated view identity")
	}
	other := newAgentConn(nil, 16)
	other.UserID = "owner"
	other.DeviceID = "other"
	other.AddSession(sid)
	data, _ := protocol.EncodeFrame(protocol.FrameBuffer, 0, protocol.TerminalViewID("view-mobile"), []byte("forged"))
	if relay.RouteFrameToClients(other, data) == nil {
		t.Fatal("cross-device frame accepted")
	}
	reg.Unsubscribe(sid, mobile)
	if _, ok := reg.terminalView(protocol.TerminalViewID("view-mobile").String()); ok {
		t.Fatal("detached route leaked")
	}
	if err := relay.RouteFrameToClients(owner, data); err != nil {
		t.Fatal("stale frame must not disconnect Agent", err)
	}
	select {
	case <-desktop.Send():
		t.Fatal("stale frame was broadcast")
	default:
	}
}
