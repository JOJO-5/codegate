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
	agent    *AgentConn
	cookie   string
	mobileUI string
}
type webStartReply struct {
	cookie string
	err    error
}
type webStartWait struct {
	agent    *AgentConn
	reply    chan webStartReply
	expected protocol.Type
}

type webOperation struct {
	agent  *AgentConn
	kind   protocol.Type
	done   chan struct{}
	result webStartReply
}
type toolWait struct {
	expected protocol.Type
	agent    *AgentConn
	reply    chan error
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
	operations   map[string]*webOperation
	uncertain    map[string]*AgentConn
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
		streams:    make(map[uuid.UUID]*proxyStream),
		operations: make(map[string]*webOperation), uncertain: make(map[string]*AgentConn),
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
	op, err := s.beginWebOperation(ac, deviceID, protocol.TypeWebStart)
	if err != nil {
		writeDSHStartError(w, err)
		return
	}
	select {
	case <-op.done:
		if op.result.err != nil {
			writeDSHStartError(w, op.result.err)
			return
		}
	case <-r.Context().Done():
		// The bounded operation keeps the Agent receipt even after this browser leaves.
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
	op, err := s.beginWebOperation(ac, deviceID, protocol.TypeWebStop)
	if err != nil {
		writeDSHStartError(w, err)
		return
	}
	select {
	case <-op.done:
		if op.result.err != nil {
			writeDSHStartError(w, op.result.err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
	case <-r.Context().Done():
	}
}

// One operation per device. Concurrent callers share its receipt; cancellation
// of an HTTP waiter never loses ownership of a possibly running process.
func (s *Server) beginWebOperation(ac *AgentConn, deviceID string, kind protocol.Type) (*webOperation, error) {
	s.web.mu.Lock()
	defer s.web.mu.Unlock()
	if op := s.web.operations[deviceID]; op != nil {
		if op.agent == ac && op.kind == kind {
			return op, nil
		}
		return nil, protocol.NewError("dsh_busy", "DSH Web 正在核对启动或停止结果，请等待后重新连接。")
	}
	if kind == protocol.TypeWebStart && !ac.Caps().DSHWebLifecycle {
		return nil, protocol.NewError("dsh_upgrade_required", "安全重连需要新版 Agent。请先让当前会话空闲并更新 Agent；不会为了打开 DSH 自动重启旧实例。")
	}
	if kind == protocol.TypeWebStart && s.cfg.DSHProxyURL != "" {
		if old := s.web.upstreams[s.web.simpleDevice]; old.agent != nil {
			select {
			case <-old.agent.Done():
				delete(s.web.upstreams, s.web.simpleDevice)
				s.web.simpleDevice = ""
			default:
			}
		}
		if s.web.simpleDevice != "" && s.web.simpleDevice != deviceID {
			return nil, protocol.NewError("dsh_in_use", "简化转发正在服务另一台设备，请先停止该设备的 DSH Web。")
		}
		s.web.simpleDevice = deviceID
	}
	op := &webOperation{agent: ac, kind: kind, done: make(chan struct{})}
	s.web.operations[deviceID] = op
	go s.runWebOperation(deviceID, op)
	return op, nil
}

func (s *Server) runWebOperation(deviceID string, op *webOperation) {
	reqID := uuid.NewString()
	reply := make(chan webStartReply, 1)
	expected := protocol.TypeWebStarted
	if op.kind == protocol.TypeWebStop {
		expected = protocol.TypeWebStopped
	}
	s.web.mu.Lock()
	s.web.pending[reqID] = webStartWait{agent: op.agent, reply: reply, expected: expected}
	s.web.mu.Unlock()
	payload := any(nil)
	if op.kind == protocol.TypeWebStart {
		payload = protocol.WebStartPayload{Host: s.dshDeviceHost(deviceID)}
	}
	env, err := protocol.NewRequest(reqID, op.kind, "", payload)
	var raw []byte
	if err == nil {
		raw, err = protocol.Encode(env)
	}
	sent := false
	if err == nil {
		err = op.agent.TrySendText(raw)
		sent = err == nil
	}
	result := webStartReply{err: err}
	if err == nil {
		timeout := 45 * time.Second
		if op.kind == protocol.TypeWebStop {
			timeout = 10 * time.Second
		}
		timer := time.NewTimer(timeout)
		select {
		case result = <-reply:
			if op.kind == protocol.TypeWebStart && result.err == nil && result.cookie == "" {
				result.err = protocol.DSHStartError(protocol.CodeInternal)
			}
		case <-op.agent.Done():
			result.err = protocol.NewError(protocol.CodeDeviceOffline, "Agent 已断开，DSH 状态需要重新核对。")
		case <-timer.C:
			result.err = protocol.NewError("dsh_timeout", "DSH 操作结果尚未确认，请重新连接核对；不会自动重启。")
		}
		timer.Stop()
	}
	current, online := s.reg.Agent(deviceID)
	if !online || current != op.agent {
		result = webStartReply{err: protocol.NewError(protocol.CodeDeviceOffline, "Agent 已断开，请重新连接核对。")}
	}
	var streams []uuid.UUID
	s.web.mu.Lock()
	delete(s.web.pending, reqID)
	delete(s.web.operations, deviceID)
	if result.err == nil {
		delete(s.web.uncertain, deviceID)
		if op.kind == protocol.TypeWebStart {
			compat := "unknown"
			if previous := s.web.upstreams[deviceID]; previous.agent == op.agent && previous.cookie == result.cookie {
				compat = previous.mobileUI
			}
			s.web.upstreams[deviceID] = webUpstream{agent: op.agent, cookie: result.cookie, mobileUI: compat}
		} else {
			delete(s.web.upstreams, deviceID)
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
			for id, stream := range s.web.streams {
				if stream.agent == op.agent {
					streams = append(streams, id)
				}
			}
		}
	} else if sent && (protocol.ErrorCodeOf(result.err) == "dsh_timeout" || protocol.ErrorCodeOf(result.err) == protocol.CodeDeviceOffline || protocol.ErrorCodeOf(result.err) == protocol.CodeDSHCleanupFailed || protocol.ErrorCodeOf(result.err) == protocol.CodeInternal) {
		s.web.uncertain[deviceID] = op.agent
	} else {
		delete(s.web.uncertain, deviceID)
	}
	// Do not release a simple-mode reservation after an ambiguous dispatch: the
	// other device must not silently replace a potentially running instance.
	if result.err != nil && s.web.uncertain[deviceID] == nil && s.web.simpleDevice == deviceID && s.web.upstreams[deviceID].agent == nil {
		s.web.simpleDevice = ""
	}
	op.result = result
	s.web.mu.Unlock()
	for _, id := range streams {
		s.closeWebStream(id, false)
	}
	close(op.done)
}

func writeDSHStartError(w http.ResponseWriter, err error) {
	code := protocol.ErrorCodeOf(err)
	status := http.StatusServiceUnavailable
	switch code {
	case protocol.CodeDSHPortInUse, protocol.CodeDSHRestartRequired, "dsh_busy", "dsh_in_use", "dsh_upgrade_required":
		status = http.StatusConflict
	case "dsh_timeout":
		status = http.StatusGatewayTimeout
	}
	var cause *protocol.CodeError
	if code == protocol.CodeInternal {
		writeError(w, status, "dsh_unavailable", protocol.DSHStartError(code).Message)
		return
	}
	if errors.As(err, &cause) {
		writeError(w, status, string(cause.Code), cause.Message)
		return
	}
	writeError(w, status, "dsh_unavailable", protocol.DSHStartError(protocol.CodeInternal).Message)
}

func (s *Server) handleDSHWebStatus(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	deviceID := r.PathValue("id")
	if _, err := s.authorizeDevice(r.Context(), userID, deviceID); err != nil {
		writeDeviceError(w, err)
		return
	}
	ac, online := s.reg.Agent(deviceID)
	state := "not_open"
	compat := "unknown"
	s.web.mu.Lock()
	if upstream := s.web.upstreams[deviceID]; upstream.agent == ac && ac != nil {
		compat = upstream.mobileUI
	}
	if op := s.web.operations[deviceID]; op != nil {
		if op.kind == protocol.TypeWebStart {
			state = "starting"
		} else {
			state = "stopping"
		}
	} else if s.web.uncertain[deviceID] == ac && ac != nil {
		state = "unknown"
	} else if upstream := s.web.upstreams[deviceID]; upstream.agent == ac && ac != nil {
		state = "forwarding"
	}
	s.web.mu.Unlock()
	if !online {
		state = "offline"
	}
	writeJSON(w, http.StatusOK, map[string]any{"online": online, "state": state, "mobile_ui": compat})
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
	if env.Type != protocol.TypeError && wait.expected != "" && env.Type != wait.expected {
		wait.reply <- webStartReply{err: protocol.DSHStartError(protocol.CodeInternal)}
		return true
	}
	if env.Type == protocol.TypeWebStopped {
		wait.reply <- webStartReply{}
		return true
	}
	if env.Type != protocol.TypeWebStarted {
		p, _ := protocol.DecodePayload[protocol.ErrorPayload](env)
		wait.reply <- webStartReply{err: protocol.DSHStartError(p.Code)}
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
			s.dshFailure(w, r, http.StatusUnauthorized, deviceID, "打开链接已过期或已使用，请从 CodeGate 获取新链接。")
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
		s.dshFailure(w, r, http.StatusUnauthorized, deviceID, "此浏览器尚未获得 DSH Web 访问授权，或授权已过期。")
		return
	}
	s.web.mu.Lock()
	grant, ok := s.web.sessions[cookie.Value]
	s.web.mu.Unlock()
	if !ok || grant.deviceID != deviceID || !s.now().Before(grant.expires) {
		s.dshFailure(w, r, http.StatusUnauthorized, deviceID, "DSH Web 访问授权已过期，请返回 CodeGate 重新核对。")
		return
	}
	user, err := s.store.UserByID(r.Context(), grant.userID)
	if err != nil || user.Disabled {
		http.Error(w, "account unavailable", http.StatusUnauthorized)
		return
	}
	if _, err := s.requireAgent(r.Context(), grant.userID, deviceID); err != nil {
		if protocol.ErrorCodeOf(err) == protocol.CodeDeviceOffline {
			s.dshFailure(w, r, http.StatusServiceUnavailable, deviceID, "设备离线，请确认电脑与 Agent 在线后返回 CodeGate 重新连接。")
			return
		}
		http.Error(w, "access revoked", http.StatusForbidden)
		return
	}
	ac, connected := s.reg.Agent(deviceID)
	if s.serveDSHMobileAsset(w, r) {
		return
	}
	s.web.mu.Lock()
	upstream, started := s.web.upstreams[deviceID]
	s.web.mu.Unlock()
	if !connected || !started || upstream.agent != ac {
		s.dshFailure(w, r, http.StatusServiceUnavailable, deviceID, "设备已断开或转发状态已失效，请返回 CodeGate 核对。")
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
			if pr.In.Method == http.MethodGet && pr.In.URL.Path == "/" && !s.cfg.DisableDSHMobileUI {
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie") // DSH's credential stays server-side.
			if location := res.Header.Get("Location"); location != "" {
				res.Header.Set("Location", rewriteDSHLocation(location, s.dshDeviceHost(deviceID)))
			}
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				if err := s.adaptDSHMobile(res); err != nil {
					return err
				}
				s.web.mu.Lock()
				if current := s.web.upstreams[deviceID]; current.agent == ac && current.cookie == upstream.cookie {
					current.mobileUI = res.Header.Get("X-CodeGate-DSH-Mobile")
					s.web.upstreams[deviceID] = current
				}
				s.web.mu.Unlock()
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, request *http.Request, _ error) {
			s.dshFailure(w, request, http.StatusBadGateway, deviceID, "DSH Web 转发连接不可用，请返回 CodeGate 重新核对。")
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
