package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jojo/codegate/internal/buffer"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/terminal"
)

// Manager 层的哨兵错误。
var (
	// ErrLimitReached 表示达到会话数上限（§35）。
	ErrLimitReached = errors.New("session: 达到会话数上限")
	// ErrNotFound 表示会话不存在。
	ErrNotFound = errors.New("session: 会话不存在")
	// ErrManagerClosed 表示 Manager 已经关闭（优雅关闭中）。
	ErrManagerClosed = errors.New("session: Manager 已关闭")
)

// Factory 创建**并启动**一个 PTY。
//
// 之所以把"启动"也交给 Factory，是因为不同平台（ConPTY / Unix PTY）
// 的创建与启动是紧耦合的（句柄、属性列表、进程属性），拆开反而更别扭。
// Manager 只负责"拿到一个已经在跑的 Terminal"。
//
// 测试时注入一个假实现即可，不需要真的起进程。
type Factory func(ctx context.Context, cfg terminal.StartConfig) (terminal.Terminal, error)

// Config 是 Manager 的运行参数。
type Config struct {
	// BufferSize 是每个会话的 ring buffer 大小。会被夹到
	// [buffer.MinSize, buffer.MaxSize]，非法值取默认值。
	BufferSize int
	// MaxSessions 是单设备（其实是单 Manager 实例）的会话数上限。
	MaxSessions int
	// ExitedRetention 是已退出会话在内存里的保留时长。
	//
	// 保留是为了让用户能 attach 回去看最后的输出（不变量 I3）——
	// 点错了或者想回看编译错误时，直接报"会话不存在"是很差的体验。
	ExitedRetention time.Duration
}

func (c Config) withDefaults() Config {
	if c.BufferSize <= 0 {
		c.BufferSize = buffer.DefaultSize
	}
	c.BufferSize = buffer.ClampSize(c.BufferSize)
	if c.MaxSessions <= 0 {
		c.MaxSessions = 20
	}
	if c.ExitedRetention <= 0 {
		c.ExitedRetention = 5 * time.Minute
	}
	return c
}

// CreateRequest 是创建一个会话所需的全部输入。
//
// ★ 这里没有"是否允许"的判断：命令白名单、cwd 白名单、环境变量构造
// 全部由 Agent 层在调用之前完成（§21.1、§18）。
// Manager 只负责资源与生命周期，不负责安全策略 —— 职责单一。
type CreateRequest struct {
	DeviceID uuid.UUID
	UserID   uuid.UUID
	Name     string
	Command  string
	Args     []string
	Cwd      string
	Env      []string
	Cols     uint16
	Rows     uint16
}

// Manager 持有本机的全部终端会话。
//
// 并发模型：map + RWMutex。会话数量上限是几十个，锁竞争可以忽略，
// 不值得为它上 shard 或 sync.Map（规范第 40 条：不要过度工程化）。
type Manager struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]*Session
	factory  Factory
	cfg      Config
	now      func() time.Time // 测试可注入
	closed   bool
}

// NewManager 创建一个 Manager。factory 为 nil 时 Create 会直接报错。
func NewManager(factory Factory, cfg Config) *Manager {
	return &Manager{
		sessions: make(map[uuid.UUID]*Session),
		factory:  factory,
		cfg:      cfg.withDefaults(),
		now:      time.Now,
	}
}

// Config 返回生效后的配置（含默认值），便于日志与 doctor 输出。
func (m *Manager) Config() Config { return m.cfg }

// Create 启动一个新会话。
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Session, error) {
	if m.factory == nil {
		return nil, errors.New("session: Manager 未配置 Factory")
	}

	// 先做一次粗粒度检查，避免在明显超限时还去创建 PTY（创建是有成本的）。
	m.mu.RLock()
	closed := m.closed
	n := len(m.sessions)
	m.mu.RUnlock()
	if closed {
		return nil, ErrManagerClosed
	}
	if n >= m.cfg.MaxSessions {
		return nil, fmt.Errorf("%w: 已有 %d 个，上限 %d", ErrLimitReached, n, m.cfg.MaxSessions)
	}

	startCfg := terminal.StartConfig{
		Command: req.Command,
		Args:    req.Args,
		Dir:     req.Cwd,
		Env:     req.Env,
		Cols:    req.Cols,
		Rows:    req.Rows,
	}

	pty, err := m.factory(ctx, startCfg)
	if err != nil {
		return nil, fmt.Errorf("session: 创建 PTY 失败: %w", err)
	}

	s := newSession(ctx, req, pty, m.cfg.BufferSize, m.now())

	// 插入时再检查一次：上面的检查与这里之间可能有并发创建。
	// 这是"检查-使用"竞态的经典修法 —— 判断和插入必须在同一把锁里。
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = pty.Close()
		return nil, ErrManagerClosed
	}
	if len(m.sessions) >= m.cfg.MaxSessions {
		m.mu.Unlock()
		_ = pty.Close()
		return nil, fmt.Errorf("%w: 上限 %d", ErrLimitReached, m.cfg.MaxSessions)
	}
	m.sessions[s.ID] = s
	m.mu.Unlock()

	return s, nil
}

// Get 按 ID 取会话。
func (m *Manager) Get(id uuid.UUID) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// MustGet 是 Get 的便捷版，不存在时返回 ErrNotFound。
func (m *Manager) MustGet(id uuid.UUID) (*Session, error) {
	if s, ok := m.Get(id); ok {
		return s, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// List 返回全部会话。顺序不保证。
func (m *Manager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// ListByDevice 返回属于某台设备的会话。
//
// Server 侧对账（session.sync）和权限校验都要用它。
func (m *Manager) ListByDevice(deviceID uuid.UUID) []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.DeviceID == deviceID {
			out = append(out, s)
		}
	}
	return out
}

// Count 返回当前会话数。
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// Snapshot 生成全部会话的对外快照，用于 session.sync / session.list。
func (m *Manager) Snapshot() []protocol.SessionSummary {
	sessions := m.List()
	out := make([]protocol.SessionSummary, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Summary())
	}
	return out
}

// Close 关闭指定会话并从注册表移除。
func (m *Manager) Close(id uuid.UUID, reason string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s.Close(reason)
}

// CloseAll 关闭全部会话。用于 Agent 优雅退出。
//
// 返回出错的数量：单个会话关不掉不应该阻断整个关闭流程，
// 但必须让调用方知道有几个没关干净。
func (m *Manager) CloseAll(reason string) (failed int) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[uuid.UUID]*Session)
	m.closed = true
	m.mu.Unlock()

	for _, s := range sessions {
		if err := s.Close(reason); err != nil {
			failed++
		}
	}
	return failed
}

// Reap 回收已结束且超过保留期的会话。
//
// 由 Agent 起一个定时器周期调用（比如每 1 分钟）。
// 返回被回收的数量，便于打日志。
func (m *Manager) Reap() int {
	cutoff := m.now().Add(-m.cfg.ExitedRetention)

	m.mu.Lock()
	defer m.mu.Unlock()

	reaped := 0
	for id, s := range m.sessions {
		s.mu.Lock()
		ended := !s.endedAt.IsZero() && s.endedAt.Before(cutoff)
		s.mu.Unlock()
		if ended {
			delete(m.sessions, id)
			reaped++
		}
	}
	return reaped
}

// MarkOrphaned 把指定设备的全部"还活着"的会话标记为已终止。
//
// ★ 它的用途很具体：Agent 重启后调用（§7.4）。
//
// Agent 进程死了，ConPTY/PTY 句柄随之关闭，子进程被回收 ——
// **没有可靠的跨进程 PTY 继承方案**（Windows 上尤其不可能）。
// 所以重启后必须把所有旧的 running/detached 会话如实标记为 terminated，
// 而不是让 UI 显示一个永远 attach 不上的"幽灵会话"。
//
// 返回被标记的数量。
func (m *Manager) MarkOrphaned(deviceID uuid.UUID, reason string) int {
	targets := m.ListByDevice(deviceID)
	n := 0
	for _, s := range targets {
		s.mu.Lock()
		if s.proc.Alive() {
			s.proc = ProcTerminated
			s.exitReason = reason
			s.endedAt = m.now()
			n++
		}
		s.mu.Unlock()
	}
	return n
}
