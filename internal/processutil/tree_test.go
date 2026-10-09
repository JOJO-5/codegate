package processutil

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A launcher that spawns a socket-owning child reproduces npm shims. In exit
// mode the launcher exits first, so cleanup cannot rely on its PID still living.
func TestTreeHelper(t *testing.T) {
	switch os.Getenv("CODEGATE_TREE_HELPER") {
	case "child":
		ln, err := net.Listen("tcp", os.Getenv("CODEGATE_TREE_ADDR"))
		if err != nil {
			os.Exit(3)
		}
		defer ln.Close()
		if os.WriteFile(os.Getenv("CODEGATE_TREE_READY"), []byte("ready"), 0600) != nil {
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
		child := exec.Command(os.Args[0], "-test.run=^TestTreeHelper$")
		child.Env = append(os.Environ(), "CODEGATE_TREE_HELPER=child")
		if child.Start() != nil {
			os.Exit(6)
		}
		if os.Getenv("CODEGATE_TREE_EXIT") == "1" {
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(os.Getenv("CODEGATE_TREE_READY")); err == nil {
					os.Exit(0)
				}
				time.Sleep(10 * time.Millisecond)
			}
			os.Exit(7)
		}
		_ = child.Wait()
		os.Exit(0)
	}
}

func TestTreeStopsDescendantsAndReleasesPort(t *testing.T) {
	// A separately started process must survive cleanup.
	sibling := exec.Command(os.Args[0], "-test.run=^TestTreeHelper$")
	siblingAddr := freeAddress(t)
	siblingReady := filepath.Join(t.TempDir(), "sibling")
	sibling.Env = append(os.Environ(), "CODEGATE_TREE_HELPER=child", "CODEGATE_TREE_ADDR="+siblingAddr, "CODEGATE_TREE_READY="+siblingReady)
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sibling.Process.Kill(); _ = sibling.Wait() }()
	awaitReady(t, siblingReady)
	for _, mode := range []string{"running-launcher", "exited-launcher"} {
		t.Run(mode, func(t *testing.T) {
			addr := freeAddress(t)
			// Reuse the same port to verify start-stop-start, not just PID disappearance.
			for attempt := 0; attempt < 2; attempt++ {
				ready := filepath.Join(t.TempDir(), "ready")
				cmd := exec.Command(os.Args[0], "-test.run=^TestTreeHelper$")
				cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
				cmd.Env = append(os.Environ(), "CODEGATE_TREE_HELPER=root", "CODEGATE_TREE_ADDR="+addr, "CODEGATE_TREE_READY="+ready)
				if mode == "exited-launcher" {
					cmd.Env = append(cmd.Env, "CODEGATE_TREE_EXIT=1")
				}
				tree, err := StartTree(cmd)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() { _ = cmd.Wait(); close(done) }()
				t.Cleanup(func() {
					_ = tree.Stop()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("launcher did not exit")
					}
				})
				awaitReady(t, ready)
				if mode == "exited-launcher" {
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Fatal("launcher did not exit")
					}
				}
				if err := tree.Stop(); err != nil {
					t.Fatal(err)
				}
				if err := tree.Stop(); err != nil {
					t.Fatalf("repeated cleanup: %v", err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					ln, err := net.Listen("tcp", addr)
					if err == nil {
						ln.Close()
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("descendant retained socket: %v", err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				conn, err := net.DialTimeout("tcp", siblingAddr, time.Second)
				if err != nil {
					t.Fatalf("unrelated process killed: %v", err)
				}
				conn.Close()
			}
		})
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}
func awaitReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child listener not ready")
}
