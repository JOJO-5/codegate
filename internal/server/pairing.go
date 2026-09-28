package server

import (
	"sync"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

// 本文件管的是「配对进行中的 Agent 连接」。
//
// # 为什么需要它
//
// 配对流程里，Agent 连上来时**还没有通过认证**（它手上还没有被服务端
// 认可的设备身份），所以这条连接不能进 Registry —— 一旦进去，
// Relay 就可能把它当成合法的路由目标，而它连自己是谁都还没证明。
//
// 但服务端又必须在用户点「确认绑定」时，往**这条**连接回一个
// `agent.pair.completed`（见 internal/agent/pairing.go：Agent 发完
// `agent.pair.begin` 后就一直在同一条连接上等）。于是需要一份
// 独立的、只服务于配对流程的登记表。
//
// # 与 Registry 的三点区别
//
//  1. 键是 device_id，而 device_id 在认证前只是 Agent **自称**的
//  2. 只能「单向通知」，不能作为中继目标
//  3. 有明确过期时间（配对码 TTL），到点自动摘除

// pairWaiter 是一个等待用户确认的配对连接。
type pairWaiter struct {
	// send 是往这条连接投递消息的函数（通常是 AgentConn.TrySend）。
	//
	// 用函数而不是 *websocket.Conn：调用方不该关心底层是 WS 还是别的，
	// 也不该在这里绕过 writePump 直接写连接（gorilla 不支持并发写）。
	send func([]byte) error

	expiresAt time.Time
}

// pairRegistry 保存「已发出配对码、正在等确认」的连接。
type pairRegistry struct {
	mu sync.Mutex
	m  map[string]*pairWaiter // deviceID → waiter
}

func newPairRegistry() *pairRegistry {
	return &pairRegistry{m: make(map[string]*pairWaiter)}
}

// Register 登记一条等待配对完成的连接。
//
// 同一个 device_id 重复登记时**顶掉旧的**：Agent 可能因为网络抖动
// 重连并重新走了 pair.begin，此时旧的连接已经没用了，留着只会
// 让「确认绑定」的通知发给一条死连接。
func (p *pairRegistry) Register(deviceID string, send func([]byte) error, expiresAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[deviceID] = &pairWaiter{send: send, expiresAt: expiresAt}
}

// Unregister 摘除登记（连接断开或配对完成时调用）。
func (p *pairRegistry) Unregister(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, deviceID)
}

// Notify 给某设备的等待连接发一条控制消息。
//
// 返回 false 表示「没有在等的连接」—— 这**不是错误**：
// 用户可能在 Agent 那边超时断开之后才点了确认（配对码在浏览器侧
// 仍然有效），此时绑定照样应该成功，只是没法通知 Agent 罢了。
// Agent 下次跑 `pair` 或 `run` 时会发现设备已绑定。
func (p *pairRegistry) Notify(deviceID string, env *protocol.Envelope) bool {
	p.mu.Lock()
	w, ok := p.m[deviceID]
	if ok {
		delete(p.m, deviceID)
	}
	p.mu.Unlock()

	if !ok {
		return false
	}

	data, err := protocol.Encode(env)
	if err != nil {
		return false
	}
	return w.send(data) == nil
}

// Waiting 判断某设备当前是否有等待确认的连接（诊断用）。
func (p *pairRegistry) Waiting(deviceID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.m[deviceID]
	return ok
}

// Len 返回当前等待配对的数量。
func (p *pairRegistry) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}

// sweep 清理已过期的等待项，由后台清理循环调用。
//
// 过期的项必须清：Agent 那边等超时会自己断开，但断开通知要经由
// readPump 退出才能到达 Unregister，中间可能有一小段延迟；
// 而且如果 Agent 进程被强杀（没有正常关闭帧），Unregister 根本不会跑。
func (p *pairRegistry) sweep(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, w := range p.m {
		if now.After(w.expiresAt) {
			delete(p.m, k)
		}
	}
}
