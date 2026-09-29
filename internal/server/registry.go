package server

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jojo/codegate/internal/protocol"
)

// ErrSendQueueFull 表示目标连接的发送队列已满（慢消费者）。
//
// 调用方拿到它之后的动作是**有讲究的**：
//   - 终端数据帧：丢弃 + 记计数，并让客户端做一次全量重放（R3）
//   - 控制消息：记警告。控制消息频率低，队列满说明这条连接已经不可用了
var ErrSendQueueFull = errors.New("server: 发送队列已满")

// defaultSendQueueSize 是每条连接的发送队列深度。
//
// ★ 刻意从 1024 降到 256（R6）：队列里存的是 []byte，深度 1024 × 平均 4 KB
// 就是 4 MB/连接的上限，1000 条连接 = 4 GB，不可接受。
// 降到 256 后上限约 1 MB，而**实际占用取决于瞬时排队量**（空闲时接近 0）。
//
// 代价是慢消费者更容易触发丢帧 —— 但丢帧本来就是设计中的行为（R3）：
// 宁可丢帧让客户端重放，也不能让 Agent 的 PTY 读循环被阻塞。
const defaultSendQueueSize = 256

// outbound 是一条待发送的消息。
//
// ★ 必须带上 binary 标记，不能靠「数据看起来像不像 JSON」来猜。
//
// WebSocket 的 Text 与 Binary 是两种**不同的帧类型**，对端按帧类型
// 决定怎么解析（Text 走 JSON 解码，Binary 走 20 字节头解析）。
// 如果这里不记录，writePump 就只能瞎猜 —— 而猜错的后果是
// 「终端输出被当成 JSON 解析」这类完全无法从日志里看出来的故障。
type outbound struct {
	binary bool
	data   []byte
}

// AgentConn 是一条已通过 Ed25519 认证的 Agent 长连接。
type AgentConn struct {
	DeviceID string
	UserID   string

	conn *websocket.Conn

	// send 是**唯一**允许向这条 WS 写入的通道。
	// gorilla/websocket 不支持并发写，所有写入必须经由单个 writePump。
	send chan outbound

	// sessions 是 Agent 自报的当前会话集合，用于快速鉴权。
	// 由心跳与 session.sync 刷新。
	sessMu   sync.RWMutex
	sessions map[string]struct{}

	lastSeen  atomic.Int64 // Unix 毫秒
	caps      protocol.AgentCaps
	updateMu sync.RWMutex
	updateStatus *protocol.AgentUpdateStatus
	commands []protocol.CommandAvailability
	agentVer  string
	platform  string
	connected time.Time

	closeOnce sync.Once
	closed    chan struct{}
}

func newAgentConn(ws *websocket.Conn, queueSize int) *AgentConn {
	if queueSize <= 0 {
		queueSize = defaultSendQueueSize
	}
	c := &AgentConn{
		conn:      ws,
		send:      make(chan outbound, queueSize),
		sessions:  make(map[string]struct{}),
		connected: time.Now(),
		closed:    make(chan struct{}),
	}
	c.Touch()
	return c
}

// Send 返回只读的发送通道，由 writePump 消费。
func (c *AgentConn) Send() <-chan outbound { return c.send }

// Done 在连接关闭后关闭，供 readPump/外部协程等待。
func (c *AgentConn) Done() <-chan struct{} { return c.closed }

// TrySendText 非阻塞投递一条控制消息（WS Text 帧）。
//
// ★ 必须非阻塞：这个函数在 Relay 的转发路径上，如果它阻塞，
// 一个慢客户端就会把整个转发链路（乃至其他客户端的转发）拖住。
func (c *AgentConn) TrySendText(data []byte) error { return c.trySend(outbound{data: data}) }

// TrySendBinary 非阻塞投递一条二进制帧（WS Binary 帧）。
func (c *AgentConn) TrySendBinary(data []byte) error {
	return c.trySend(outbound{binary: true, data: data})
}

func (c *AgentConn) SendWeb(data []byte) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-c.closed:
		return errors.New("agent disconnected")
	case c.send <- outbound{binary: true, data: data}:
		return nil
	case <-timer.C:
		return ErrSendQueueFull
	}
}

func (c *AgentConn) trySend(o outbound) error {
	select {
	case c.send <- o:
		return nil
	default:
		return ErrSendQueueFull
	}
}

// Close 幂等地关闭连接。重复调用安全。
func (c *AgentConn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		// 关掉底层连接让 readPump 的 ReadMessage 立刻返回。
		// 不在这里 close(c.send)：writePump 可能正在 select 它，
		// 由 writePump 自己在退出时处理。
		_ = c.conn.Close()
	})
}

// Touch 记录一次活动（收到任何消息时调用）。
func (c *AgentConn) Touch() { c.lastSeen.Store(time.Now().UnixMilli()) }

// LastSeen 返回最后活动时间。
func (c *AgentConn) LastSeen() time.Time {
	return time.UnixMilli(c.lastSeen.Load())
}

// ConnectedAt 返回连接建立时间。
func (c *AgentConn) ConnectedAt() time.Time { return c.connected }

// Caps 返回 Agent 自报的能力。
func (c *AgentConn) Caps() protocol.AgentCaps { return c.caps }

// AgentVersion 返回 Agent 版本（审计与展示用）。
func (c *AgentConn) AgentVersion() string { return c.agentVer }

// UpdateStatus is scoped to this authenticated connection and cannot outlive it.
func (c *AgentConn) UpdateStatus() *protocol.AgentUpdateStatus {
	c.updateMu.RLock()
	defer c.updateMu.RUnlock()
	if c.updateStatus == nil { return nil }
	copy := *c.updateStatus
	return &copy
}

func (c *AgentConn) SetUpdateStatus(status protocol.AgentUpdateStatus) {
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	c.updateStatus = &status
}

func (c *AgentConn) SetCommands(commands []protocol.CommandAvailability) {
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	c.commands = append([]protocol.CommandAvailability(nil), commands...)
}

func (c *AgentConn) Commands() []protocol.CommandAvailability {
	c.updateMu.RLock()
	defer c.updateMu.RUnlock()
	return append([]protocol.CommandAvailability(nil), c.commands...)
}

// SetSessions 整体替换该 Agent 持有的会话集合。
//
// 用「整体替换」而不是增量更新：Agent 重启后可能丢会话（R5），
// 增量更新会在这种情况下留下幽灵条目。整体替换天然收敛。
func (c *AgentConn) SetSessions(ids []string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.sessions = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		c.sessions[id] = struct{}{}
	}
}

// AddSession 登记一个新会话（session.created 时调用）。
func (c *AgentConn) AddSession(id string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.sessions[id] = struct{}{}
}

// RemoveSession 摘除一个会话（session.closed / session.exit 时调用）。
func (c *AgentConn) RemoveSession(id string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	delete(c.sessions, id)
}

// HasSession 判断该 Agent 当前是否持有某会话。
func (c *AgentConn) HasSession(id string) bool {
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	_, ok := c.sessions[id]
	return ok
}

// SessionIDs 返回该 Agent 持有的全部会话 ID 快照。
func (c *AgentConn) SessionIDs() []string {
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	out := make([]string, 0, len(c.sessions))
	for id := range c.sessions {
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------

// ClientConn 是一条已认证的浏览器长连接。
type ClientConn struct {
	ID     string // 连接 ID（UUID），用于日志与角色标识
	UserID string

	conn *websocket.Conn
	send chan outbound

	// attached 是该连接当前 attach 的会话集合。
	// 一个客户端同时只看一个终端，但保留集合是为了将来支持分屏。
	attMu    sync.RWMutex
	attached map[string]struct{}
	attachIDs map[string]string

	lastSeen  atomic.Int64
	connected time.Time

	closeOnce sync.Once
	closed    chan struct{}
}

func newClientConn(id string, ws *websocket.Conn, queueSize int) *ClientConn {
	if queueSize <= 0 {
		queueSize = defaultSendQueueSize
	}
	c := &ClientConn{
		ID:        id,
		conn:      ws,
		send:      make(chan outbound, queueSize),
		attached:  make(map[string]struct{}),
		attachIDs: make(map[string]string),
		connected: time.Now(),
		closed:    make(chan struct{}),
	}
	c.Touch()
	return c
}

func (c *ClientConn) Send() <-chan outbound  { return c.send }
func (c *ClientConn) Done() <-chan struct{}  { return c.closed }
func (c *ClientConn) Touch()                 { c.lastSeen.Store(time.Now().UnixMilli()) }
func (c *ClientConn) LastSeen() time.Time    { return time.UnixMilli(c.lastSeen.Load()) }
func (c *ClientConn) ConnectedAt() time.Time { return c.connected }

// TrySendText 非阻塞投递一条控制消息（WS Text 帧）。
func (c *ClientConn) TrySendText(data []byte) error { return c.trySend(outbound{data: data}) }

// TrySendBinary 非阻塞投递一条二进制帧（WS Binary 帧）。
func (c *ClientConn) TrySendBinary(data []byte) error {
	return c.trySend(outbound{binary: true, data: data})
}

func (c *ClientConn) trySend(o outbound) error {
	select {
	case c.send <- o:
		return nil
	default:
		return ErrSendQueueFull
	}
}

func (c *ClientConn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

// Attach 记录该客户端已 attach 到某会话。
func (c *ClientConn) Attach(sessionID string) {
	c.attMu.Lock()
	defer c.attMu.Unlock()
	c.attached[sessionID] = struct{}{}
}

// Detach 解除 attach 记录。
func (c *ClientConn) Detach(sessionID string) {
	c.attMu.Lock()
	defer c.attMu.Unlock()
	delete(c.attached, sessionID)
	delete(c.attachIDs, sessionID)
}

// SetAttachID remembers the request that created the Agent-side view.
func (c *ClientConn) SetAttachID(sessionID, attachID string) {
	c.attMu.Lock()
	defer c.attMu.Unlock()
	c.attachIDs[sessionID] = attachID
}

func (c *ClientConn) AttachID(sessionID string) string {
	c.attMu.RLock()
	defer c.attMu.RUnlock()
	return c.attachIDs[sessionID]
}

// IsAttached 判断是否已 attach 到某会话。
func (c *ClientConn) IsAttached(sessionID string) bool {
	c.attMu.RLock()
	defer c.attMu.RUnlock()
	_, ok := c.attached[sessionID]
	return ok
}

// AttachedToOther 判断该连接是否已 attach 到**别的**会话。
//
// 用途：一条连接同时只能看一个终端（§7.6）。前端切会话时必须
// 先 `session.detach` 再 `session.attach`，这里就是那个约束的
// 判定入口 —— 同会话重复 attach 是幂等的，不算冲突。
func (c *ClientConn) AttachedToOther(sessionID string) bool {
	c.attMu.RLock()
	defer c.attMu.RUnlock()
	for id := range c.attached {
		if id != sessionID {
			return true
		}
	}
	return false
}

// AttachedSessions 返回已 attach 的会话 ID 快照。
func (c *ClientConn) AttachedSessions() []string {
	c.attMu.RLock()
	defer c.attMu.RUnlock()
	out := make([]string, 0, len(c.attached))
	for id := range c.attached {
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------

// Registry 是所有活跃连接的注册表。
//
// 它同时维护三份索引：
//   - agents:      deviceID → AgentConn（路由 Agent 方向的数据）
//   - clients:     connID   → ClientConn（生命周期管理）
//   - sessionSubs: sessionID → 订阅的客户端（多客户端广播，§7.6）
//   - sessionOwner: sessionID → deviceID（把客户端请求路由到正确的 Agent）
//
// 所有方法可并发调用。
type Registry struct {
	mu      sync.RWMutex
	agents  map[string]*AgentConn
	clients map[string]*ClientConn

	sessionSubs  map[string]map[string]*ClientConn
	sessionOwner map[string]string

	queueSize int
}

// NewRegistry 构造注册表。queueSize <= 0 时用默认值。
func NewRegistry(queueSize int) *Registry {
	if queueSize <= 0 {
		queueSize = defaultSendQueueSize
	}
	return &Registry{
		agents:       make(map[string]*AgentConn),
		clients:      make(map[string]*ClientConn),
		sessionSubs:  make(map[string]map[string]*ClientConn),
		sessionOwner: make(map[string]string),
		queueSize:    queueSize,
	}
}

// ---- Agent ----

// AddAgent 注册一条 Agent 连接。
//
// ★ 返回被顶掉的旧连接（可能为 nil）。
// 理由（§8.1）：Agent 重启或切网时旧连接可能还没超时，
// 如果拒绝新连接会导致最长 60 秒不可用。所以策略是**新连接顶掉旧连接**，
// 由调用方给旧连接发 close 4409 `duplicate_connection`。
func (r *Registry) AddAgent(c *AgentConn) (evicted *AgentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if old, ok := r.agents[c.DeviceID]; ok && old != c {
		evicted = old
		// 顺手清理旧连接占用的 session 索引，避免残留指向已死连接。
		for sid := range r.sessionOwner {
			if r.sessionOwner[sid] == c.DeviceID {
				delete(r.sessionOwner, sid)
			}
		}
	}
	r.agents[c.DeviceID] = c
	return evicted
}

// RemoveAgent 摘除一条 Agent 连接。
//
// 只在「注册表里当前就是这条连接」时才删除 ——
// 否则会把刚顶上来新连接误删（旧连接的清理协程总是会跑到的）。
func (r *Registry) RemoveAgent(c *AgentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur, ok := r.agents[c.DeviceID]
	if !ok || cur != c {
		return
	}
	delete(r.agents, c.DeviceID)
	for sid := range r.sessionOwner {
		if r.sessionOwner[sid] == c.DeviceID {
			delete(r.sessionOwner, sid)
		}
	}
}

// Agent 返回某设备的活跃 Agent 连接。
func (r *Registry) Agent(deviceID string) (*AgentConn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.agents[deviceID]
	return c, ok
}

// IsOnline 实现 device.OnlineChecker。
func (r *Registry) IsOnline(deviceID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.agents[deviceID]
	return ok
}

// AgentCount 返回当前 Agent 连接数。
func (r *Registry) AgentCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.agents)
}

// ---- Client ----

// AddClient 注册一条浏览器连接。
func (r *Registry) AddClient(c *ClientConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[c.ID] = c
}

// RemoveClient 摘除一条浏览器连接，并清理它所有的订阅关系。
//
// ★ 必须清理 sessionSubs：不清的话，会话广播会往一条已关闭的连接
// 反复投递，队列很快满，日志被 ErrSendQueueFull 刷屏，
// 而真正的慢消费者问题就被淹没了。
func (r *Registry) RemoveClient(c *ClientConn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.clients, c.ID)
	for sid, subs := range r.sessionSubs {
		delete(subs, c.ID)
		if len(subs) == 0 {
			delete(r.sessionSubs, sid)
		}
	}
}

// Client 按连接 ID 取连接。
func (r *Registry) Client(id string) (*ClientConn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.clients[id]
	return c, ok
}

// ClientCount 返回当前浏览器连接数。
func (r *Registry) ClientCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

// ---- 会话订阅（多客户端广播）----

// Subscribe 让一个客户端订阅某会话的输出。
func (r *Registry) Subscribe(sessionID string, c *ClientConn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	subs, ok := r.sessionSubs[sessionID]
	if !ok {
		subs = make(map[string]*ClientConn)
		r.sessionSubs[sessionID] = subs
	}
	subs[c.ID] = c
}

// Unsubscribe 取消一个客户端对某会话的订阅。
func (r *Registry) Unsubscribe(sessionID string, c *ClientConn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	subs, ok := r.sessionSubs[sessionID]
	if !ok {
		return
	}
	delete(subs, c.ID)
	if len(subs) == 0 {
		delete(r.sessionSubs, sessionID)
	}
}

// Subscribers 返回某会话当前的订阅者快照。
//
// 返回**副本**：调用方会在持锁之外遍历投递，直接返回内部 map
// 会在并发修改时 panic（Go 的 map 并发读写会直接 crash，不是数据竞争那么温和）。
func (r *Registry) Subscribers(sessionID string) []*ClientConn {
	r.mu.RLock()
	defer r.mu.RUnlock()

	subs := r.sessionSubs[sessionID]
	out := make([]*ClientConn, 0, len(subs))
	for _, c := range subs {
		out = append(out, c)
	}
	return out
}

// SubscriberCount 返回某会话的订阅者数量。
func (r *Registry) SubscriberCount(sessionID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessionSubs[sessionID])
}

// ---- 会话归属 ----

// SetSessionOwner 登记某会话属于哪个设备。
func (r *Registry) SetSessionOwner(sessionID, deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionOwner[sessionID] = deviceID
}

// SessionOwner 查某会话当前属于哪个设备。
func (r *Registry) SessionOwner(sessionID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.sessionOwner[sessionID]
	return d, ok
}

// ForgetSession 清除某会话的归属与订阅（会话结束时调用）。
func (r *Registry) ForgetSession(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessionOwner, sessionID)
	delete(r.sessionSubs, sessionID)
}

// QueueSize 返回配置的发送队列深度（供诊断接口展示）。
func (r *Registry) QueueSize() int { return r.queueSize }
