package transport

import (
	"sync/atomic"
	"time"
)

// Stats contains timings and sizes only, never terminal/file contents.
type Stats struct {
	waitNS     atomic.Int64
	observedNS atomic.Int64
	writeNS    atomic.Int64
}
type Snapshot struct {
	InteractiveDepth      int      `json:"interactive_depth"`
	BulkDepth             int      `json:"bulk_depth"`
	LastInteractiveWaitMS *float64 `json:"last_interactive_wait_ms,omitempty"`
	SampleAgeMS           *float64 `json:"sample_age_ms,omitempty"`
	LastWriteMS           float64  `json:"last_write_ms"`
}

func (s *Stats) ObserveWait(enqueued time.Time, bulk bool) {
	if bulk || enqueued.IsZero() {
		return
	}
	s.waitNS.Store(time.Since(enqueued).Nanoseconds())
	s.observedNS.Store(time.Now().UnixNano())
}
func (s *Stats) ObserveWrite(started time.Time) { s.writeNS.Store(time.Since(started).Nanoseconds()) }
func (s *Stats) Snapshot(interactive, bulk int) Snapshot {
	out := Snapshot{InteractiveDepth: interactive, BulkDepth: bulk, LastWriteMS: float64(s.writeNS.Load()) / float64(time.Millisecond)}
	if at := s.observedNS.Load(); at != 0 {
		wait := float64(s.waitNS.Load()) / float64(time.Millisecond)
		age := float64(time.Now().UnixNano()-at) / float64(time.Millisecond)
		out.LastInteractiveWaitMS = &wait
		out.SampleAgeMS = &age
	}
	return out
}
func BulkQueueSize(interactive int) int {
	size := interactive / 4
	if size < 1 {
		return 1
	}
	if size > 16 {
		return 16
	}
	return size
}
