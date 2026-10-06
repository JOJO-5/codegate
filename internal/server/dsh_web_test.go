package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/config"
	"github.com/jojo/codegate/internal/protocol"
)

func TestDSHTicketIsBoundToDeviceAndUsedOnce(t *testing.T) {
	id := strings.Repeat("a", 32)
	other := strings.Repeat("b", 32)
	s := &Server{cfg: &config.Config{DSHProxyDomain: "dsh.example.test"}, web: newWebGateway(), now: time.Now}
	s.web.tickets["secret"] = webGrant{userID: "owner", deviceID: id, expires: time.Now().Add(time.Minute)}
	req := httptest.NewRequest("GET", "https://"+other+".dsh.example.test/?cg_ticket=secret", nil)
	req.Host = other + ".dsh.example.test"
	w := httptest.NewRecorder()
	s.serveDSH(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("other device got %d", w.Code)
	}
	if len(s.web.sessions) != 0 {
		t.Fatal("cross-device ticket created a session")
	}
	s.web.tickets["secret"] = webGrant{userID: "owner", deviceID: id, expires: time.Now().Add(time.Minute)}
	req = httptest.NewRequest("GET", "https://"+id+".dsh.example.test/?cg_ticket=secret", nil)
	req.Host = id + ".dsh.example.test"
	w = httptest.NewRecorder()
	s.serveDSH(w, req)
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 {
		t.Fatalf("expected host-only cookie redirect, status=%d cookies=%v", w.Code, w.Result().Cookies())
	}
	c := w.Result().Cookies()[0]
	if c.Domain != "" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("weak proxy cookie: %+v", c)
	}
	w = httptest.NewRecorder()
	s.serveDSH(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed ticket got %d", w.Code)
	}
}

func TestDSHProxyRejectsOtherOrigin(t *testing.T) {
	id := strings.Repeat("a", 32)
	s := &Server{cfg: &config.Config{DSHProxyDomain: "dsh.example.test"}, web: newWebGateway(), now: time.Now}
	req := httptest.NewRequest("POST", "https://"+id+".dsh.example.test/api", nil)
	req.Host = id + ".dsh.example.test"
	req.Header.Set("Origin", "https://codegate.example.test")
	w := httptest.NewRecorder()
	s.serveDSH(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation got %d", w.Code)
	}
}

func TestWebTunnelCarriesBytesWithoutBrowserFrames(t *testing.T) {
	s := &Server{web: newWebGateway()}
	ac := &AgentConn{DeviceID: strings.Repeat("a", 32), send: make(chan outbound, 16), closed: make(chan struct{})}
	conn, err := s.openWebStream(context.Background(), ac)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	open := <-ac.send
	env, err := protocol.Decode(open.data)
	if err != nil || env.Type != protocol.TypeWebOpen {
		t.Fatalf("unexpected open frame: %v %v", env, err)
	}
	p, err := protocol.DecodePayload[protocol.WebStreamPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("HTTP request"))
		done <- err
	}()
	data := <-ac.send
	f, err := protocol.DecodeFrame(data.data)
	if err != nil || f.Type != protocol.FrameWebToAgent || string(f.Payload) != "HTTP request" {
		t.Fatalf("tunnel lost request bytes: %v %v", f, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	response, err := protocol.EncodeFrame(protocol.FrameWebToServer, 0, f.StreamID, []byte("HTTP response"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.webFrame(ac, response); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 13)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "HTTP response" {
		t.Fatalf("bad response %q", buf)
	}
	_ = p
}

func TestDSHSimplePortOriginAndTickets(t *testing.T) {
	id := strings.Repeat("a", 32)
	s := &Server{cfg: &config.Config{DSHProxyURL: "https://codegate.example.test:8443"}, web: newWebGateway(), now: time.Now}
	s.web.simpleDevice = id
	if !s.isDSHHost("codegate.example.test:8443") || s.isDSHHost("codegate.example.test") || s.isDSHHost("codegate.example.test:8444") {
		t.Fatal("port isolation failed")
	}
	s.web.tickets["secret"] = webGrant{userID: "owner", deviceID: id, expires: time.Now().Add(time.Minute)}
	request := httptest.NewRequest("GET", "https://codegate.example.test:8443/?cg_ticket=secret", nil)
	w := httptest.NewRecorder()
	s.serveDSH(w, request)
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 {
		t.Fatalf("ticket exchange %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.serveDSH(w, request)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("ticket replay %d", w.Code)
	}
	request = httptest.NewRequest("POST", "https://codegate.example.test:8443/api", nil)
	request.Header.Set("Origin", "https://codegate.example.test")
	w = httptest.NewRecorder()
	s.serveDSH(w, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("main origin accepted %d", w.Code)
	}
	request = httptest.NewRequest("GET", "https://codegate.example.test:8443/", nil)
	w = httptest.NewRecorder()
	s.serveDSH(w, request)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous access %d", w.Code)
	}
}
