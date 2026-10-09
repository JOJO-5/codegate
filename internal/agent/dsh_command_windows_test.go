//go:build windows

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jojo/codegate/internal/processutil"
)

func TestDSHWindowsNPMShim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "npm tools & scripts")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "dsh.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\necho %1 %2 %3 %~4\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := dshWebCommand(shim, "codegate.example.test:8443")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	tree, err := processutil.StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Stop()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("npm shim: %v: %s", err, output.String())
	}
	if got := strings.TrimSpace(output.String()); got != "web --no-open --trusted-host codegate.example.test:8443" {
		t.Fatalf("arguments: %q", got)
	}
}
