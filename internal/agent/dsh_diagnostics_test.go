package agent

import (
	"github.com/jojo/codegate/internal/protocol"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDSHDiagnosticSurvivesAgentErrorMapping(t *testing.T) {
	for _, code := range []protocol.ErrorCode{"dsh_port_in_use", "dsh_restart_required", "dsh_unhealthy", "dsh_not_installed", "dsh_start_failed"} {
		got := toCodeError(protocol.NewError(code, "safe diagnostic"))
		if got.Code != code {
			t.Fatalf("%s becomes unhelpful %s", code, got.Code)
		}
	}
}

func TestDSHExternalPortProbeDoesNotAuthenticateOrStopListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if !dshPortInUse(listener.Addr().String()) {
		t.Fatal("external port conflict missed")
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buffer := make([]byte, 1)
	if n, _ := conn.Read(buffer); n != 0 {
		t.Fatal("port probe sent bytes or credentials to external service")
	}
}

func TestDSHManagedReconnectChecksHealthAndNeverReauthenticates(t *testing.T) {
	hits := 0
	status := http.StatusOK
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.RawQuery != "" || r.Header.Get("Cookie") != "dsh-auth-fixture=private" || r.Host != "dsh.example.test" {
			t.Error("incorrect managed instance probe")
		}
		w.WriteHeader(status)
	}))
	defer service.Close()
	address := strings.TrimPrefix(service.URL, "http://")
	if err := checkManagedDSH(address, "dsh.example.test", "dsh-auth-fixture=private", "dsh.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := checkManagedDSH(address, "dsh.example.test", "private", "changed.example.test"); protocol.ErrorCodeOf(err) != protocol.CodeDSHRestartRequired || hits != 1 {
		t.Fatal("changed authority contacted or replaced old instance")
	}
	status = http.StatusUnauthorized
	if err := checkManagedDSH(address, "dsh.example.test", "dsh-auth-fixture=private", "dsh.example.test"); protocol.ErrorCodeOf(err) != protocol.CodeDSHUnhealthy {
		t.Fatal("stale authentication was treated as a healthy process")
	}
}
