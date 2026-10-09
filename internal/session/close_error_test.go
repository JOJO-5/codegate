package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

type failingClosePTY struct {
	*fakePTY
	err error
}

func (p *failingClosePTY) Close() error { _ = p.fakePTY.Close(); return p.err }

func TestSessionCloseReportsProcessCleanupFailure(t *testing.T) {
	expected := errors.New("process tree termination failed")
	p := &failingClosePTY{fakePTY: newFakePTY(123), err: expected}
	s := newSession(context.Background(), testRequest(), p, 4096, time.Now())
	if err := s.Close("user_requested"); !errors.Is(err, expected) {
		t.Fatalf("cleanup failure hidden: %v", err)
	}
	if err := s.Close("user_requested"); !errors.Is(err, expected) {
		t.Fatalf("repeated close lost error: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("reader did not exit")
	}
}
