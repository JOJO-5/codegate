package agent

import (
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"testing"
	"time"
)

func TestTerminalStateQueueIsNonblockingAndPrecedesBytes(t *testing.T) {
	conn := &Conn{send: make(chan outbound, 2), closed: make(chan struct{})}
	a := &Agent{}
	a.setConn(conn)
	a.terminalViews.Store(true)
	sink := &sessionSink{a: a, sid: uuid.New(), attachID: "private-view"}
	conn.send <- outbound{}
	conn.send <- outbound{}
	start := time.Now()
	sink.SendState(session.RoleViewer, 160, 45)
	if time.Since(start) > time.Second {
		t.Fatal("state blocked session lock")
	}
	if sink.SendOutput([]byte("output"), false) {
		t.Fatal("sent bytes without grid state")
	}
	<-conn.send
	<-conn.send
	if !sink.SendOutput([]byte("output"), true) {
		t.Fatal("queue did not recover")
	}
	state := <-conn.send
	data := <-conn.send
	if state.binary || !data.binary {
		t.Fatal("state must precede output")
	}
	env, err := protocol.Decode(state.data)
	if err != nil || env.Type != protocol.TypeSessionRoleChanged {
		t.Fatal("bad state envelope", err)
	}
	_, _, id, err := protocol.PeekFrameHeader(data.data)
	if err != nil || id != protocol.TerminalViewID("private-view") {
		t.Fatal("output was not directed to view")
	}
}
