// Package server 是 CodeGate 服务端：HTTP API + 两条 WebSocket 通路 + 中继。
//
// # 分层
//
//	handler_*   HTTP/WS 入口，只做「解析请求 → 调用业务 → 写响应」
//	authz       授权唯一入口（防 IDOR 的关键）
//	relay       唯一跨连接通路
//	registry    连接注册表
//
// 业务逻辑在 internal/auth、internal/device、internal/storage 里，
// 本包负责把它们串起来，并处理 HTTP/WS 特有的关注点（状态码、中间件、连接生命周期）。
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jojo/codegate/internal/auth"
	"github.com/jojo/codegate/internal/config"
	"github.com/jojo/codegate/internal/device"
	"github.com/jojo/codegate/internal/server/webui"
	"github.com/jojo/codegate/internal/storage"
)

// Server 是服务的装配点。所有依赖在这里注入，没有全局状态。
type Server struct {
	cfg   *config.Config
	log   *slog.Logger
	store storage.Store

	reg   *Registry
	relay *Relay

	tokens  *auth.TokenIssuer
	devices *device.Service

	// tickets 是浏览器 WS 连接用的一次性票据。
	tickets *ticketStore
	installTickets *installTicketStore

	// pairs 保存「已发配对码、等浏览器确认」的 Agent 连接。
	//
	// ★ 刻意**不放进 Registry**：那些连接尚未通过认证，
	// 一旦进注册表就可能被 Relay 当成可路由的目标。
	pairs *pairRegistry

	// pending 保存「客户端发出、等 Agent 回」的请求。
	//
	// 中继本身是无状态的：request_id 由客户端生成，Agent 只在
	// reply_to 里原样回填 —— 所以「这条响应该发给谁」只有 Server 知道。
	pending *pendingRegistry

	// 限流器按攻击面分开，**刻意不共用**。
	//
	// 登录爆破、注册洪水、配对码猜解是三种完全不同的攻击，
	// 合理的阈值差几个数量级。共用一个限流器只能取一个折中值，
	// 结果是要么某个面被限制得太松，要么正常用户被误伤。
	//
	// loginLimit 按 email（防「盯着一个账号爆破」）；
	// loginIPLimit 按 IP（防「换邮箱遍历」）—— 两者必须**同时**通过，
	// 理由见 handleLogin 里的注释。
	loginLimit     *RateLimiter  // 按 email
	loginIPLimit   *RateLimiter  // 按 IP
	registerLimit  *RateLimiter  // 按 IP
	wsLimit        *RateLimiter  // 按 IP，挡 WS 建连洪水
	pairBeginLimit *RateLimiter  // 按 IP，挡配对码洪水
	pairLimit      *FailureGuard // 按 user + IP，连续失败锁定

	now func() time.Time

	httpSrv   *http.Server
	startedAt time.Time

	// cleanupStop 用于停止后台清理协程。
	cleanupStop chan struct{}
	cleanupOnce sync.Once
}

// Options 是构造 Server 的入参。
type Options struct {
	Config *config.Config
	Logger *slog.Logger
	Store  storage.Store
	// Now 可注入，便于测试时间相关行为（限流、票据过期）。
	Now func() time.Time
}

// New 装配 Server。不做任何 I/O（不监听端口、不连数据库）——
// 让「构造」和「启动」分开，测试里可以只构造不启动。
func New(o Options) (*Server, error) {
	if o.Config == nil {
		return nil, errors.New("server: 缺少配置")
	}
	if o.Store == nil {
		return nil, errors.New("server: 缺少存储")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}

	tokens, err := auth.NewTokenIssuer(o.Config.JWTSecret, o.Config.AccessTokenTTL)
	if err != nil {
		return nil, err
	}

	reg := NewRegistry(o.Config.SendQueueSize)

	s := &Server{
		cfg:     o.Config,
		log:     log,
		store:   o.Store,
		reg:     reg,
		relay:   NewRelay(reg, log),
		tokens:  tokens,
		tickets: newTicketStore(),
		installTickets: newInstallTicketStore(),
		pairs:   newPairRegistry(),
		pending: newPendingRegistry(),

		// 阈值的选择依据：
		//   login 5/分钟（按 email）—— 正常人手误不会超过 5 次，
		//                            而爆破的尝试速度被压到 5/分钟 → 一年也试不完 2^40。
		//   login 20/分钟（按 IP） —— 比 email 松：NAT 后面可能有一整间办公室，
		//                            阈值太紧会把同事一起误伤。
		//   register 10/分钟（按 IP）—— 开放注册的实例要防脚本批量刷账号。
		//   ws 60/分钟（按 IP）    —— 挡建连洪水；正常用户重连不会到这个量级。
		loginLimit:     NewRateLimiter(5.0/60.0, 5, now),
		loginIPLimit:   NewRateLimiter(20.0/60.0, 20, now),
		registerLimit:  NewRateLimiter(10.0/60.0, 10, now),
		wsLimit:        NewRateLimiter(60.0/60.0, 30, now),
		pairBeginLimit: NewRateLimiter(6.0/60.0, 6, now),
		pairLimit:      NewFailureGuard(10, 30*time.Minute, now),

		now:         now,
		startedAt:   now(),
		cleanupStop: make(chan struct{}),
	}
	// device 服务需要实时在线状态，注册表正好实现 OnlineChecker。
	s.devices = device.NewService(o.Store, reg, log)

	return s, nil
}

// Config 返回配置（只读用途）。
func (s *Server) Config() *config.Config { return s.cfg }

// Logger 返回 logger。
func (s *Server) Logger() *slog.Logger { return s.log }

// Registry 返回连接注册表（诊断接口用）。
func (s *Server) Registry() *Registry { return s.reg }

// Relay 返回转发器（诊断接口用）。
func (s *Server) Relay() *Relay { return s.relay }

// Handler 构造完整的 HTTP handler（含路由与中间件）。
//
// 返回 http.Handler 而不是直接启动 http.Server，是为了让测试能用
// httptest.NewServer 挂上去，不必真的占用端口。
func (s *Server) Handler() http.Handler {
	return s.buildRouter()
}

// Serve 阻塞地提供服务，直到 ctx 取消或出错。
func (s *Server) Serve(ctx context.Context) error {
	ln, err := s.listen()
	if err != nil {
		return err
	}

	s.httpSrv = &http.Server{
		Handler: s.Handler(),
		// 不设 ReadTimeout/WriteTimeout：它们会掐断长连接。
		// WebSocket 的活性由应用层心跳与 PongTimeout 保证。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	s.startCleanupLoop()

	// 协议区分 http/https 由 listener 决定（TLS 时用 tls.NewListener）。
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("服务已启动",
			"addr", ln.Addr().String(),
			"tls", s.cfg.TLS.Enabled,
			"base_url", s.cfg.BaseURL,
		)

		// 内嵌的不是完整产物时说清楚 —— 并且说清楚**是哪种不完整**。
		//
		// 不这么做的话，症状是「打开网页只有一句话」，而用户会去猜是
		// 反向代理、是端口、还是浏览器缓存 —— 唯独猜不到"这个二进制
		// 编译时前端还没构建"。一条警告就能省掉整段排查。
		//
		// 带上 webui.DistState()：`distIncomplete`（index.html 是真实产物
		// 但资源缺失）和 `distPlaceholder`（全新 clone）的处置方式不同，
		// 只说"未构建"会让前者被误判成后者、白白重跑一次 make web。
		if !webui.Built() {
			s.log.Warn("内嵌的不是完整的前端产物，Web 界面不可用；API 不受影响。",
				"状态", webui.DistState(),
				"修复", "在仓库根目录执行 `make web` 后重新编译 Server")
		}
		if s.cfg.TLS.Enabled {
			errCh <- s.httpSrv.ServeTLS(ln, s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
			return
		}
		errCh <- s.httpSrv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		return s.Shutdown()
	}
}

// Shutdown 优雅关闭：先停 HTTP（不再接新连接），再关所有 WS 连接。
func (s *Server) Shutdown() error {
	s.cleanupOnce.Do(func() { close(s.cleanupStop) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if s.httpSrv != nil {
		if err := s.httpSrv.Shutdown(ctx); err != nil {
			s.log.Warn("HTTP 优雅关闭超时，强制关闭", "err", err)
		}
	}

	// ★ 主动关闭所有 WS 连接。
	//
	// http.Server.Shutdown **不会**关闭 hijack 掉的连接（WebSocket 就是），
	// 所以不显式关的话，进程会一直卡在这里等它们自然断开 ——
	// 而客户端在等我们发消息，双方互等。
	s.closeAllConns()
	return nil
}

// closeAllConns 关闭全部活跃连接，给对端一个可读的关闭码。
func (s *Server) closeAllConns() {
	s.reg.mu.RLock()
	agents := make([]*AgentConn, 0, len(s.reg.agents))
	for _, c := range s.reg.agents {
		agents = append(agents, c)
	}
	clients := make([]*ClientConn, 0, len(s.reg.clients))
	for _, c := range s.reg.clients {
		clients = append(clients, c)
	}
	s.reg.mu.RUnlock()

	for _, c := range agents {
		// 1001 = going away，语义正好是「服务端要关了」。
		// Agent 收到后应当退避重连（不是致命关闭码）。
		closeWithCode(c.conn, CloseGoingAway, "server_shutdown")
	}
	for _, c := range clients {
		closeWithCode(c.conn, CloseGoingAway, "server_shutdown")
	}
}

// kickAgent 踢掉某设备当前的 Agent 连接，返回是否真的有连接被踢。
//
// # 为什么「先发关闭帧、再 Close」
//
// 只 Close 不发送：对端看到的是 TCP 直接断开（1006 异常关闭），
// 于是把它归类成「网络抖动」并立刻重连 —— 而重连又会被拒，
// 形成「连上就被踢」的循环，日志里只有一堆无意义的断连记录。
//
// 只发关闭帧不 Close：连接会一直挂着，直到对端读到关闭帧后主动断开。
// 正常情况下这没问题，但如果对端进程卡死了，这条连接会一直占着
// 注册表里的位置，让「设备已删除」这个事实永远不生效。
//
// 先发帧再关连接：WriteControl 是**同步写**到 socket 的，
// 所以关闭帧的字节已经进了内核发送缓冲区，对端能读到；
// 随后 Close 只是不让它再读下去。两者缺一不可。
func (s *Server) kickAgent(deviceID string, code int, reason string) bool {
	agent, ok := s.reg.Agent(deviceID)
	if !ok {
		return false
	}
	closeWithCode(agent.conn, code, reason)
	agent.Close()
	s.log.Info("已踢掉 Agent 连接", "device_id", deviceID, "code", code, "reason", reason)
	return true
}

// startCleanupLoop 周期清理过期数据。
//
// 清理是**写操作**，不能塞进请求路径 —— 那会让正常请求去抢写锁（R9）。
func (s *Server) startCleanupLoop() {
	go func() {
		// 启动时先跑一次：进程重启后，上次留下的过期记录需要及时清掉。
		s.runCleanup()

		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-s.cleanupStop:
				return
			case <-ticker.C:
				s.runCleanup()
			}
		}
	}()
}

func (s *Server) runCleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := s.now()
	if n, err := s.store.DeleteExpiredRefreshTokens(ctx, now); err != nil {
		s.log.Warn("清理过期刷新令牌失败", "err", err)
	} else if n > 0 {
		s.log.Info("已清理过期刷新令牌", "count", n)
	}

	if n, err := s.store.DeleteExpiredPairingCodes(ctx, now); err != nil {
		s.log.Warn("清理过期配对码失败", "err", err)
	} else if n > 0 {
		s.log.Info("已清理过期配对码", "count", n)
	}

	s.tickets.sweep(now)
	s.installTickets.sweep(now)
	s.pairs.sweep(now)
	s.pending.sweep(now)
}

// Uptime 返回服务运行时长。
func (s *Server) Uptime() time.Duration { return s.now().Sub(s.startedAt) }
