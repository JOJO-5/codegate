package server

import (
	"sync"
	"time"
)

// RateLimiter 是按键隔离的令牌桶。
//
// 「按键隔离」是重点：全局一个桶的话，一个攻击者就能把所有人的配额吃光
// （这本身就是一种 DoS）。按 email + IP 分开计数，攻击者只能锁死自己。
//
// 用令牌桶而不是固定窗口计数器：固定窗口在窗口边界会有 2 倍突发
// （窗口末尾打满 + 下个窗口开头再打满），对登录爆破这种场景不够。
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket

	rate  float64 // 每秒补充多少令牌
	burst float64 // 桶容量（允许的瞬时突发）

	now func() time.Time

	// gcEvery / gcIdle 控制惰性 GC：没有 GC 的话，攻击者用海量
	// 随机 email 打过来就能把 buckets map 撑爆（内存泄漏式 DoS）。
	gcEvery time.Duration
	gcIdle  time.Duration
	lastGC  time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter 构造限流器。
//
//	ratePerSecond：每秒补充的令牌数（例如 5 次/分钟 → 5.0/60）
//	burst：        桶容量，也是允许的瞬时突发量
func NewRateLimiter(ratePerSecond, burst float64, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	if ratePerSecond <= 0 {
		ratePerSecond = 1
	}
	if burst <= 0 {
		burst = 1
	}
	return &RateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    ratePerSecond,
		burst:   burst,
		now:     now,
		gcEvery: 10 * time.Minute,
		gcIdle:  30 * time.Minute,
	}
}

// Allow 消耗一个令牌。返回 false 表示被限流。
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.maybeGC(now)

	b, ok := l.buckets[key]
	if !ok {
		// 新键：满桶。第一个请求永远放行 —— 否则「新用户第一次登录就被拒」
		// 会是个很难排查的问题。
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Reset 清掉某个键的计数（例如登录成功后）。
func (l *RateLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// Len 返回当前跟踪的键数量（诊断用）。
func (l *RateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// maybeGC 惰性清理长期不活跃的桶。调用方必须持锁。
func (l *RateLimiter) maybeGC(now time.Time) {
	if now.Sub(l.lastGC) < l.gcEvery {
		return
	}
	l.lastGC = now
	for k, b := range l.buckets {
		// 桶已满 = 这个键已经完全恢复，删掉不影响限流行为。
		if b.tokens >= l.burst && now.Sub(b.last) > l.gcIdle {
			delete(l.buckets, k)
		}
	}
}

// ---------------------------------------------------------------------------

// FailureGuard 实现「连续失败 N 次 → 锁定 M 分钟」（§10.4 的配对码防护）。
//
// 与 RateLimiter 的区别：限流限制的是**速率**，FailureGuard 限制的是
// **结果**。5 次/分钟的限流挡不住「每分钟试 5 次、试一年」的耐心攻击，
// 而连续失败锁定能让这条路径在 10 次之后彻底关闭。
type FailureGuard struct {
	mu     sync.Mutex
	states map[string]*failState

	maxFailures int
	lockFor     time.Duration
	now         func() time.Time
}

type failState struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

// NewFailureGuard 构造守卫。
//
//	maxFailures：连续失败多少次后锁定
//	lockFor：    锁定时长
func NewFailureGuard(maxFailures int, lockFor time.Duration, now func() time.Time) *FailureGuard {
	if now == nil {
		now = time.Now
	}
	if maxFailures <= 0 {
		maxFailures = 10
	}
	if lockFor <= 0 {
		lockFor = 30 * time.Minute
	}
	return &FailureGuard{
		states:      make(map[string]*failState),
		maxFailures: maxFailures,
		lockFor:     lockFor,
		now:         now,
	}
}

// Locked 判断某个键当前是否被锁定。
func (g *FailureGuard) Locked(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	st, ok := g.states[key]
	if !ok {
		return false
	}
	if st.lockedUntil.IsZero() {
		return false
	}
	if g.now().Before(st.lockedUntil) {
		return true
	}
	// 锁定期已过：清掉记录，给用户重新开始的机会。
	// 不保留失败计数是刻意的 —— 否则「解封后失败一次又立刻被锁」。
	delete(g.states, key)
	return false
}

// RecordFailure 记录一次失败，必要时锁定。
func (g *FailureGuard) RecordFailure(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	g.gcLocked(now)

	st, ok := g.states[key]
	if !ok {
		st = &failState{}
		g.states[key] = st
	}
	st.failures++
	st.lastFailure = now

	if st.failures >= g.maxFailures {
		st.lockedUntil = now.Add(g.lockFor)
	}
}

// Reset 清除某个键的失败记录（成功时调用）。
func (g *FailureGuard) Reset(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.states, key)
}

// LockRemaining 返回剩余锁定时长；未锁定返回 0。
func (g *FailureGuard) LockRemaining(key string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()

	st, ok := g.states[key]
	if !ok || st.lockedUntil.IsZero() {
		return 0
	}
	if d := st.lockedUntil.Sub(g.now()); d > 0 {
		return d
	}
	return 0
}

// Len 返回当前跟踪的键数量（诊断用）。
func (g *FailureGuard) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.states)
}

// gcLocked 清理早已过期的失败记录。调用方必须持锁。
func (g *FailureGuard) gcLocked(now time.Time) {
	for k, st := range g.states {
		// 锁定中的条目不能清 —— 清了等于提前解封。
		if !st.lockedUntil.IsZero() && now.Before(st.lockedUntil) {
			continue
		}
		// 失败记录保留 2 倍锁定时长，超过就认为这个键不再活跃。
		if now.Sub(st.lastFailure) > 2*g.lockFor {
			delete(g.states, k)
		}
	}
}
