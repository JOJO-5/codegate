package transport

import (
	"testing"
	"time"
)

func TestPriorityFIFOAndBulkFairness(t *testing.T) {
	high, bulk := make(chan int, 24), make(chan int, 3)
	for i := 0; i < 24; i++ {
		high <- i
	}
	for i := 0; i < 3; i++ {
		bulk <- 100 + i
	}
	burst := 0
	for group := 0; group < 3; group++ {
		for i := 0; i < InteractiveBurst; i++ {
			n, event := Next(high, bulk, nil, nil, &burst)
			if event != Message || n != group*8+i {
				t.Fatalf("interactive order: %d %v", n, event)
			}
		}
		n, event := Next(high, bulk, nil, nil, &burst)
		if event != Message || n != 100+group {
			t.Fatalf("bulk starved/reordered: %d %v", n, event)
		}
	}
}

// A slow socket can finish only one frame at a time. After the in-flight
// frame, a key must overtake the already queued file frames, not wait for all.
func TestSlowWriterPrioritizesNewKey(t *testing.T) {
	high, bulk := make(chan string, 1), make(chan string, 16)
	done := make(chan struct{})
	defer close(done)
	wrote, release := make(chan string), make(chan struct{})
	for i := 0; i < 16; i++ {
		bulk <- "file"
	}
	go func() {
		burst := 0
		for {
			msg, event := Next(high, bulk, done, nil, &burst)
			if event == Closed {
				return
			}
			select {
			case wrote <- msg:
			case <-done:
				return
			}
			select {
			case <-release:
			case <-done:
				return
			}
		}
	}()
	if got := <-wrote; got != "file" {
		t.Fatal(got)
	}
	high <- "key"
	release <- struct{}{}
	select {
	case got := <-wrote:
		if got != "key" {
			t.Fatalf("key behind file backlog: %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("writer blocked")
	}
	release <- struct{}{}
	if got := <-wrote; got != "file" {
		t.Fatal("bulk did not resume", got)
	}
}

func TestCancellationAndHeartbeat(t *testing.T) {
	high := make(chan int, 1)
	high <- 1
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	done := make(chan struct{})
	burst := 0
	if _, event := Next(high, nil, done, ticks, &burst); event != Tick {
		t.Fatal("heartbeat starved")
	}
	close(done)
	if _, event := Next(high, nil, done, ticks, &burst); event != Closed {
		t.Fatal("close starved")
	}
}
func TestStatsUnknownAndBulkDoesNotReplaceInteractive(t *testing.T) {
	var stats Stats
	if s := stats.Snapshot(1, 2); s.LastInteractiveWaitMS != nil || s.SampleAgeMS != nil {
		t.Fatal("unknown sample represented as zero")
	}
	stats.ObserveWait(time.Now().Add(-50*time.Millisecond), false)
	before := stats.Snapshot(1, 2)
	stats.ObserveWait(time.Now().Add(-time.Hour), true)
	after := stats.Snapshot(1, 2)
	if *before.LastInteractiveWaitMS < 50 || *after.LastInteractiveWaitMS != *before.LastInteractiveWaitMS || after.InteractiveDepth != 1 || after.BulkDepth != 2 {
		t.Fatalf("bad stats: %+v", after)
	}
}
