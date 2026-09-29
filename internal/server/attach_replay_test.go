package server

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jojo/codegate/internal/protocol"
)

func TestAttachReplayArrivesBeforeAttachedResponse(t *testing.T) {
	e := newTestEnv(t)
	token := e.signup(t, "replay@example.com", "correct-horse-battery")
	userID := e.meUserID(t, token)
	deviceID, priv := e.seedPairedDevice(t, userID)
	agent := e.agentHandshake(t, deviceID, priv)
	client := e.clientWS(t, token)
	sessionID := e.openSession(t, client, agent, deviceID)

	// Detach the first view, then attach again as a restored session.
	detach, err := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionDetach, sessionID,
		protocol.SessionDetachPayload{SessionID: sessionID, Reason: "client_close"})
	if err != nil { t.Fatal(err) }
	sendJSON(t, client, detach)
	fwd := agentRecv(t, agent)
	detachPayload, err := protocol.DecodePayload[protocol.SessionDetachPayload](fwd)
	if err != nil || detachPayload.AttachID == "" || detachPayload.AttachID == detach.RequestID {
		t.Fatalf("detach must use original attach ID: %+v, err=%v", detachPayload, err)
	}
	agentReply(t, agent, fwd, protocol.TypeSessionDetached,
		protocol.SessionDetachedPayload{SessionID: sessionID, Reason: "client_close"})
	if got := recvEnvelope(t, client); got.Type != protocol.TypeSessionDetached { t.Fatal(got.Type) }

	attach, err := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionAttach, sessionID,
		protocol.SessionAttachPayload{SessionID: sessionID, Cols: 80, Rows: 24})
	if err != nil { t.Fatal(err) }
	sendJSON(t, client, attach)
	fwd = agentRecv(t, agent)
	sid := uuid.MustParse(sessionID)
	data := []byte("replayed output")
	frame, err := protocol.EncodeFrame(protocol.FrameBuffer, 0, sid, data)
	if err != nil { t.Fatal(err) }
	if err := agent.WriteMessage(websocket.BinaryMessage, frame); err != nil { t.Fatal(err) }
	agentReply(t, agent, fwd, protocol.TypeSessionAttached,
		protocol.SessionAttachedPayload{Session: protocol.SessionSummary{SessionID:sessionID,Status:"running"},Role:"controller"})
	mt, got, err := client.ReadMessage()
	if err != nil { t.Fatal(err) }
	if mt != websocket.BinaryMessage { t.Fatalf("first message type = %d, expected replay frame", mt) }
	decoded, err := protocol.DecodeFrame(got)
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(decoded.Payload,data) { t.Fatalf("replay = %q", decoded.Payload) }
	if got := recvEnvelope(t, client); got.Type != protocol.TypeSessionAttached { t.Fatal(got.Type) }
}
