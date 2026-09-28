package agent

import (
	"math/rand/v2"
	"time"
)

// backoff 是指数退避 + jitter（§3.2）。
//
// # 为什么要 jitter
//
// 没有 jitter 时，所有 Agent 的重连间隔完全一致。Server 重启后，
// 几百个 Agent 会在同一毫秒一起重连 —— 这就是惊群（thundering herd），
// 刚起来的 Server 会被瞬间打满，然后全体再次失败、再次同步重试，
// 形成稳定的失败节律，永远不会自愈。
//
// 加上 [0.5, 1.5) 的随机因子后，重试时刻被打散，Server 能逐步吸收。
//
// # 为什么上限是 30s 而不是"退到 5 分钟"
//
// 退避太激进会让恢复变得迟钝：Server 重启通常几秒就绪，
// 而用户看到的是"我的设备一直离线"。30s 是"不再打爆 Server"
// 和"用户不至于等太久"之间的折中。
type backoff struct {
	min, max time.Duration
	attempt  int
}

func newBackoff(min, max time.Duration) *backoff {
	if min <= 0 {
		min = DefaultReconnectMin
	}
	if max < min {
		max = min
	}
	return &backoff{min: min, max: max}
}

// Next 返回下一次重试前应等待的时长，并推进尝试计数。
func (b *backoff) Next() time.Duration {
	base := b.delay()
	b.attempt++

	// jitter: [0.5, 1.5) × base
	factor := 0.5 + rand.Float64()
	return time.Duration(float64(base) * factor)
}

// delay 计算未加 jitter 的基准间隔。
//
// 用循环而不是 `min << attempt`：位移在 attempt 较大时会溢出成负数
// 或零，而 `1s << 64` 这种溢出是**静默**的 —— 表现为"重连突然变得极快"
// 或"永远不再重连"，都不会报错。循环里显式判断上限更笨，但不会错。
func (b *backoff) delay() time.Duration {
	d := b.min
	for i := 0; i < b.attempt; i++ {
		if d >= b.max {
			return b.max
		}
		d *= 2
	}
	if d > b.max {
		return b.max
	}
	return d
}

// Reset 在连接成功后清零尝试计数。
//
// 必须重置：否则一个跑了三个月的 Agent 会带着几百次历史失败累积的
// 退避等级，某次短暂断网后要等满 30s 才重连 —— 而它其实一直很健康。
func (b *backoff) Reset() { b.attempt = 0 }

// Attempts 返回当前尝试次数（日志与测试用）。
func (b *backoff) Attempts() int { return b.attempt }
