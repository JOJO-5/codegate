//go:build linux || darwin

package terminal

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func readUntil(t *testing.T, terminal Terminal, marker string) string {
	t.Helper()
	result := make(chan string, 1)
	errors := make(chan error, 1)
	go func() {
		var out strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := terminal.Read(buf)
			out.Write(buf[:n])
			if strings.Contains(out.String(), marker) {
				result <- out.String()
				return
			}
			if err != nil {
				errors <- err
				return
			}
		}
	}()
	select {
	case text := <-result:
		return text
	case err := <-errors:
		t.Fatalf("PTY read before %q: %v", marker, err)
	case <-time.After(5 * time.Second):
		_ = terminal.Close()
		t.Fatalf("PTY read timed out before %q", marker)
	}
	return ""
}

func TestUnixPTYReadWriteResize(t *testing.T) {
	if err := Available(); err != nil {
		t.Fatal(err)
	}
	term := New()
	err := term.Start(context.Background(), StartConfig{
		Command: "/bin/sh",
		Args: []string{"-c", "stty -echo; stty size; printf '\\nREADY\\n'; IFS= read -r line; stty size; printf '\\nGOT:%s\\n' \"$line\""},
		Cols: 80,
		Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	initial := readUntil(t, term, "READY")
	if !strings.Contains(initial, "24 80") {
		t.Fatalf("initial PTY size missing from %q", initial)
	}
	if err := term.Resize(100, 40); err != nil {
		t.Fatal(err)
	}
	if n, err := term.Write([]byte("hello\n")); err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	output := readUntil(t, term, "GOT:hello")
	if !strings.Contains(output, "40 100") {
		t.Fatalf("resized PTY size missing from %q", output)
	}
	// Wait is idempotent and returns the real shell exit result.
	done := make(chan ExitResult, 1)
	go func() { done <- term.Wait() }()
	select {
	case result := <-done:
		if !result.Success() {
			t.Fatalf("Wait = %s", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait timed out")
	}
}

func TestUnixPTYCloseUnblocksRead(t *testing.T) {
	term := New()
	if err := term.Start(context.Background(), StartConfig{
		Command: "/bin/sh", Args: []string{"-c", "sleep 60"},
	}); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		_, err := term.Read(buf)
		readDone <- err
	}()
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err != io.EOF {
			t.Fatalf("Read after Close = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
	reaped := make(chan ExitResult, 1)
	go func() { reaped <- term.Wait() }()
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("child process was not reaped")
	}
}
