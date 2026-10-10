package protocol

import "testing"

func TestTrafficKeepsStreamLifecycleInOneLane(t *testing.T) {
	for _, typ := range []string{"file.read", "file.read.begin", "file.read.result", "file.cancel", "file.write", "web.open", "web.close", "web.data"} {
		if !BulkTraffic(false, []byte(`{"type":"`+typ+`"}`)) {
			t.Fatal("bulk lifecycle reordered", typ)
		}
	}
	for _, typ := range []string{"session.attached", "session.closed", "session.resize", "ping", "pong"} {
		if BulkTraffic(false, []byte(`{"type":"`+typ+`"}`)) {
			t.Fatal("interactive control in bulk", typ)
		}
	}
	for _, frame := range []FrameType{FrameFileData, FrameWebToAgent, FrameWebToServer} {
		b := make([]byte, BinaryHeaderLen)
		b[1] = byte(frame)
		if !BulkTraffic(true, b) {
			t.Fatal(frame)
		}
	}
	b := make([]byte, BinaryHeaderLen)
	b[1] = byte(FrameStdout)
	if BulkTraffic(true, b) {
		t.Fatal("terminal in bulk")
	}
	if BulkTraffic(true, []byte{0, 0x10}) || BulkTraffic(false, []byte(`broken`)) {
		t.Fatal("invalid input classified")
	}
}
