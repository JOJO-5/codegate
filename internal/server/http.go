package server

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/jojo/codegate/internal/protocol"
)

// apiErrorBody 是 REST 的统一错误响应体（§14.2）。
//
//	{"error":{"code":"forbidden","message":"..."}}
//
// 用嵌套结构而不是平铺的 `{"code":...,"message":...}`：平铺的话，
// 成功响应里如果恰好有个叫 `code` 的业务字段就会撞车。
type apiErrorBody struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// listen 创建监听套接字（TLS 时包一层）。
func (s *Server) listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("server: 监听 %s 失败: %w", s.cfg.Listen, err)
	}

	if !s.cfg.TLS.Enabled {
		return ln, nil
	}

	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("server: 加载 TLS 证书失败: %w", err)
	}
	return tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
		// 1.2 是底线。1.0/1.1 已有已知攻击（BEAST/POODLE），
		// 而 Agent 是 Go 写的，天然支持 1.3。
		MinVersion: tls.VersionTLS12,
	}), nil
}

// ---------------------------------------------------------------------------
// 响应辅助
// ---------------------------------------------------------------------------

// writeJSON 写一个 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	// 先序列化再写头：如果 Marshal 失败，我们还有机会返回 500
	// （一旦 WriteHeader 被调用，状态码就定死了）。
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("序列化响应失败", "err", err)
		http.Error(w, `{"error":{"code":"internal","message":"响应序列化失败"}}`,
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// API 响应绝不能被缓存：里面有 token、设备列表等敏感数据。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeError 写一个统一格式的错误响应。
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiErrorBody{Error: apiErrorDetail{Code: code, Message: message}})
}

// writeProtoError 把协议错误映射成 HTTP 响应。
//
// 非 *protocol.CodeError 一律折叠成 500 + `internal`：
// ★ 绝不把原始错误字符串回给客户端 —— 那会泄露 SQL 语句、文件路径等实现细节。
func writeProtoError(w http.ResponseWriter, err error) {
	var ce *protocol.CodeError
	if !errors.As(err, &ce) {
		// 内部错误只记日志，对外只给一句话。
		slog.Warn("REST 处理失败（内部错误）", "err", err)
		writeError(w, http.StatusInternalServerError, string(protocol.CodeInternal), "internal error")
		return
	}

	writeError(w, httpStatusFor(ce.Code), string(ce.Code), ce.Message)
}

// httpStatusFor 把协议错误码映射成 HTTP 状态码。
//
// 这个映射只做一次、集中在一个函数里 —— 分散在各 handler 里的话，
// 迟早会出现「同一个错误码在 A 处返回 403、在 B 处返回 400」。
func httpStatusFor(code protocol.ErrorCode) int {
	switch code {
	case protocol.CodeUnauthenticated:
		return http.StatusUnauthorized // 401：没有有效凭证
	case protocol.CodeForbidden:
		return http.StatusForbidden // 403：有凭证但无权
	case protocol.CodeNotFound, protocol.CodeDeviceNotPaired, protocol.CodeSessionNotFound:
		// ★ 404 而不是 403：「不存在」与「存在但不是你的」必须同码同状态。
		// 分开的话，响应本身就确认了资源存在性 —— 那就是一个枚举预言机。
		return http.StatusNotFound
	case protocol.CodeInvalidMessage, protocol.CodeInvalidPayload,
		protocol.CodeCwdNotAllowed, protocol.CodeCommandNotAllowed:
		return http.StatusBadRequest // 400：请求本身有问题
	case protocol.CodeAlreadyAttached, protocol.CodeSessionLimit,
		protocol.CodeDeviceOffline:
		return http.StatusConflict // 409：当前状态不允许这个操作
	case protocol.CodeFrameTooLarge:
		return http.StatusRequestEntityTooLarge // 413
	case protocol.CodeRateLimited:
		return http.StatusTooManyRequests // 429
	case protocol.CodeVersionUnsupported:
		return http.StatusUpgradeRequired // 426：让客户端去升级
	default:
		return http.StatusInternalServerError
	}
}

// ---------------------------------------------------------------------------
// 请求辅助
// ---------------------------------------------------------------------------

// bearerToken 从 Authorization 头里取出 Bearer token。
//
// 大小写不敏感地匹配 "Bearer"：RFC 7235 规定 scheme 是大小写不敏感的，
// 而不同客户端的实现五花八门。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// clientIP 取真实客户端 IP。
//
// ★ 只有来源在 TrustedProxies 里时才读 X-Forwarded-For。
//
// 无条件信任这个头等于让攻击者随意伪造 IP：限流可以换个头就绕过，
// 审计日志里则全是攻击者写的假 IP —— 比没有审计更糟，因为它会误导调查。
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if len(s.cfg.TrustedProxies) == 0 {
		return host
	}
	if !s.ipTrusted(host) {
		return host
	}

	// X-Forwarded-For 可能是 "client, proxy1, proxy2"。
	// 取**最左边**那个是标准做法：它是原始客户端，
	// 后面的都是中间代理。前提是只有可信代理才能追加，
	// 而我们已经确认了直连来源可信。
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	first := strings.TrimSpace(strings.Split(xff, ",")[0])
	if first == "" {
		return host
	}
	return first
}

// ipTrusted 判断一个 IP 是否在可信代理列表里。
func (s *Server) ipTrusted(ip string) bool {
	parsed := net.ParseIP(ip)
	for _, entry := range s.cfg.TrustedProxies {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// 支持 CIDR 写法（10.0.0.0/8）和单 IP。
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && parsed != nil {
				if cidr.Contains(parsed) {
					return true
				}
			}
			continue
		}
		if entry == ip {
			return true
		}
	}
	return false
}

// sameOrigin 判断请求的 Origin 是否允许。
func (s *Server) originOK(r *http.Request) bool {
	return s.cfg.OriginAllowed(r.Header.Get("Origin"))
}
