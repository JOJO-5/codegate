package server

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/jojo/codegate/internal/auth"
	"github.com/jojo/codegate/internal/logging"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

// contextKey 是本包私有的 context 键类型。
//
// ★ 用私有类型而不是裸字符串：context 的键是全局命名空间，
// 两个包都用 "userID" 就会互相覆盖，而且编译器不会报错 ——
// 这类 bug 只在运行时以「用户莫名其妙变成别人」的形式出现。
type contextKey int

const ctxKeyUserID contextKey = iota

// userIDFromContext 取当前认证用户的 ID。
func userIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyUserID).(string)
	return v, ok && v != ""
}

// withContextUser 把 userID 放进 context。测试直接调 handler 时用得上。
func withContextUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, userID)
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

// withRecover 捕获 panic，避免一个 handler 的 bug 带走整个进程。
//
// 没有它的话，任何一个 nil 解引用都会让所有用户的连接一起断掉 ——
// 这是「一个请求的错误」升级成「全站故障」的最短路径。
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// ★ 打完整堆栈。panic 的堆栈是唯一能定位问题的线索，
			// 只记一句 "panic: nil pointer" 等于没记。
			s.log.Error("HTTP handler panic",
				"panic", rec,
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)
			// http.ErrAbortHandler 是 net/http 用来「静默中止连接」的哨兵，
			// 不该当成 bug 处理，也不该再写响应。
			if errors.Is(anyErr(rec), http.ErrAbortHandler) {
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

// anyErr 把 recover 出来的值转成 error（可能不是 error 类型）。
func anyErr(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return nil
}

// statusRecorder 记录响应状态码与字节数，供访问日志使用。
//
// ★ 必须显式转发 Hijack 与 Flush。
//
// 包一层 ResponseWriter 是很容易踩的坑：WebSocket 升级依赖
// `http.Hijacker` 接口，而包装类型默认**不实现**它。
// 结果是中间件一加，所有 WS 升级都失败，报的错却是
// "response does not implement http.Hijacker" —— 很难联想到是日志中间件干的。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Hijack 转发给底层，保住 WebSocket 升级能力。
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("server: 底层 ResponseWriter 不支持 Hijack（WebSocket 无法升级）")
	}
	return h.Hijack()
}

// Flush 转发给底层（SSE / 流式响应用）。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withRequestLog 记录访问日志。
//
// 用 Debug 级别记录全部请求，用 Warn 记录慢请求与 5xx ——
// 生产环境开 Info 会把日志淹没在健康检查里。
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		elapsed := time.Since(start)

		// ★ 用 RedactURL 而不是 r.URL.String()：
		// WS 票据在 query 里（?ticket=xxx），日志往往长期保留，
		// 泄露后在有效期内仍可被用来抢占连接。
		path := logging.RedactURL(r.URL)

		switch {
		case rec.status >= 500:
			s.log.Warn("请求失败",
				"method", r.Method, "path", path,
				"status", rec.status, "ms", elapsed.Milliseconds(),
				"ip", s.clientIP(r))
		case elapsed > 2*time.Second:
			s.log.Warn("慢请求",
				"method", r.Method, "path", path,
				"status", rec.status, "ms", elapsed.Milliseconds())
		default:
			s.log.Debug("请求",
				"method", r.Method, "path", path,
				"status", rec.status, "bytes", rec.bytes,
				"ms", elapsed.Milliseconds())
		}
	})
}

// requireAuth 校验 Bearer access token，把 userID 注入 context。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.authenticate(r)
		if err != nil {
			writeProtoError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withContextUser(r.Context(), user.ID)))
	})
}

// authenticate 校验凭证并取出用户。
//
// ★ 每次请求都查库确认用户仍然有效，而不是只信 token 里的 sub。
//
// 这正是 §10.1「token 里不放权限声明」的代价与收益：
// 代价是一次主键查询；收益是**禁用用户立刻生效**，
// 而不是等最长 15 分钟的 token 过期窗口。
// 在这个窗口里，一个已被停用的账号仍然能连上你的机器 —— 这个风险不值得省一次查询。
func (s *Server) authenticate(r *http.Request) (*storage.User, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errUnauthenticated()
	}

	claims, err := s.tokens.Verify(token)
	if err != nil {
		// 过期与无效要给不同的提示：前端据此决定「静默刷新」还是「回登录页」。
		if errors.Is(err, auth.ErrTokenExpired) {
			return nil, protocol.NewError(protocol.CodeUnauthenticated, "access token 已过期")
		}
		return nil, protocol.NewError(protocol.CodeUnauthenticated, "access token 无效")
	}

	u, err := s.store.UserByID(r.Context(), claims.Subject)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// token 签名有效但用户已被删除。
			return nil, protocol.NewError(protocol.CodeUnauthenticated, "账号不存在")
		}
		return nil, err
	}
	if u.Disabled {
		return nil, protocol.NewError(protocol.CodeUnauthenticated, "账号已被禁用")
	}
	return u, nil
}

// withOriginCheck 校验 Origin 头（WS 端点用）。
//
// WebSocket **不受同源策略保护**：浏览器允许任意站点发起 WS 连接，
// 且会自动带上 cookie。没有 Origin 校验的话，用户访问一个恶意页面
// 就能被它拿 cookie 连上我们的服务（CSRF over WebSocket）。
func (s *Server) withOriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.originOK(r) {
			s.log.Warn("拒绝来源不在白名单的 WebSocket 连接",
				"origin", r.Header.Get("Origin"), "ip", s.clientIP(r))
			writeError(w, http.StatusForbidden, "forbidden", "origin 不在白名单内")
			return
		}
		next.ServeHTTP(w, r)
	})
}
