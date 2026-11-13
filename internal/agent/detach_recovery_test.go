package agent

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
)

func TestDetachOfReapedSessionIsIdempotent(t *testing.T) {
	conn := &Conn{send: make(chan outbound, 8), closed: make(chan struct{})}
	a := &Agent{log: testLogger(), mgr: session.NewManager(nil, session.Config{}), views: make(map[uuid.UUID]map[string]struct{})}
	a.setConn(conn)
	sid, other := uuid.New(), uuid.New()
	a.trackView(sid, "closed-view")
	a.trackView(other, "other-view")
	for i := 0; i < 2; i++ {
		req, _ := protocol.NewRequest(uuid.NewString(), protocol.TypeSessionDetach, sid.String(), protocol.SessionDetachPayload{SessionID: sid.String(), AttachID: "closed-view", Reason: "client_close"})
		a.onSessionDetach(req)
		reply, err := protocol.Decode((<-conn.send).data)
		if err != nil || reply.Type != protocol.TypeSessionDetached || reply.ReplyTo != req.RequestID {
			t.Fatal("closed/reaped session detach must acknowledge absence", reply, err)
		}
	}
	if a.viewCount(sid) != 0 || a.viewCount(other) != 1 {
		t.Fatal("detach cleanup crossed session boundary")
	}
	req, _ := protocol.NewRequest("invalid", protocol.TypeSessionDetach, "", protocol.SessionDetachPayload{SessionID: "not-a-uuid"})
	a.onSessionDetach(req)
	reply, _ := protocol.Decode((<-conn.send).data)
	if reply.Type != protocol.TypeError {
		t.Fatal("malformed detach accepted")
	}
}
