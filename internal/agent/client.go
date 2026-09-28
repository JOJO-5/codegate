package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 连接层的时间参数（§3.3）。
const (
	// writeTimeout 是单帧写不完就判定对端卡死的时间。
	writeTimeout = 10 * time.Second

	// readTimeout 是读不到任何帧（含 Ping/Pong）就断链重连的时间。
	//
	// 90s 而不是"心跳间隔 × 2"：Server 每 30s 发 WS Ping，
	// 就算应用层心跳全丢，只要有 Ping 在，链路就是活的。
	// 用 90s 可以容忍连续两次 Ping 丢失。
	readTimeout = 90 * time.Second

	// sendQueueSize 是发送队列长度。
	sendQueueSize = 256

	// sendEnqueueTimeout 是控制消息入队的等待上限。
	sendEnqueueTimeout = 5 * time.Second

	// maxReadSize 是单条入站消息的大小上限，与 protocol 的控制消息上限一致。
	maxReadSize = 1 << 20
)

// WebSocket 关闭码。4400-4499 是应用自定义区间。
const (
	// CloseUnauthorized 表示 Server 判定认证失败。
	//
	// 收到它**不能重连** —— 设备密钥不对或已被解绑，
	// 重连一万次也一样，只会把日志刷满。
	CloseUnauthorized = 4401

	// CloseVersionUnsupported 表示协议版本不兼容。
	//
	// 同样不重连：需要人工升级 Agent。
	CloseVersionUnsupported = 4426
)

// ErrAuthRejected 表示 Server 明确拒绝认证（不是网络问题）。
//
// 它与 CloseUnauthorized 是同一类事的两种表达：前者是收到了 error 消息，
// 后者是收到了关闭码。两者都必须**不重连**。
var ErrAuthRejected = errors.New("agent: 认证被拒绝")

// IsFatalError 判断错误是否属于「不该重连」（§3.2）。
//
// 两类：认证被拒（设备密钥不对 / 已被解绑）、协议版本不兼容（需人工升级）。
// 其余一律重连 —— 包括 Server 重启、网络抖动、TLS 握手失败。
//
// 区分它们很重要：前者重连只会把日志刷满，并让用户以为"网络不好"，
// 而真正的问题是"需要重新配对"；后者不重连就等于"网络抖一下要人工介入"。
func IsFatalError(err error) bool {
	if errors.Is(err, ErrAuthRejected) {
		return true
	}
	return IsFatalClose(err)
}

// IsFatalClose 判断一个错误是否属于「不该重连」的致命关闭（§3.2）。
func IsFatalClose(err error) bool {
	var ce *websocket.CloseError
	if !errors.As(err, &ce) {
		return false
	}
	switch ce.Code {
	case CloseUnauthorized, CloseVersionUnsupported:
		return true
	default:
		return false
	}
}

// outbound 是一条待发送的帧。
type outbound struct {
	binary bool
	data   []byte
}

// Conn 是一条到 Server 的 WebSocket 连接。
//
// # 并发模型：单写者
//
// 所有写出都经过 send channel，由唯一的 writePump goroutine 执行。
// 这不是性能优化，是**正确性要求**：gorilla/websocket 明确不支持并发写，
// 多个 goroutine 直接 WriteMessage 会导致帧交错。而帧交错在终端场景下的
// 表现是「屏幕上随机出现半行乱码」—— 没人会把它归因到并发问题上。
type Conn struct {
	ws   *websocket.Conn
	log  *slog.Logger
	send chan outbound

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error

	// 分发回调，由上层注入。
	//
	// ★ 它们必须**非阻塞**：在 readPump 里被同步调用，阻塞住就等于
	// 停止读，而停止读会让 TCP 接收窗口填满，最终反压到 Server ——
	// 一个慢的本地处理会把整条链路拖死。
	onText   func([]byte)
	onBinary func([]byte)

	pumpDone sync.WaitGroup
}

// DialOptions 是建立连接所需的参数。
type DialOptions struct {
	URL      string
	Header   http.Header
	Insecure bool
	Log      *slog.Logger
	OnText   func([]byte)
	OnBinary func([]byte)
}

// Dial 建立到 Server 的 WebSocket 连接并启动读写泵。
//
// ctx 只控制**建连阶段**（DNS + TCP + TLS + Upgrade）。连接建立之后，
// 它的生命周期由 Close 控制 —— 用建连的 ctx 管整条连接会导致
// 上层传进来的一个带超时的 context 悄悄把长连接掐断。
func Dial(ctx context.Context, opts DialOptions) (*Conn, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: DefaultConnectTimeout,
		// Insecure 时跳过证书校验。仅供开发（自签证书）。
		// 生产必须关掉 —— 终端流量是明文的，TLS 是唯一的保护层。
		TLSClientConfig: &tls.Config{InsecureSkipVerify: opts.Insecure}, //nolint:gosec // 见上
	}

	ws, resp, err := dialer.DialContext(ctx, opts.URL, opts.Header)
	if err != nil {
		// 把 HTTP 状态码带出来：403/404 和"连不上"是完全不同的问题，
		// 只报一个 "websocket: bad handshake" 会让人查错方向。
		if resp != nil {
			return nil, fmt.Errorf("agent: 连接 %s 失败（HTTP %d）: %w", opts.URL, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("agent: 连接 %s 失败: %w", opts.URL, err)
	}

	c := &Conn{
		ws:       ws,
		log:      log,
		send:     make(chan outbound, sendQueueSize),
		closed:   make(chan struct{}),
		onText:   opts.OnText,
		onBinary: opts.OnBinary,
	}

	c.pumpDone.Add(2)
	go c.readPump()
	go c.writePump()

	return c, nil
}

// ---------------------------------------------------------------------------
// 发送
// ---------------------------------------------------------------------------

// SendText 发送一条控制消息。
//
// 阻塞直到入队或超时。控制消息**不能丢** —— 丢掉一条 session.exit
// 会让 UI 永远显示「运行中」，而用户看到的现象是"进程明明结束了"。
// 所以这里宁可阻塞（有上限）也不丢。
func (c *Conn) SendText(data []byte) error {
	return c.enqueue(outbound{data: data}, sendEnqueueTimeout)
}

// SendBinary 发送一条二进制帧。
//
// ★ 非阻塞：队列满时立即返回 false，由调用方标记 DROPPED。
//
// 终端输出**可以丢**（§R3）：丢数据总好过把 PTY 读循环堵死 ——
// 后者会让用户的 CLI 直接卡住，那是"整个会话不能用"，
// 而丢几帧只是"屏幕上少了点历史"。
func (c *Conn) SendBinary(data []byte) bool {
	return c.enqueue(outbound{binary: true, data: data}, 0) == nil
}

// enqueue 把帧放进发送队列。timeout 为 0 表示非阻塞。
func (c *Conn) enqueue(out outbound, timeout time.Duration) error {
	select {
	case <-c.closed:
		return ErrConnClosed
	case c.send <- out:
		return nil
	default:
	}

	if timeout <= 0 {
		return ErrSendQueueFull
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.closed:
		return ErrConnClosed
	case c.send <- out:
		return nil
	case <-timer.C:
		return fmt.Errorf("%w: 等待 %s 仍无法入队", ErrSendQueueFull, timeout)
	}
}

// ---------------------------------------------------------------------------
// 读写泵
// ---------------------------------------------------------------------------

func (c *Conn) writePump() {
	// ★ 这里只标记退出，**不能**调 c.Close()。
	//
	// Close() 会 pumpDone.Wait() 等两个泵退出；如果泵自己在退出时调 Close，
	// 就变成「readPump 等 writePump 的 Done，writePump 等 readPump 的 Done」——
	// 而两者的 Done 都排在 Close() 之后，永远等不到。死锁。
	//
	// 正确分工：泵只负责「发现错误 → fail()」，等退出这件事由
	// 外部调用方（或另一个泵退出后触发的 fail）通过 Close() 完成。
	defer c.pumpDone.Done()

	for {
		select {
		case <-c.closed:
			return
		case out := <-c.send:
			mt := websocket.TextMessage
			if out.binary {
				mt = websocket.BinaryMessage
			}
			// 写超时保护：对端卡死时不能让这个 goroutine 无限阻塞，
			// 否则队列很快填满，整个 Agent 都发不出东西。
			if err := c.ws.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				c.fail(err)
				return
			}
			if err := c.ws.WriteMessage(mt, out.data); err != nil {
				c.fail(err)
				return
			}
		}
	}
}

func (c *Conn) readPump() {
	defer c.pumpDone.Done() // 见 writePump 的说明：不能在泵里调 Close()

	c.ws.SetReadLimit(maxReadSize)
	_ = c.ws.SetReadDeadline(time.Now().Add(readTimeout))
	// Server 每 30s 发 Ping；收到就把读超时往后推。
	// gorilla 会自动回 Pong，这里只需要重置 deadline。
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(readTimeout))
	})

	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			c.fail(err)
			return
		}
		// 任何帧（不只是 Pong）都说明链路活着。
		_ = c.ws.SetReadDeadline(time.Now().Add(readTimeout))

		switch mt {
		case websocket.TextMessage:
			if c.onText != nil {
				c.onText(data)
			}
		case websocket.BinaryMessage:
			if c.onBinary != nil {
				c.onBinary(data)
			}
		default:
			// 忽略其他帧类型。不报错：未来的协议扩展可能引入新类型，
			// 旧代码应当容忍而不是断连（§9.4 的演进原则）。
		}
	}
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// fail 记录第一个导致连接终止的错误。
func (c *Conn) fail(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		close(c.closed)
		_ = c.ws.Close()
	})
}

// Close 关闭连接。幂等。
func (c *Conn) Close() error {
	c.fail(nil)
	// 等两个泵退出，保证调用方在 Close 返回后不会再收到回调 ——
	// 否则上层可能在已经清理过的状态上被回调一次，引发竞态。
	c.pumpDone.Wait()
	return c.closeErr
}

// Done 在连接终止时关闭。
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Err 返回导致连接终止的错误。正常关闭时为 nil。
func (c *Conn) Err() error {
	select {
	case <-c.closed:
		return c.closeErr
	default:
		return nil
	}
}

// 连接层的哨兵错误。
var (
	// ErrConnClosed 表示连接已经关闭。
	ErrConnClosed = errors.New("agent: 连接已关闭")
	// ErrSendQueueFull 表示发送队列已满。
	ErrSendQueueFull = errors.New("agent: 发送队列已满")
)
