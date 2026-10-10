package agent

import (
	"bufio"
	"context"
	"github.com/jojo/codegate/internal/processutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Run with CODEGATE_TEST_DSH=1 after installing the real dsh CLI.
func TestRealDSHBrowserTokenExchange(t *testing.T) {
	if os.Getenv("CODEGATE_TEST_DSH") != "1" {
		t.Skip("requires installed dsh")
	}
	t.Run("first-start", testRealDSHBrowserTokenExchange)
	t.Run("restart-after-stop", testRealDSHBrowserTokenExchange)
}

func testRealDSHBrowserTokenExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	binary, err := exec.LookPath("dsh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := dshWebCommand(binary, "a.dsh.example.test")
	cmd.Env = append(os.Environ(), "DSH_HOME="+t.TempDir())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Never print stderr or the startup URL: both can contain credentials.
	cmd.Stderr = nil
	tree, err := processutil.StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tree.Stop(); err != nil {
			t.Errorf("stop DSH tree: %v", err)
		}
		_ = cmd.Wait()
		deadline := time.Now().Add(5 * time.Second)
		for {
			ln, err := net.Listen("tcp4", "127.0.0.1:3080")
			if err == nil {
				ln.Close()
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("DSH retained its port after stop: %v", err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	urls := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "dsh web: http://") {
				urls <- strings.TrimPrefix(line, "dsh web: ")
				return
			}
		}
	}()
	var launchURL string
	select {
	case launchURL = <-urls:
	case <-ctx.Done():
		t.Fatal("dsh did not print a launch URL")
	}
	cookie, err := bootstrapDSH(launchURL, "a.dsh.example.test")
	if err != nil {
		t.Fatalf("real dsh token exchange failed: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+dshLoopback+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "a.dsh.example.test"
	req.Header.Set("Cookie", cookie)
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dsh rejected exchanged cookie: status %d", res.StatusCode)
	}
}
