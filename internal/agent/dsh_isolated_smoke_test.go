package agent

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/processutil"
	"github.com/jojo/codegate/internal/protocol"
)

// Opt-in, no models: use the installed CLI entrypoint on an ephemeral port,
// with a private DSH_HOME, leaving the user's managed port/process untouched.
func TestDSHIsolatedLifecycleSmoke(t *testing.T) {
	entry := os.Getenv("CODEGATE_DSH_SMOKE")
	if entry == "" {
		t.Skip("set CODEGATE_DSH_SMOKE to installed dsh lib/bin.js")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	cmd := exec.Command("node", entry, "web", "--no-open", "--port", port, "--trusted-host", "a.dsh.example.test")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "DSH_HOME="+t.TempDir())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	tree, err := processutil.StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tree.Stop(); err != nil {
			t.Error("owned DSH tree cleanup failed")
		}
		_ = cmd.Wait()
	}()
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "dsh web: http://") {
				select {
				case ready <- strings.TrimPrefix(line, "dsh web: "):
				default:
				}
			}
		}
	}()
	var raw string
	select {
	case raw = <-ready:
	case <-time.After(40 * time.Second):
		t.Fatal("isolated DSH did not announce readiness")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Port() != port || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || u.Query().Get("token") == "" {
		t.Fatal("unexpected isolated startup address")
	}
	cookie, err := exchangeDSHBrowserToken(u, "a.dsh.example.test")
	if err != nil {
		t.Fatal("real DSH browser authentication failed")
	}
	if err := checkManagedDSH(u.Host, "a.dsh.example.test", cookie, "a.dsh.example.test"); err != nil {
		t.Fatal("authenticated instance could not be reused")
	}
	if err := checkManagedDSH(u.Host, "a.dsh.example.test", cookie, "changed.example.test"); protocol.ErrorCodeOf(err) != protocol.CodeDSHRestartRequired {
		t.Fatal("host change did not require explicit restart")
	}
	if err := checkManagedDSH(u.Host, "a.dsh.example.test", cookie, "a.dsh.example.test"); err != nil {
		t.Fatal("failed reconnect damaged the original instance")
	}
	if !dshPortInUse(u.Host) {
		t.Fatal("real listener conflict not found")
	}
	t.Log("real DSH ready/authentication/reuse/host-change safety passed; model calls=0")
}
