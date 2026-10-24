package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/config"
	"github.com/jojo/codegate/internal/protocol"
)

func TestDSHExpiredNavigationOffersRecoveryWithoutCredentials(t *testing.T) {
	id := strings.Repeat("a", 32)
	now := time.Now()
	s := &Server{cfg: &config.Config{BaseURL: "https://codegate.example.test", DSHProxyDomain: "dsh.example.test"}, web: newWebGateway(), now: func() time.Time { return now }}
	s.web.sessions["expired-private-cookie"] = webGrant{deviceID: id, userID: "owner", expires: now.Add(-time.Second)}
	for _, target := range []string{"/", "/?cg_ticket=expired-private-ticket"} {
		r := httptest.NewRequest("GET", "https://"+id+".dsh.example.test"+target, nil)
		r.Header.Set("Accept", "text/html")
		r.AddCookie(&http.Cookie{Name: webCookieName, Value: "expired-private-cookie"})
		w := httptest.NewRecorder()
		s.serveDSH(w, r)
		if w.Code != 401 || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("browser has no recovery page: %d %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, text := range []string{"重新连接", "https://codegate.example.test/devices/" + id, "viewport"} {
			if !strings.Contains(body, text) {
				t.Fatalf("missing recovery instruction %q", text)
			}
		}
		for _, secret := range []string{"expired-private-cookie", "expired-private-ticket", "<script"} {
			if strings.Contains(body, secret) {
				t.Fatal("recovery page exposed credentials or active script")
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("recovery page lacks privacy headers")
		}
	}
	r := httptest.NewRequest("GET", "https://"+id+".dsh.example.test/api/sessions", nil)
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	s.serveDSH(w, r)
	if w.Code != 401 || strings.Contains(w.Body.String(), "<!doctype") {
		t.Fatal("API failure must not become an HTML page")
	}
}

func TestDSHStartReplyKeepsOnlyRecognizedSafeCause(t *testing.T) {
	for _, code := range []protocol.ErrorCode{"dsh_port_in_use", "dsh_restart_required", "dsh_unhealthy", protocol.CodeInternal} {
		s := &Server{web: newWebGateway()}
		ac := newAgentConn(nil, 4)
		result := make(chan webStartReply, 1)
		s.web.pending["start"] = webStartWait{agent: ac, reply: result}
		s.acceptWebReply(ac, protocol.NewErrorEnvelope("start", protocol.NewError(code, "private-token-and-path")))
		got := <-result
		if code == protocol.CodeInternal {
			if strings.Contains(got.err.Error(), "private-token") {
				t.Fatal("unknown error leaked raw detail")
			}
			continue
		}
		var cause *protocol.CodeError
		if !errors.As(got.err, &cause) || cause.Code != code {
			t.Fatalf("safe cause %s was lost: %v", code, got.err)
		}
		if strings.Contains(cause.Message, "private-token") {
			t.Fatal("Agent supplied arbitrary message was exposed")
		}
	}
}

func TestDSHOfflineIsRecoverableRatherThanRevokedAccess(t *testing.T) {
	e, _, id, ac := dshOperationFixture(t)
	e.srv.cfg.BaseURL = "https://codegate.example.test"
	e.srv.web.simpleDevice = id
	e.srv.web.sessions["valid"] = webGrant{deviceID: id, userID: ac.UserID, expires: e.clock.Now().Add(time.Hour)}
	e.srv.reg.RemoveAgent(ac)
	r := httptest.NewRequest("GET", "https://codegate.example.test:8443/", nil)
	r.Header.Set("Accept", "text/html")
	r.AddCookie(&http.Cookie{Name: webCookieName, Value: "valid"})
	w := httptest.NewRecorder()
	e.srv.serveDSH(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "重新连接") {
		t.Fatalf("offline device was misclassified as revoked access: %d %s", w.Code, w.Body.String())
	}
}
