package server

import (
	"net/http"

	"github.com/jojo/codegate/internal/server/webui"
)

// buildRouter 组装路由表（架构文档 §14.2 / §14.3）。
//
// 用标准库的 ServeMux（Go 1.22+ 支持 `METHOD /path/{id}` 与路径通配符）。
// 路由需求就是「方法 + 路径 + 一个 id 段」，为它引一个第三方 router
// 只会多一份依赖，外加一套需要额外理解的匹配语义（优先级、尾部斜杠、
// 大小写敏感……）。标准库的语义是所有人都已经知道的那套。
//
// # 中间件的顺序是有讲究的
//
//	withRecover( withRequestLog( mux ) )
//
// recover 在最外层：它必须能兜住日志中间件自己可能产生的 panic。
// 日志在路由外层、认证内层：这样记录到的 status 已经是业务 handler
// 真正写出来的状态码，而不是被中间件改写过的。
//
// # ★ 认证刻意不挂在全局链上
//
// 全局挂一个 requireAuth，会逼着每个 handler 再去判断
// 「我这个路径要不要豁免认证」—— 那种写法迟早会漏掉一个。
// 改成按路由组显式挂：探针端点不挂，业务端点挂。
// 加新路由时必须主动选一次，而不是被动继承一个默认值。
func (s *Server) buildRouter() http.Handler {
	mux := http.NewServeMux()

	// ---- 探针与版本（无认证）----
	//
	// 探针不能要认证：编排系统的健康检查不会带 token，
	// 而「探针 401」在编排系统眼里和「服务没起来」是一回事。
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)

	// ---- 需要 Bearer access token 的端点 ----
	//
	// authed 只是 requireAuth 的简写，让下面的路由表读起来
	// 像一张「哪些要认证」的清单，而不是一堆 http.HandlerFunc 嵌套。
	authed := func(h http.HandlerFunc) http.Handler {
		return s.requireAuth(h)
	}

	mux.Handle("POST /api/v1/ws-ticket", authed(s.handleWSTicket))

	// ---- 认证（§14.2）----
	//
	// ★ register / login / refresh 三个**不能**挂 authed。
	//
	// 它们本身就是「换取凭证」的入口：register 时还没有账号，
	// login 时只有密码，refresh 时 access token 恰好已经过期 ——
	// 挂上 requireAuth 会让这三个端点永远返回 401，形成死锁。
	// 它们的防滥用靠限流（loginLimit / loginIPLimit / registerLimit），
	// 不靠 token。
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/refresh", s.handleRefresh)

	// 这两个**要**认证：
	//   - logout 必须知道「注销谁的」—— 匿名注销没有意义，且会变成
	//     一个可以被用来试探 token 是否有效的探针。
	//   - change-password 同理，而且它必须能拿到当前用户 ID 去吊销
	//     该用户的全部 refresh token。
	mux.Handle("POST /api/v1/auth/logout", authed(s.handleLogout))
	mux.Handle("POST /api/v1/auth/password", authed(s.handleChangePassword))
	mux.Handle("GET /api/v1/me", authed(s.handleMe))

	// ---- 设备（§14.2）----
	//
	// ★ 路径段的写法是有讲究的：`/devices/pair` 必须写在
	// `/devices/{id}` 之前。
	//
	// Go 1.22+ 的 ServeMux 用「更具体的模式优先」来消歧 —— 字面量段
	// 比通配符段更具体，所以 `pair` 不会被 `{id}` 吃掉。但把这个
	// 依赖显式写在注释里，比让下一个人去翻标准库文档要便宜。
	mux.Handle("POST /api/v1/devices/pair", authed(s.handleDevicePair))
	mux.Handle("POST /api/v1/devices/pair/confirm", authed(s.handleDevicePairConfirm))

	mux.Handle("GET /api/v1/devices", authed(s.handleDeviceList))
	mux.Handle("GET /api/v1/devices/{id}", authed(s.handleDeviceGet))
	mux.Handle("PATCH /api/v1/devices/{id}", authed(s.handleDeviceRename))
	mux.Handle("DELETE /api/v1/devices/{id}", authed(s.handleDeviceDelete))
	mux.Handle("GET /api/v1/devices/{id}/sessions", authed(s.handleDeviceSessions))

	mux.Handle("GET /api/v1/audit", authed(s.handleAuditLogs))

	// ---- WebSocket（§14.3）----
	//
	// ★ 这两个端点**不做 Bearer 认证**，各自有独立的凭证机制：
	//   - /ws/agent：Ed25519 挑战-应答，握手在 WS 内部完成
	//     （HTTP 层没法签 nonce，因为 nonce 是服务端在连上之后才发的）
	//   - /ws/client：一次性 ws-ticket，在 upgrade 之前消费
	//
	// 但两者都挂 withOriginCheck：WS 不受同源策略保护，任何网页都能
	// 对这两个端点发起跨站连接。Origin 校验在这里是廉价且必要的。
	mux.Handle("GET /api/v1/ws/agent",
		s.withOriginCheck(http.HandlerFunc(s.handleWSAgent)))
	mux.Handle("GET /api/v1/ws/client",
		s.withOriginCheck(http.HandlerFunc(s.handleWSClient)))

	// ---- 前端（SPA）----
	//
	// ★ 注册在**最后**，模式是最不具体的 `/`。
	//
	// Go 1.22+ 的 ServeMux 按「更具体的模式优先」消歧，所以上面所有
	// `/api/v1/...`、`/healthz`、`/readyz` 都会先命中；只有真正没人
	// 认领的路径才会落到 SPA handler，由它返回 index.html 让前端路由接手。
	//
	// 顺序在这里其实是无关的（消歧靠具体性而非注册次序），但把 catch-all
	// 放在最后读起来最清楚 —— 它确实就是兜底。
	//
	// webui.Handler 内部还会再挡一次 `/api/` 前缀：那些是「API 路径写错了」
	// 而不是「前端路由」，必须返回 404 JSON 而不是 HTML，否则前端会在
	// 很远的地方报一个"响应不是 JSON"。
	mux.Handle("/", webui.Handler())

	return s.withRecover(s.withRequestLog(mux))
}
