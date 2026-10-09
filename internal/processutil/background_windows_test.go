//go:build windows

package processutil

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestBackgroundChildHasNoConsole(t *testing.T) {
	if os.Getenv("CODEGATE_TEST_BACKGROUND_CHILD") == "1" {
		window, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		if window != 0 {
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString("background-stdout-ok")
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBackgroundChildHasNoConsole$")
	cmd.Env = append(os.Environ(), "CODEGATE_TEST_BACKGROUND_CHILD=1")
	attrs := &syscall.SysProcAttr{CreationFlags: 0x00000400} // CREATE_UNICODE_ENVIRONMENT
	cmd.SysProcAttr = attrs
	Background(cmd)
	if cmd.SysProcAttr != attrs || attrs.CreationFlags&0x00000400 == 0 {
		t.Fatal("existing process attributes lost")
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("console-free child: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) != "background-stdout-ok" {
		t.Fatalf("stdout lost: %q", output)
	}
}
