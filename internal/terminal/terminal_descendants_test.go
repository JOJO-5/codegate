//go:build windows || linux || darwin

package terminal

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminalTreeHelper(t *testing.T) {
	switch os.Getenv("CODEGATE_TERMINAL_TREE") {
	case "child":
		ln, err := net.Listen("tcp", os.Getenv("CODEGATE_TERMINAL_ADDR"))
		if err != nil {
			os.Exit(3)
		}
		if os.WriteFile(os.Getenv("CODEGATE_TERMINAL_READY"), []byte("ready"), 0600) != nil {
			os.Exit(4)
		}
		for {
			conn, err := ln.Accept()
			if err != nil {
				os.Exit(5)
			}
			conn.Close()
		}
	case "root":
		cmd := exec.Command(os.Args[0], "-test.run=^TestTerminalTreeHelper$")
		cmd.Env = append(os.Environ(), "CODEGATE_TERMINAL_TREE=child")
		detachTerminalTestChild(cmd)
		if cmd.Start() != nil {
			os.Exit(6)
		}
		if os.Getenv("CODEGATE_TERMINAL_EXIT") == "1" {
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(os.Getenv("CODEGATE_TERMINAL_READY")); err == nil {
					os.Exit(0)
				}
				time.Sleep(10 * time.Millisecond)
			}
			os.Exit(7)
		}
		_ = cmd.Wait()
		os.Exit(0)
	}
}

func TestTerminalCloseKillsOwnedDescendants(t *testing.T) {
	for _, mode := range []string{"running-launcher", "exited-launcher"} {
		t.Run(mode, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			ln.Close()
			ready := filepath.Join(t.TempDir(), "ready")
			env := append(os.Environ(), "CODEGATE_TERMINAL_TREE=root", "CODEGATE_TERMINAL_ADDR="+addr, "CODEGATE_TERMINAL_READY="+ready)
			if mode == "exited-launcher" {
				env = append(env, "CODEGATE_TERMINAL_EXIT=1")
			}
			term := New()
			if err := term.Start(context.Background(), StartConfig{Command: os.Args[0], Args: []string{"-test.run=^TestTerminalTreeHelper$"}, Env: env}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = term.Close() })
			readDone := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, term); close(readDone) }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("descendant not ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mode == "exited-launcher" {
				time.Sleep(100 * time.Millisecond)
			}
			closed := make(chan error, 1)
			go func() { closed <- term.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("Close blocked")
			}
			select {
			case <-readDone:
			case <-time.After(time.Second):
				t.Fatal("reader did not wake")
			}
			deadline = time.Now().Add(5 * time.Second)
			for {
				ln, err = net.Listen("tcp", addr)
				if err == nil {
					ln.Close()
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("owned descendant kept its port: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
