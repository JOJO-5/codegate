package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/jojo/codegate/internal/protocol"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestWaitingDoesNotChangeLastVersionCheck(t *testing.T) {
	a := idleActivityAgent(t)
	a.setUpdateStatus("checking", "", "checking")
	before := a.currentUpdateStatus().CheckedAt
	time.Sleep(2 * time.Millisecond)
	a.setUpdateStatus("waiting", "", "busy")
	if a.currentUpdateStatus().CheckedAt != before {
		t.Fatal("waiting changed the last version check timestamp")
	}
}

func TestUpdateLoopReportsAndResetsNextCheck(t *testing.T) {
	a := idleActivityAgent(t)
	a.cfg.UpdateEnabled = true
	a.cfg.ServerURL = "ws://localhost"
	a.updateWake = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.updateLoop(ctx) }()
	defer func() { cancel(); <-done }()
	wait := func(previous int64) protocol.AgentUpdateStatus {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			s := a.currentUpdateStatus()
			if s.NextCheckAt > previous {
				return s
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("next automatic check was not scheduled")
		return protocol.AgentUpdateStatus{}
	}
	first := wait(0)
	if first.NextCheckAt <= first.CheckedAt {
		t.Fatal("next check precedes completed attempt")
	}
	time.Sleep(2 * time.Millisecond)
	a.updateWake <- struct{}{}
	second := wait(first.NextCheckAt)
	if second.CheckedAt <= first.CheckedAt {
		t.Fatal("manual check did not update check time")
	}
}

func TestCancelRequestedRestartKeepsWorkAndPackage(t *testing.T) {
	a := idleActivityAgent(t)
	a.restartRequested.Store(true)
	a.restartReady = true
	a.stagedPath = "verified-package"
	release, _ := a.beginActivity()
	defer release()
	if err := a.cancelRequestedRestart(); err != nil {
		t.Fatal(err)
	}
	if a.restartRequested.Load() || a.stagedPath != "verified-package" {
		t.Fatal("cancel lost package or kept restart")
	}
	count, _ := a.activityState()
	if count != 1 {
		t.Fatal("cancel interrupted work")
	}
	status := a.currentUpdateStatus()
	if status.BlockingOperations != 1 {
		t.Fatal("active operation missing from heartbeat")
	}
	a.stagedPath = ""
	a.OnRestart = func() error { t.Fatal("cancelled restart ran"); return nil }
	a.applyStagedUpdate()
	a.setUpdateStatus("switching", "", "")
	if a.cancelRequestedRestart() == nil {
		t.Fatal("accepted cancellation after switching")
	}
}

func TestBusyAgentStagesThenSwitchesWithoutRefetch(t *testing.T) {
	payload := []byte("next agent")
	sum := sha256.Sum256(payload)
	hits := 0
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/binary" {
			w.Write(payload)
			return
		}
		json.NewEncoder(w).Encode(updateManifest{Version: "v1.2.4", OS: runtime.GOOS, Arch: runtime.GOARCH, URL: server.URL + "/binary", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload))})
	}))
	defer server.Close()
	oldClient, oldVersion := updateHTTPClient, Version
	updateHTTPClient, Version = server.Client(), "v1.2.3"
	defer func() { updateHTTPClient, Version = oldClient, oldVersion }()
	a := idleActivityAgent(t)
	a.cfg = Config{StateDir: t.TempDir(), ServerURL: server.URL, UpdateEnabled: true}
	a.log = slog.Default()
	staged, switched := 0, 0
	a.OnStagedUpdate = func(path, version string) error { staged++; return nil }
	a.OnVerifiedUpdate = func(path, version string) error {
		if !a.FreezeForUpdate() {
			return ErrUpdateBusy
		}
		switched++
		return nil
	}
	release, _ := a.beginActivity()
	a.checkUpdate(context.Background())
	if staged != 1 || switched != 0 || a.currentUpdateStatus().Version != "v1.2.4" {
		t.Fatalf("busy download: staged=%d switched=%d status=%+v", staged, switched, a.currentUpdateStatus())
	}
	release()
	a.applyStagedUpdate()
	if switched != 1 || hits != 2 {
		t.Fatalf("idle switch: switched=%d hits=%d", switched, hits)
	}
}

func TestRequestedRestartWaitsForActivityEvenWithoutNewVersion(t *testing.T) {
	a := idleActivityAgent(t)
	a.restartRequested.Store(true)
	a.restartReady = true
	restarts := 0
	a.OnRestart = func() error {
		if !a.FreezeForUpdate() {
			return ErrUpdateBusy
		}
		restarts++
		return nil
	}
	release, _ := a.beginActivity()
	a.applyStagedUpdate()
	if restarts != 0 || a.currentUpdateStatus().State != "waiting" {
		t.Fatal("restart interrupted active work")
	}
	release()
	a.applyStagedUpdate()
	a.applyStagedUpdate()
	if restarts != 1 || a.currentUpdateStatus().State != "switching" {
		t.Fatalf("restarts=%d status=%+v", restarts, a.currentUpdateStatus())
	}
}
func TestFailedCheckDoesNotRestartOrHideFailure(t *testing.T) {
	a := idleActivityAgent(t)
	a.cfg = Config{StateDir: t.TempDir(), ServerURL: "ws://localhost/ws/agent"}
	a.restartRequested.Store(true)
	a.restartReady = true
	a.OnRestart = func() error { t.Error("restarted after failed update check"); return nil }
	a.checkUpdate(context.Background())
	a.applyStagedUpdate()
	if a.currentUpdateStatus().State != "error" {
		t.Fatal("failure status hidden")
	}
}

func TestUpdateControlAcknowledgesOverSocketBeforeScheduling(t *testing.T) {
	received := make(chan *protocol.Envelope, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Decode(raw)
		if err == nil {
			received <- env
		}
	}))
	defer server.Close()
	conn, err := Dial(context.Background(), DialOptions{URL: "ws" + server.URL[4:], Log: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	oldVersion := Version
	Version = "v1.2.3"
	defer func() { Version = oldVersion }()
	a := idleActivityAgent(t)
	a.conn, a.log, a.updateWake = conn, slog.Default(), make(chan struct{}, 1)
	a.OnRestart = func() error { return nil }
	env, err := protocol.NewRequest("update-request", protocol.TypeAgentUpdateControl, "", protocol.AgentUpdateControl{Action: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	a.onUpdateControl(env)
	select {
	case reply := <-received:
		if reply.Type != protocol.TypeAgentUpdateAccepted || reply.ReplyTo != env.RequestID {
			t.Fatalf("ack: %+v", reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no acknowledgement on socket")
	}
	if !a.restartRequested.Load() || len(a.updateWake) != 1 {
		t.Fatal("restart not scheduled after acknowledgement")
	}
}
