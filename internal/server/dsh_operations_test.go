package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

func dshOperationFixture(t *testing.T) (*testEnv, string, string, *AgentConn) {
	t.Helper()
	e := newTestEnv(t)
	token := e.signup(t, "dsh-owner@example.test", "correct-horse-battery")
	owner := e.meUserID(t, token)
	id, _ := e.seedPairedDevice(t, owner)
	e.srv.cfg.DSHProxyURL = "https://codegate.example.test:8443"
	ac := newAgentConn(nil, 16)
	ac.UserID = owner
	ac.DeviceID = id
	ac.caps.DSHWebLifecycle = true
	e.srv.reg.AddAgent(ac)
	t.Cleanup(func() { close(ac.closed) })
	return e, token, id, ac
}
func dshNextRequest(t *testing.T, ac *AgentConn) *protocol.Envelope {
	t.Helper()
	select {
	case out := <-ac.bulk:
		env, err := protocol.Decode(out.data)
		if err != nil {
			t.Fatal(err)
		}
		return env
	case <-time.After(3 * time.Second):
		t.Fatal("missing Agent request")
		return nil
	}
}
func dshWaitOperation(t *testing.T, op *webOperation) {
	t.Helper()
	select {
	case <-op.done:
	case <-time.After(3 * time.Second):
		t.Fatal("missing bounded operation receipt")
	}
}

func TestDSHOperationsShareStartAndPreserveBrowserCancellationReceipt(t *testing.T) {
	e, token, id, ac := dshOperationFixture(t)
	op, err := e.srv.beginWebOperation(ac, id, protocol.TypeWebStart)
	if err != nil {
		t.Fatal(err)
	}
	req := dshNextRequest(t, ac)
	again, err := e.srv.beginWebOperation(ac, id, protocol.TypeWebStart)
	if err != nil || again != op {
		t.Fatal("duplicate start was not shared")
	}
	if _, err = e.srv.beginWebOperation(ac, id, protocol.TypeWebStop); protocol.ErrorCodeOf(err) != "dsh_busy" {
		t.Fatal("stop raced pending start")
	}
	if state := e.get(t, "/api/v1/devices/"+id+"/dsh-web/status", token).JSON(t)["state"]; state != "starting" {
		t.Fatalf("bad waiting feedback: %v", state)
	}
	// A browser leaving its wait must not discard the eventual receipt.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/devices/"+id+"/dsh-web/start", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	w := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(w, r.WithContext(ctx))
	reply, _ := protocol.NewReply(req, protocol.TypeWebStarted, protocol.WebStartedPayload{Cookie: "dsh-auth-fixture=private"})
	e.srv.acceptWebReply(ac, reply)
	dshWaitOperation(t, op)
	if op.result.err != nil {
		t.Fatal(op.result.err)
	}
	status := e.get(t, "/api/v1/devices/"+id+"/dsh-web/status", token)
	if status.JSON(t)["state"] != "forwarding" || strings.Contains(string(status.Body), "private") {
		t.Fatalf("lost receipt or leaked credential: %s", status.Body)
	}
	select {
	case <-ac.bulk:
		t.Fatal("cancelled/duplicate waiter dispatched again")
	default:
	}
}

func TestDSHConfirmedFailureReleasesSimpleReservation(t *testing.T) {
	e, _, id, ac := dshOperationFixture(t)
	op, _ := e.srv.beginWebOperation(ac, id, protocol.TypeWebStart)
	req := dshNextRequest(t, ac)
	e.srv.acceptWebReply(ac, protocol.NewErrorEnvelope(req.RequestID, protocol.NewError(protocol.CodeDSHPortInUse, "private")))
	dshWaitOperation(t, op)
	if e.srv.web.simpleDevice != "" {
		t.Fatal("confirmed failed start retained the sole-device reservation")
	}
}

func TestDSHOldAgentCannotSilentlyRestartDuringReconnection(t *testing.T) {
	e, _, id, ac := dshOperationFixture(t)
	ac.caps.DSHWebLifecycle = false
	op, err := e.srv.beginWebOperation(ac, id, protocol.TypeWebStart)
	if err == nil {
		reply := dshNextRequest(t, ac)
		e.srv.acceptWebReply(ac, protocol.NewErrorEnvelope(reply.RequestID, protocol.NewError(protocol.CodeInternal, "legacy")))
		dshWaitOperation(t, op)
		t.Fatal("legacy Agent accepted safe reconnect without lifecycle capability")
	}
	if protocol.ErrorCodeOf(err) != "dsh_upgrade_required" {
		t.Fatalf("missing recoverable upgrade explanation: %v", err)
	}
	select {
	case <-ac.bulk:
		t.Fatal("unsupported Agent received a side-effecting request")
	default:
	}
}

func TestDSHStatusAuthorizationAndOfflineState(t *testing.T) {
	e, token, id, _ := dshOperationFixture(t)
	other := e.signup(t, "dsh-other@example.test", "correct-horse-battery")
	foreign := e.get(t, "/api/v1/devices/"+id+"/dsh-web/status", other)
	absent := e.get(t, "/api/v1/devices/"+strings.Repeat("b", 32)+"/dsh-web/status", other)
	if foreign.Status != 404 || string(foreign.Body) != string(absent.Body) {
		t.Fatal("status leaked device ownership")
	}
	if e.get(t, "/api/v1/devices/"+id+"/dsh-web/status", "").Status != 401 {
		t.Fatal("anonymous status allowed")
	}
	got := e.get(t, "/api/v1/devices/"+id+"/dsh-web/status", token)
	var status map[string]any
	if err := json.Unmarshal(got.Body, &status); err != nil || status["state"] != "not_open" {
		t.Fatalf("bad initial state %s", got.Body)
	}
}

func TestDSHStopRequiresExactReceiptAndPreservesOtherGrants(t *testing.T) {
	e, token, id, ac := dshOperationFixture(t)
	e.srv.web.sessions["own"] = webGrant{deviceID: id}
	e.srv.web.sessions["other"] = webGrant{deviceID: strings.Repeat("b", 32)}
	op, err := e.srv.beginWebOperation(ac, id, protocol.TypeWebStop)
	if err != nil {
		t.Fatal(err)
	}
	req := dshNextRequest(t, ac)
	if state := e.get(t, "/api/v1/devices/"+id+"/dsh-web/status", token).JSON(t)["state"]; state != "stopping" {
		t.Fatal("stop receipt not pending")
	}
	if _, err := e.srv.beginWebOperation(ac, id, protocol.TypeWebStart); protocol.ErrorCodeOf(err) != "dsh_busy" {
		t.Fatal("start raced stop")
	}
	wrong, _ := protocol.NewReply(req, protocol.TypeWebStarted, protocol.WebStartedPayload{Cookie: "dsh-auth-fixture=private"})
	e.srv.acceptWebReply(ac, wrong)
	dshWaitOperation(t, op)
	if op.result.err == nil || len(e.srv.web.sessions) != 2 {
		t.Fatal("non-stop receipt was displayed as stopped or discarded access")
	}
	op, err = e.srv.beginWebOperation(ac, id, protocol.TypeWebStop)
	if err != nil {
		t.Fatal(err)
	}
	req = dshNextRequest(t, ac)
	ack, _ := protocol.NewReply(req, protocol.TypeWebStopped, nil)
	e.srv.acceptWebReply(ac, ack)
	dshWaitOperation(t, op)
	if op.result.err != nil || len(e.srv.web.sessions) != 1 || e.srv.web.sessions["other"].deviceID == "" {
		t.Fatal("confirmed stop lost unrelated grants")
	}
}
