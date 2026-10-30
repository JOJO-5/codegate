package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

type reconnectAttemptsWriter struct{ attempts chan int }

func (w reconnectAttemptsWriter) Write(data []byte) (int, error) {
	var event struct {
		Attempt int `json:"attempt"`
	}
	if json.Unmarshal(data, &event) == nil && event.Attempt > 0 {
		w.attempts <- event.Attempt
	}
	return len(data), nil
}

// Exercise the authenticated Run loop, rather than Reset in isolation: a
// healthy connection must not inherit previous outages' exponential delay.
func TestAgentReconnectResetsAfterAuthentication(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := newFakeServer(t, id.PublicKey())
	a := newTestAgent(t, testConfig(t, srv.URL()), id)
	attempts := make(chan int, 10)
	a.log = slog.New(slog.NewJSONHandler(reconnectAttemptsWriter{attempts}, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Agent did not stop after cancellation")
		}
	}()
	for cycle := 1; cycle <= 4; cycle++ {
		srv.waitFor("authenticated session reconciliation", time.Second, func() bool {
			_, _, _, syncs, _ := srv.counts()
			return syncs >= cycle
		})
		srv.DropConnections()
		select {
		case attempt := <-attempts:
			if attempt != 1 {
				t.Fatalf("outage %d after successful authentication: attempt=%d, want 1", cycle, attempt)
			}
		case <-ctx.Done():
			t.Fatal("no reconnect scheduled")
		}
	}
}
