//go:build linux || darwin

package terminal

import (
	"context"
	"testing"
	"time"
)

// Measure an actual raw-mode PTY echo after its reader has become idle.
func BenchmarkUnixPTYIdleEcho(b *testing.B) {
	term := New()
	if err := term.Start(context.Background(), StartConfig{Command: "/bin/sh", Args: []string{"-c", "stty -echo -icanon min 1 time 0; printf R; exec cat"}}); err != nil {
		b.Fatal(err)
	}
	defer term.Close()
	bytes := make(chan byte, 1024)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := term.Read(buf)
			for _, v := range buf[:n] {
				bytes <- v
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-bytes:
	case <-time.After(3 * time.Second):
		b.Fatal("PTY not ready")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		time.Sleep(time.Millisecond)
		b.StartTimer()
		if _, err := term.Write([]byte{'x'}); err != nil {
			b.Fatal(err)
		}
		select {
		case v := <-bytes:
			if v != 'x' {
				b.Fatalf("echo %q", v)
			}
		case <-time.After(time.Second):
			b.Fatal("echo stalled")
		}
	}
}
