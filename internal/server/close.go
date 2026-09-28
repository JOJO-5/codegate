package server

import (
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// WebSocket 关闭码（架构文档 §14.3）。
//
// 1000-1015 是 RFC 6455 定义的标准码，4000-4999 是应用自定义区间。
//
// ★ 这张表是**协议的一部分**，不是实现细节：Agent 和浏览器都按它决定
// 「要不要重连」。改一个码值，等于改了一个客户端的重试策略 ——
// 而且改错了的表现是「服务端重启后所有 Agent 疯狂重连」或
// 「认证失败的 Agent 永远不重连」，两种都很难从日志里一眼看出来。
const (
	// CloseNormal 正常关闭。对端不重连。
	CloseNormal = 1000

	// CloseGoingAway 服务端要关了。
	//
	// ★ 文档 §14.3 的表里没有这一行，因为它是标准码而不是自定义码。
	// 选 1001 而不是 1013：1013（Try Again Later）表达的是「过载，稍后再来」，
	// 而优雅关闭的语义是「我要走了」，对端应当按常规退避重连 —— 1001 正好。
	CloseGoingAway = 1001

	// ClosePolicyViolation 对端违反了协议（RFC 6455 §7.4.1 的 1008）。
	//
	// 触发场景：发来方向非法的控制消息（浏览器冒充 Agent）、
	// 二进制帧头损坏、消息类型不是 Text/Binary。
	//
	// **不可重连**：这不是网络问题，是对端逻辑错了。让它重连只会
	// 得到一模一样的失败 —— 而且重连风暴会把日志冲干净，
	// 把真正的错误埋掉。
	ClosePolicyViolation = 1008

	// CloseFrameTooLarge 收到超限帧。这是本端 bug 或对端不守协议，不重连。
	CloseFrameTooLarge = 1009

	// CloseInternalError 服务端内部错误（RFC 6455 §7.4.1 的 1011）。
	//
	// 用于「写数据库失败」这类本端问题。**可重连** ——
	// 对端没有任何过错，重连往往就好了。
	CloseInternalError = 1011

	// CloseOverloaded 服务端过载，或慢消费者被踢。**可重连**（退避）。
	CloseOverloaded = 1013

	// CloseUnauthorized 认证失败。
	// Agent 需重新 pair，浏览器需重新登录。**不可重连**。
	//
	// ★ 必须与 internal/agent.CloseUnauthorized 保持一致。
	CloseUnauthorized = 4401

	// CloseForbidden 已认证但无权限。**不可重连**。
	CloseForbidden = 4403

	// CloseDuplicateConnection 同一设备的旧连接被新连接顶掉（§8.1）。
	//
	// **不可重连**：新连接已经在服务了，旧连接再连回来只会两边互踢，
	// 形成 1 秒一次的重连风暴，日志和 CPU 都被吃掉。
	CloseDuplicateConnection = 4409

	// CloseVersionUnsupported 协议版本不兼容，需人工升级。**不可重连**。
	//
	// ★ 必须与 internal/agent.CloseVersionUnsupported 保持一致。
	CloseVersionUnsupported = 4426

	// CloseRateLimited 建连过于频繁。**可重连**（退避）。
	CloseRateLimited = 4429
)

// maxCloseReasonLen 是关闭帧里 reason 的最大字节数。
//
// RFC 6455 §5.5：控制帧的 payload 上限 125 字节，其中前 2 字节是关闭码，
// 所以 reason 最多 123 字节。超了 WriteControl 会直接返回错误 ——
// 结果是「想告诉对方为什么关，结果连关闭帧都没发出去」，
// 对端只能看到 TCP 断开（1006 异常关闭），把主动踢人误判成网络故障。
const maxCloseReasonLen = 123

// closeWriteTimeout 是发送关闭帧的写超时。
//
// 用固定短值而不是配置项：走到这一步说明对端已经不可靠了，
// 不能为了送出一句告别话把清理协程挂住。
const closeWriteTimeout = 2 * time.Second

// closeWithCode 向一条连接发送关闭帧。
//
// ★ 用 WriteControl 而不是走 send channel。
//
// gorilla 明确保证 Close/WriteControl 可以与其他所有写方法并发调用，
// 所以它不必排在 writePump 的队列后面 —— 而队列满时（正是「慢消费者
// 被踢」这个场景）恰恰最需要能把关闭帧立刻发出去。走队列的话，
// 关闭帧会排在几百个待发数据帧后面，等排到的时候连接早就被别处关掉了。
//
// ★ 刻意**不**在这里调用 conn.Close()。
//
// 关闭帧只是「通知」，真正的关闭由对端的 readPump 收到后返回、
// 再走正常清理路径。本地直接把 TCP 掐了，对端看到的是 1006（异常关闭），
// 于是把「服务端主动踢我」误判成「网络断了」，触发不必要的重连。
func closeWithCode(c *websocket.Conn, code int, reason string) {
	if c == nil {
		return
	}
	reason = truncateUTF8(reason, maxCloseReasonLen)
	msg := websocket.FormatCloseMessage(code, reason)
	_ = c.WriteControl(websocket.CloseMessage, msg, time.Now().Add(closeWriteTimeout))
}

// truncateUTF8 按字节截断，但保证结果仍是合法 UTF-8。
//
// 关闭帧的 reason 必须是合法 UTF-8，否则对端可能直接判协议错误
// （有些实现会关掉连接而不是忽略）。所以不能像 audit.go 的 truncate
// 那样直接切字节 —— 那里切的是入库字段，这里切的是协议帧。
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 从 n 往前退，退到某个字符的起始字节为止。
	// UTF-8 单字符最长 4 字节，所以最多退 3 次。
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
