package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

const webCookieName = "cg_dsh"
const webTicketTTL = 60 * time.Second
const webSessionTTL = 60 * time.Minute
const webLoopback = "localhost:3080"

type webGrant struct {
	userID, deviceID string
	expires          time.Time
}
type webUpstream struct {
	agent  *AgentConn
	cookie string
}
type webStartReply struct {
	cookie string
	err    error
}
type webStartWait struct {
	agent *AgentConn
	reply chan webStartReply
}
type toolWait struct {
	agent *AgentConn
	reply chan error
}
type proxyStream struct {
	agent  *AgentConn
	pipe   net.Conn
	client net.Conn
	queue  chan []byte
	done   chan struct{}
	once   sync.Once
}

type webGateway struct {
	mu           sync.Mutex
	simpleDevice string // URL mode serves one device until explicitly stopped.
	simpleStarts int
	tickets      map[string]webGrant
	sessions     map[string]webGrant
	upstreams    map[string]webUpstream
	pending      map[string]webStartWait
	toolPending  map[string]toolWait
	streams      map[uuid.UUID]*proxyStream
}

func newWebGateway() *webGateway {
	return &webGateway{
		tickets: make(map[string]webGrant), sessions: make(map[string]webGrant),
		upstreams: make(map[string]webUpstream), pending: make(map[string]webStartWait), toolPending: make(map[string]toolWait),
		streams: make(map[uuid.UUID]*proxyStream),
	}
}

func (g *webGateway) sweep(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for token, grant := range g.tickets {
		if !now.Before(grant.expires) {
			delete(g.tickets, token)
		}
	}
	for token, grant := range g.sessions {
		if !now.Before(grant.expires) {
			delete(g.sessions, token)
		}
	}
}
func randomWebToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

func (s *Server) dshProxyEnabled() bool { return s.cfg.DSHProxyDomain != "" || s.cfg.DSHProxyURL != "" }
func (s *Server) isDSHHost(raw string) bool {
	if s.cfg.DSHProxyURL != "" {
		u, _ := url.Parse(s.cfg.DSHProxyURL)
		return strings.EqualFold(raw, u.Host)
	}

	host := strings.TrimSuffix(strings.ToLower(raw), ":443")
	return strings.HasSuffix(host, "."+s.cfg.DSHProxyDomain)
}

func (s *Server) dshDeviceHost(deviceID string) string {
	if s.cfg.DSHProxyURL != "" {
		u, _ := url.Parse(s.cfg.DSHProxyURL)
		return u.Host
	}
	return deviceID + "." + s.cfg.DSHProxyDomain
}

func (s *Server) deviceFromDSHHost(raw string) (string, bool) {
	host := strings.TrimSuffix(strings.ToLower(raw), ":443")
	suffix := "." + s.cfg.DSHProxyDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(host, suffix)
	if len(id) != 32 {
		return "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", false
	}
	return id, true
}

// POST with a normal CodeGate Bearer token opens the host service, then returns
// a one-use navigation URL. The DSH token and cookie never go to this browser.
func (s *Server) handleDSHWebStart(w http.ResponseWriter, r *http.Request) {
	if !s.dshProxyEnabled() {
		writeError(w, http.StatusNotImplemented, "unavailable", "DSH Web 代理未配置")
		return
	}
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")
	if _, err := s.authorizeDevice(r.Context(), userID, deviceID); err != nil {
		writeDeviceError(w, err)
		return
	}
	if len(deviceID) != 32 {
		writeError(w, http.StatusBadRequest, "invalid_device", "设备 ID 非法")
		return
	}
	if _, err := hex.DecodeString(deviceID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_device", "设备 ID 非法")
		return
	}
	ac, ok := s.reg.Agent(deviceID)
	if !ok {
		writeError(w, http.StatusConflict, "device_offline", "设备离线")
		return
	}
	if s.cfg.DSHProxyURL != "" {
		s.web.mu.Lock()
		// Disconnected devices cannot retain the sole simple-mode reservation.
		if old := s.web.upstreams[s.web.simpleDevice]; old.agent != nil {
			select {
			case <-old.agent.Done():
				delete(s.web.upstreams, s.web.simpleDevice)
				s.web.simpleDevice = ""
			default:
			}
		}
		if s.web.simpleDevice != "" && s.web.simpleDevice != deviceID {
			s.web.mu.Unlock()
			writeError(w, http.StatusConflict, "dsh_in_use", "简化转发正在服务另一台设备，请先停止该设备的 DSH Web；多设备同时使用可配置通配域名")
			return
		}
		s.web.simpleDevice = deviceID
		s.web.simpleStarts++
		s.web.mu.Unlock()
		defer func() {
			s.web.mu.Lock()
			defer s.web.mu.Unlock()
			s.web.simpleStarts--
			if _, ok := s.web.upstreams[deviceID]; !ok && s.web.simpleDevice == deviceID && s.web.simpleStarts == 0 {
				s.web.simpleDevice = ""
			}
		}()
	}
	reqID := uuid.NewString()
	reply := make(chan webStartReply, 1)
	s.web.mu.Lock()
	s.web.pending[reqID] = webStartWait{agent: ac, reply: reply}
	s.web.mu.Unlock()
	defer func() {
		s.web.mu.Lock()
		delete(s.web.pending, reqID)
		s.web.mu.Unlock()
	}()
	env, err := protocol.NewRequest(reqID, protocol.TypeWebStart, "", protocol.WebStartPayload{Host: s.dshDeviceHost(deviceID)})
	if err != nil {
		writeProtoError(w, err)
		return
	}
	raw, err := protocol.Encode(env)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	if err := ac.TrySendText(raw); err != nil {
		writeError(w, http.StatusServiceUnavailable, "agent_busy", "Agent 忙碌")
		return
	}
	timer := time.NewTimer(45 * time.Second)
	defer timer.Stop()
	select {
	case started := <-reply:
		if started.err != nil || started.cookie == "" {
			writeError(w, http.StatusServiceUnavailable, "dsh_unavailable", "DSH Web 启动失败：请检查 Agent 配置、dsh 安装与本机端口")
			return
		}
		s.web.mu.Lock()
		s.web.upstreams[deviceID] = webUpstream{agent: ac, cookie: started.cookie}
		s.web.mu.Unlock()
	case <-r.Context().Done():
		return
	case <-ac.Done():
		writeError(w, http.StatusServiceUnavailable, "device_offline", "Agent 已断开")
		return
	case <-timer.C:
		writeError(w, http.StatusGatewayTimeout, "dsh_timeout", "等待 DSH Web 启动超时")
		return
	}
	token, err := randomWebToken()
	if err != nil {
		writeProtoError(w, err)
		return
	}
	s.web.mu.Lock()
	if len(s.web.tickets) >= 1024 {
		s.web.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "busy", "打开票据过多，请稍后再试")
		return
	}
	s.web.tickets[token] = webGrant{userID: userID, deviceID: deviceID, expires: s.now().Add(webTicketTTL)}
	s.web.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"url": "https://" + s.dshDeviceHost(deviceID) + "/?cg_ticket=" + url.QueryEscape(token),
	})
}

func (s *Server) handleDSHWebStop(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")
	ac, err := s.requireAgent(r.Context(), userID, deviceID)
	if err != nil {
		writeDeviceError(w, err)
		return
	}
	s.web.mu.Lock()
	starting := s.cfg.DSHProxyURL != "" && s.web.simpleDevice == deviceID && s.web.simpleStarts > 0
	s.web.mu.Unlock()
	if starting {
		writeError(w, http.StatusConflict, "dsh_starting", "DSH Web 正在启动，请完成后再停止")
		return
	}
	reqID := uuid.NewString()
	reply := make(chan webStartReply, 1)
	s.web.mu.Lock()
	s.web.pending[reqID] = webStartWait{agent: ac, reply: reply}
	s.web.mu.Unlock()
	defer func() {
		s.web.mu.Lock()
		delete(s.web.pending, reqID)
		s.web.mu.Unlock()
	}()
	env, err := protocol.NewRequest(reqID, protocol.TypeWebStop, "", nil)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	raw, err := protocol.Encode(env)
	if err != nil {
		writeProtoError(w, err)
		return
	}
	if err := ac.TrySendText(raw); err != nil {
		writeError(w, http.StatusServiceUnavailable, "agent_busy", "Agent 忙碌")
		return
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case result := <-reply:
		if result.err != nil {
			writeError(w, http.StatusServiceUnavailable, "dsh_stop_failed", "DSH Web 未能停止")
			return
		}
	case <-timer.C:
		writeError(w, http.StatusGatewayTimeout, "dsh_timeout", "等待 DSH Web 停止超时")
		return
	case <-ac.Done():
		writeError(w, http.StatusServiceUnavailable, "device_offline", "Agent 已断开")
		return
	case <-r.Context().Done():
		return
	}
	s.web.mu.Lock()
	if u := s.web.upstreams[deviceID]; u.agent == ac {
		delete(s.web.upstreams, deviceID)
	}
	if s.web.simpleDevice == deviceID {
		s.web.simpleDevice = ""
	}
	for token, g := range s.web.tickets {
		if g.deviceID == deviceID {
			delete(s.web.tickets, token)
		}
	}
	for token, g := range s.web.sessions {
		if g.deviceID == deviceID {
			delete(s.web.sessions, token)
		}
	}
	var streams []uuid.UUID
	for id, stream := range s.web.streams {
		if stream.agent == ac {
			streams = append(streams, id)
		}
	}
	s.web.mu.Unlock()
	for _, id := range streams {
		s.closeWebStream(id, false)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
}

func (s *Server) acceptWebReply(ac *AgentConn, env *protocol.Envelope) bool {
	s.web.mu.Lock()
	wait := s.web.pending[env.ReplyTo]
	if wait.reply != nil && wait.agent == ac {
		delete(s.web.pending, env.ReplyTo)
	}
	s.web.mu.Unlock()
	if wait.reply == nil || wait.agent != ac {
		return false
	}
	if env.Type == protocol.TypeWebStopped {
		wait.reply <- webStartReply{}
		return true
	}
	if env.Type != protocol.TypeWebStarted {
		wait.reply <- webStartReply{err: errors.New("DSH Web refused start")}
		return true
	}
	p, err := protocol.DecodePayload[protocol.WebStartedPayload](env)
	if err != nil || p.Cookie == "" || len(p.Cookie) > 4096 || strings.ContainsAny(p.Cookie, ";\r\n") {
		wait.reply <- webStartReply{err: errors.New("invalid DSH start response")}
	} else {
		wait.reply <- webStartReply{cookie: p.Cookie}
	}
	return true
}

func (s *Server) serveDSH(w http.ResponseWriter, r *http.Request) {
	deviceID, valid := s.deviceFromDSHHost(r.Host)
	if s.cfg.DSHProxyURL != "" && s.isDSHHost(r.Host) {
		s.web.mu.Lock()
		deviceID = s.web.simpleDevice
		s.web.mu.Unlock()
		valid = true
	}
	if !valid {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	// Browser credentials are host-only; never accept a cross-origin mutation
	// or upgrade even when both subdomains belong to the same site.
	origin := r.Header.Get("Origin")
	if origin != "" && origin != "https://"+s.dshDeviceHost(deviceID) {
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	if origin == "" && (r.Method != http.MethodGet && r.Method != http.MethodHead ||
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")) {
		http.Error(w, "origin required", http.StatusForbidden)
		return
	}
	if token := r.URL.Query().Get("cg_ticket"); token != "" {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.Error(w, "invalid ticket path", http.StatusBadRequest)
			return
		}
		s.web.mu.Lock()
		grant, ok := s.web.tickets[token]
		delete(s.web.tickets, token)
		s.web.mu.Unlock()
		if !ok || grant.deviceID != deviceID || !s.now().Before(grant.expires) {
			http.Error(w, "ticket expired", http.StatusUnauthorized)
			return
		}
		session, err := randomWebToken()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		grant.expires = s.now().Add(webSessionTTL)
		s.web.mu.Lock()
		s.web.sessions[session] = grant
		s.web.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: webCookieName, Value: session, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(webSessionTTL.Seconds())})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	cookie, err := r.Cookie(webCookieName)
	if err != nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	s.web.mu.Lock()
	grant, ok := s.web.sessions[cookie.Value]
	s.web.mu.Unlock()
	if !ok || grant.deviceID != deviceID || !s.now().Before(grant.expires) {
		http.Error(w, "session expired", http.StatusUnauthorized)
		return
	}
	user, err := s.store.UserByID(r.Context(), grant.userID)
	if err != nil || user.Disabled {
		http.Error(w, "account unavailable", http.StatusUnauthorized)
		return
	}
	if _, err := s.requireAgent(r.Context(), grant.userID, deviceID); err != nil {
		http.Error(w, "access revoked", http.StatusForbidden)
		return
	}
	ac, connected := s.reg.Agent(deviceID)
	s.web.mu.Lock()
	upstream, started := s.web.upstreams[deviceID]
	s.web.mu.Unlock()
	if !connected || !started || upstream.agent != ac {
		http.Error(w, "DSH Web is offline; reopen it from CodeGate", http.StatusServiceUnavailable)
		return
	}
	target := &url.URL{Scheme: "http", Host: webLoopback}
	proxy := &httputil.ReverseProxy{
		Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.openWebStream(ctx, ac)
		}},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = s.dshDeviceHost(deviceID)
			pr.Out.Header.Set("Cookie", upstream.cookie)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Proxy-Authorization")
			pr.SetXForwarded()
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie") // DSH's credential stays server-side.
			if location := res.Header.Get("Location"); location != "" {
				res.Header.Set("Location", rewriteDSHLocation(location, s.dshDeviceHost(deviceID)))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "DSH Web tunnel unavailable", http.StatusBadGateway)
		},
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}

// Match the complete authority; a prefix match also accepts localhost:3080.evil.
func rewriteDSHLocation(location, publicHost string) string {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Host != webLoopback && u.Host != "127.0.0.1:3080") {
		return location
	}
	u.Scheme, u.Host = "https", publicHost
	return u.String()
}
