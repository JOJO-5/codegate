package server

import (
	"sync"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

// pendingTTL 是一条待回请求的最长存活时间。
//
// 超过它还没收到 Agent 的响应，就认为这条请求永远不会回来了 ——
// 可能是 Agent 崩了、可能响应在半路丢了。留着它只会让 map 慢慢涨大，
// 而且更糟的是：**它会掩盖真正的故障**（前端等到天荒地老，
// 日志里却什么都没有）。
const pendingTTL = 60 * time.Second

// pendingReq 是一条「客户端发出、等 Agent 回」的请求。
//
// 需要它是因为中继是**无状态转发**：Server 把 `session.create` 转给 Agent，
// Agent 回的 `session.created` 里只有 `reply_to`，没有任何信息说明
// 这条响应该发给哪个浏览器连接。而 Server 可能同时服务着几十个客户端。
//
// 换句话说：request_id 是客户端生成的，Agent 只是原样回填，
// 所以**只有 Server 记得住这个映射**。
type pendingReq struct {
	client *ClientConn

	// 下面三个字段是「收到响应时要做的副作用」所需的上下文。
	// 比如 session.attached 回来时，Server 要顺手把这条客户端
	// 订阅到那个会话上 —— 而 session_id 只在请求里，不在响应里。
	userID    string
	deviceID  string
	sessionID string
	kind      protocol.Type
	requestID string
	// release undoes a provisional attach subscription if no success arrives.
	release func()

	expiresAt time.Time
}

// pendingRegistry 保存待回请求。
type pendingRegistry struct {
	mu sync.Mutex
	m  map[string]*pendingReq
}

func newPendingRegistry() *pendingRegistry {
	return &pendingRegistry{m: make(map[string]*pendingReq)}
}

// Add 登记一条待回请求。
//
// ★ request_id 重复时**顶掉旧的**，而不是报错。
//
// 理由：request_id 由客户端生成。一个有 bug 的前端（比如用固定 id）
// 会不断覆盖自己 —— 顶掉旧的行为让「后一次请求能被正确应答」，
// 而报错会让那个前端完全不可用。代价是前一次请求的响应会被
// 当成「未知 reply_to」丢掉，这本来就是它的归宿（前端自己搞混了）。
func (p *pendingRegistry) Add(reqID string, r *pendingReq, now time.Time) {
	if reqID == "" {
		return
	}
	r.expiresAt = now.Add(pendingTTL)

	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.m[reqID]; old != nil && old.release != nil { old.release() }
	p.m[reqID] = r
}

// Take 取出并删除一条待回请求。
//
// 取到就删（而不是「查完再删」）：一次请求只该被应答一次。
// 不删的话，一个恶意（或出错）的 Agent 可以拿同一个 reply_to
// 反复回消息，把内容重复投递给客户端。
func (p *pendingRegistry) Take(reqID string) (*pendingReq, bool) {
	if reqID == "" {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.m[reqID]
	if ok {
		delete(p.m, reqID)
	}
	return r, ok
}

// DropClient 摘除某条客户端连接的所有待回请求。
//
// 客户端断开时必须调：否则这些请求会一直挂到 TTL 过期，
// 期间 Agent 回过来的响应会被投递给一条已经关闭的连接
// （TrySend 会失败并记一条无意义的警告）。
func (p *pendingRegistry) DropClient(c *ClientConn) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0
	for k, r := range p.m {
		if r.client == c {
			delete(p.m, k)
			if r.release != nil { r.release() }
			n++
		}
	}
	return n
}

// DropDevice 摘除某设备相关的全部待回请求（Agent 断线时调用）。
func (p *pendingRegistry) DropDevice(deviceID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0
	for k, r := range p.m {
		if r.deviceID == deviceID {
			delete(p.m, k)
			if r.release != nil { r.release() }
			n++
		}
	}
	return n
}

// sweep 清理超时未回的请求，由后台清理循环调用。
//
// ★ 顺带给客户端回一条错误。
//
// 静默清理是不行的：前端的 UI 会永远停在 loading 上，用户以为
// 「点了没反应」。回一条 timeout 错误至少能让它显示出「请求超时，请重试」。
func (p *pendingRegistry) sweep(now time.Time) {
	p.mu.Lock()
	var expired []*pendingReq
	for k, r := range p.m {
		if now.After(r.expiresAt) {
			delete(p.m, k)
			expired = append(expired, r)
		}
	}
	p.mu.Unlock()

	for _, r := range expired {
		if r.release != nil { r.release() }
		sendErrorEnvelope(r.client.TrySendText, nil,
			protocol.NewRetryableError(protocol.CodeInternal, "请求超时，请重试"))
	}
}

// Len 返回当前待回请求数量（诊断用）。
func (p *pendingRegistry) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}
