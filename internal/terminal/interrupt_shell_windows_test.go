//go:build windows

package terminal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise actual shells rather than treating ReadConsole's cancelled read as
// proof that an interactive shell has closed. Every case owns its process tree.
func TestConPTYShellInterruptKeepsSession(t *testing.T) {
	if testing.Short() {
		t.Skip("real ConPTY shells")
	}
	type scenario struct {
		name, command, echo string
		args                []string
		start, ready, after string
	}
	tests := []scenario{
		{"cmd_native", filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "127.0.0.1", []string{"/d", "/q"}, "ping -t 127.0.0.1\r", "TTL=", "echo CG_A^FTER\r"},
	}
	for _, name := range []string{"powershell.exe", "pwsh.exe"} {
		command, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		args := []string{"-NoLogo", "-NoProfile", "-NoExit", "-Command", "function prompt { 'CG_PROMPT> ' }"}
		tests = append(tests,
			scenario{name + "_native", command, "127.0.0.1", args, "ping -t 127.0.0.1\r", "TTL=", "Write-Output ('CG_'+'AFTER')\r"},
			scenario{name + "_builtin", command, "60", args, "Write-Output ('CG_'+'RUNNING'); Start-Sleep -Seconds 60\r", "CG_RUNNING", "Write-Output ('CG_'+'AFTER')\r"},
		)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := New()
			defer p.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := p.Start(ctx, StartConfig{Command: tc.command, Args: tc.args, Dir: t.TempDir(), Env: append(childEnv(), "PROMPT=CG_PROMPT$G "), Cols: 120, Rows: 30}); err != nil {
				t.Fatal(err)
			}
			rd := startReader(p)
			if output, ok := rd.waitFor("CG_PROMPT>", 15*time.Second); !ok {
				t.Fatalf("shell not ready: %s", sanitize(output))
			}
			before := strings.Count(rd.snapshot(), "CG_PROMPT>")
			writeCommand := func(command, echo string) {
				t.Helper()
				if _, err := p.Write([]byte(strings.TrimSuffix(command, "\r"))); err != nil {
					t.Fatal(err)
				}
				if output, ok := rd.waitFor(echo, 10*time.Second); !ok {
					t.Fatalf("command not echoed: %s", sanitize(output))
				}
				if _, err := p.Write([]byte("\r")); err != nil {
					t.Fatal(err)
				}
			}
			writeCommand(tc.start, tc.echo)
			if output, ok := rd.waitFor(tc.ready, 10*time.Second); !ok {
				t.Fatalf("owned command not running: %s", sanitize(output))
			}
			if err := p.Signal(SignalInterrupt); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for strings.Count(rd.snapshot(), "CG_PROMPT>") <= before && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if strings.Count(rd.snapshot(), "CG_PROMPT>") <= before {
				t.Fatalf("interrupt did not return to shell: %s", sanitize(rd.snapshot()))
			}
			writeCommand(tc.after, "FTER")
			if output, ok := rd.waitFor("CG_AFTER", 5*time.Second); !ok {
				t.Fatalf("shell cannot execute after interrupt: %s", sanitize(output))
			}
		})
	}
}
