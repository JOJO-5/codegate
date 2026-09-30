package server

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jojo/codegate/internal/protocol"
)

// WebSocket 层的公共参数。
//
// 这里的时间值不是随手定的，它们和 Agent 侧的常数必须能对上
// （internal/agent/client.go 的 readTimeout = 90s）：
//
//	服务端 Ping 间隔 30s  <  Agent 读超时 90s   → Agent 永远能收到 Ping
//	服务端 Pong 等待 90s  >  服务端 Ping 间隔   → 允许连续丢两次 Ping
//
// 两组数字反过来会得到「连接每 30 秒断一次」这种极难排查的现象：
// 双方都觉得自己在正常工作，都在等对方先说话。
const (
	// wsPingPeriod 是服务端主动发 Ping 的间隔。
	wsPingPeriod = 30 * time.Second

	// wsPongWait 是读不到**任何**消息（含 Pong）就判定连接死亡的时间。
	//
	// 90s 而不是「Ping 间隔 × 2」：即使应用层心跳全丢，只要有 Pong 在，
	// 链路就是活的。给 90s 可以容忍连续两次 Ping 丢失，
	// 而这在移动网络下是常态。
	wsPongWait = 90 * time.Second

	// wsWriteWait 是单条消息写完的期限。
	//
	// 必须设：不设的话，一个不读数据的对端会让 WriteMessage 永久阻塞，
	// 而 writePump 是每条连接唯一的写者 —— 它一卡，整条连接的
	// 发送队列（含关闭通知）就全部堵死。
	wsWriteWait = 10 * time.Second

	// wsHandshakeTimeout 是「连上到认证完成」的期限。
	//
	// 这个窗口必须有上限：未认证的连接不占注册表，但仍然占着一个
	// goroutine + 一个 socket。攻击者可以开一万条连接、全部只发 hello
	// 不发 auth，就得到一个廉价的 goroutine 泄漏。
	wsHandshakeTimeout = 30 * time.Second

	// wsReadBufferSize / wsWriteBufferSize 是 gorilla 内部缓冲区大小。
	//
	// 4 KB 是权衡：太小会让大帧被拆成多次系统调用，太大则每条连接
	// 多占 2 × N 字节 —— 1000 条连接时 64 KB 的缓冲区就是 128 MB。
	wsReadBufferSize  = 4096
	wsWriteBufferSize = 4096

	// wsMaxReadSize 是单条入站消息的大小上限。
	//
	// ★ 与 protocol.MaxControlMessageSize 一致（1 MB）。
	// 这个限制必须在**读之前**生效 —— gorilla 的 SetReadLimit 会在
	// 超限时直接断开连接，而不是把 1 GB 读进内存再判断。
	wsMaxReadSize = protocol.MaxControlMessageSize
)

// upgrader 构造升级器。
//
// # 为什么是方法而不是包级变量
//
// CheckOrigin 必须读配置（AllowedOrigins 白名单），而包级变量拿不到
// Server 实例。写成方法还顺带保证了「每一处升级都经过同一份 Origin 策略」。
//
// ★ CheckOrigin 在这里**再校验一次**，即使路由上已经挂了 withOriginCheck。
//
// 中间件那道闸给的是友好的 JSON 错误（前端能直接展示），
// 这一道是**兜底**：将来有人加了一条新的 WS 路由却忘了挂中间件，
// 升级仍然会被拒。两个独立的闸门都失效才会出问题，
// 而这正是「深度防御」在这个场景下的具体含义 —— WebSocket 不受
// 同源策略保护，漏一次的代价是「用户访问恶意页面就被拿了 cookie」。
func (s *Server) upgrader() websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  wsReadBufferSize,
		WriteBufferSize: wsWriteBufferSize,
		CheckOrigin:     func(r *http.Request) bool { return s.originOK(r) },
	}
}

// upgrade 完成 WebSocket 升级。
//
// 失败时 Upgrade 已经写过 HTTP 响应了，调用方只需 return ——
// 再写一次响应会产生 "superfluous WriteHeader" 警告，而且客户端
// 收到的是两个响应拼在一起，看不出真正原因。
func (s *Server) upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	// ★ 必须先赋给变量再调 Upgrade。
	//
	// Upgrader.Upgrade 的接收者是**指针**（它要写回缓冲区字段），
	// 而 `s.upgrader()` 返回的是值 —— 值不是可寻址的，
	// Go 不允许对函数返回值取地址，编译期直接报
	// `cannot call pointer method Upgrade on websocket.Upgrader`。
	u := s.upgrader()
	ws, err := u.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("WebSocket 升级失败",
			"path", r.URL.Path, "ip", s.clientIP(r), "err", err)
		return nil, err
	}
	// ★ 读上限在读之前就设好。放在读循环里判断已经晚了 ——
	// 那时数据已经在内存里了。
	ws.SetReadLimit(wsMaxReadSize)
	return ws, nil
}

// writePump 是**每条连接唯一的写者**。
//
// gorilla/websocket 明确不支持并发写，多个 goroutine 直接 WriteMessage
// 会导致帧交错 —— 而帧交错在终端场景下的表现是「屏幕上随机出现半行乱码」，
// 没有人会把它归因到并发问题上。
//
// 退出条件有三个：发送队列被关闭、写失败、连接已关闭。
// ★ 刻意**不**在这里关连接：关连接由读循环的退出路径统一负责，
// 两条路径都能关的话，「谁先关的、为什么关」就再也说不清了。
func (s *Server) writePump(ws *websocket.Conn, send <-chan outbound, done <-chan struct{}, label string) {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()

	for {
		select {
		case o, ok := <-send:
			if !ok {
				return
			}
			mt := websocket.TextMessage
			if o.binary {
				mt = websocket.BinaryMessage
			}
			if err := ws.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if err := ws.WriteMessage(mt, o.data); err != nil {
				s.log.Debug("WebSocket 写失败，连接将关闭", "conn", label, "err", err)
				return
			}

		case <-ticker.C:
			if err := ws.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(wsWriteWait)); err != nil {
				s.log.Debug("发送 Ping 失败，连接将关闭", "conn", label, "err", err)
				return
			}

		case <-done:
			return
		}
	}
}

// sendErrorEnvelope 给对端回一条 error 控制消息。
//
// 用于「请求本身合法、但服务端处理不了」的情况（无权访问、设备离线……）。
// 与「直接断开连接」的分工：**协议层的越界行为断连，业务层的失败回错误**。
// 前者说明对端在试探（或已损坏），继续对话没有意义；
// 后者只是这一次请求不成立，连接还能继续用。
func sendErrorEnvelope(send func([]byte) error, req *protocol.Envelope, err error) {
	env := protocol.NewErrorEnvelope(replyTo(req), err)
	data, encErr := protocol.Encode(env)
	if encErr != nil {
		return
	}
	_ = send(data)
}

// replyTo 取请求的 request_id（用于把错误回填到正确的请求上）。
func replyTo(req *protocol.Envelope) string {
	if req == nil {
		return ""
	}
	return req.RequestID
}

// refreshReadDeadline 在每次成功读到消息后重置读超时。
//
// 必须每次重置，而不是设一个固定 deadline：固定 deadline 会在
// 到点那一刻掐断一条**完全健康**的长连接 —— 而终端会话可能几个小时
// 都没有任何输入，这正是长连接的正常形态。
func refreshReadDeadline(ws *websocket.Conn, d time.Duration) {
	_ = ws.SetReadDeadline(time.Now().Add(d))
}

// armReadDeadline 把读超时接到 Pong 上，并在**认证完成**后调用一次。
//
// # 为什么必须有它
//
// Server 是主动发 Ping 的一方（writePump 每 wsPingPeriod 发一个控制帧 Ping），
// 对端（浏览器 / Agent）按 WebSocket 规范会自动回 Pong。但 gorilla 把收到的
// Pong 当控制帧处理：advanceFrame 直接交给 PongHandler 后继续读，
// **ReadMessage 根本不会返回** —— 而默认的 PongHandler 是空实现。
// 于是 refreshReadDeadline 只在「对端主动发业务数据」时才被调到。
//
// 这个缺口在两端的表现不对称，所以很难被发现：
//   - Agent 连接有 20s 的应用层心跳兜着，服务端这侧看不出任何问题；
//   - 浏览器连接空闲时一个字都不发，于是每条客户端连接都在 wsPongWait(90s)
//     处被服务端自己掐断，前端表现为「莫名其妙地周期性重连」。
//
// ★ 只能在认证完成后调用。握手阶段用的是 wsHandshakeTimeout(30s)：
// 若那时就把读超时接到 Pong 上，一个只发 hello、不发 auth 的连接只要回一次
// Pong 就能无限续命，那道防 goroutine/socket 耗尽的闸门就形同虚设。
func armReadDeadline(ws *websocket.Conn, d time.Duration) {
	_ = ws.SetReadDeadline(time.Now().Add(d))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(d))
	})
}
