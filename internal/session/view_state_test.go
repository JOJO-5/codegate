package session

import (
	"context"
	"sync"
	"testing"
)

type stateSink struct {
	fakeSink
	muState           sync.Mutex
	role              Role
	cols, rows        uint16
	stateBeforeBuffer bool
}

func (s *stateSink) SendState(role Role, cols, rows uint16) {
	s.muState.Lock()
	defer s.muState.Unlock()
	s.role, s.cols, s.rows = role, cols, rows
	s.stateBeforeBuffer = true
}
func (s *stateSink) SendBuffer(p []byte, end bool) bool {
	s.muState.Lock()
	ready := s.stateBeforeBuffer
	s.muState.Unlock()
	if !ready {
		panic("replay before grid state")
	}
	return s.fakeSink.SendBuffer(p, end)
}
func TestTerminalViewsKeepOneControllerAndNativeGrid(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	desktop, mobile := &stateSink{}, &stateSink{}
	s.Attach(AttachRequest{ConnID: "desktop", Sink: desktop})
	s.Attach(AttachRequest{ConnID: "mobile", Sink: mobile, Cols: 40, Rows: 20})
	if mobile.role != RoleViewer || mobile.cols != 100 || mobile.rows != 30 {
		t.Fatalf("viewer grid: %+v", mobile)
	}
	if err := s.Resize("mobile", 40, 20); err != ErrNotController {
		t.Fatalf("viewer resize: %v", err)
	}
	s.Resize("desktop", 160, 45)
	if mobile.cols != 160 || mobile.rows != 45 || s.Summary().Cols != 160 {
		t.Fatal("native grid was not synchronized")
	}
	result, err := s.Attach(AttachRequest{ConnID: "desktop", Sink: desktop})
	if err != nil || result.Role != RoleController {
		t.Fatal("reattach lost controller", err)
	}
	s.ClaimControl("mobile")
	if mobile.role != RoleController || desktop.role != RoleViewer {
		t.Fatal("claim did not update both views")
	}
	s.Resize("mobile", 40, 20)
	if desktop.cols != 40 || desktop.rows != 20 {
		t.Fatal("desktop did not follow mobile grid")
	}
	s.Detach("mobile")
	if desktop.role != RoleController {
		t.Fatal("disconnect did not promote desktop")
	}
}

func TestFailedReplayDoesNotLeavePhantomController(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	s, _ := m.Create(context.Background(), testRequest())
	full := &failedBufferSink{}
	if _, err := s.Attach(AttachRequest{ConnID: "failed", Sink: full}); err == nil {
		t.Fatal("expected full replay failure")
	}
	res, err := s.Attach(AttachRequest{ConnID: "next", Sink: &fakeSink{}})
	if err != nil || res.Role != RoleController {
		t.Fatal("failed attach retained control", err)
	}
}

type failedBufferSink struct{ fakeSink }

func (s *failedBufferSink) SendBuffer([]byte, bool) bool { return false }
