package agent

import (
	"testing"
	"time"
)

// TestBackoffNoOverflow 是位移溢出的回归测试。
//
// 如果用 `min << attempt` 实现，attempt 到 30 左右就会溢出。
// 而溢出是**静默**的：结果可能是负数（变成"立刻重连"，把 Server 打爆）
// 或零（time.NewTimer(0) 立即触发，形成忙循环）。
// 两种表现都不会报错，只会让重连逻辑变得诡异。
func TestBackoffNoOverflow(t *testing.T) {
	b := newBackoff(time.Second, 30*time.Second)

	for i := 0; i < 200; i++ {
		d := b.Next()
		if d <= 0 {
			t.Fatalf("第 %d 次退避 = %v（溢出或为零）", i, d)
		}
		// jitter 最多放大 1.5 倍。
		if max := time.Duration(float64(30*time.Second) * 1.5); d > max {
			t.Fatalf("第 %d 次退避 = %v，超过上限 %v", i, d, max)
		}
	}
}

func TestBackoffGrowsExponentially(t *testing.T) {
	b := newBackoff(time.Second, 1*time.Hour)

	// delay() 不带 jitter，可以直接断言。
	want := []time.Duration{1, 2, 4, 8, 16}
	for i, w := range want {
		if got := b.delay(); got != w*time.Second {
			t.Fatalf("第 %d 次基准间隔 = %v，期望 %v", i, got, w*time.Second)
		}
		b.Next()
	}
}

func TestBackoffCapsAtMax(t *testing.T) {
	b := newBackoff(time.Second, 4*time.Second)

	for i := 0; i < 100; i++ {
		b.Next()
	}
	if got := b.delay(); got != 4*time.Second {
		t.Errorf("长时间失败后基准间隔 = %v，期望封顶在 4s", got)
	}
}

// TestBackoffHasJitter 验证惊群保护确实生效。
//
// 没有 jitter 时，几百个 Agent 会在同一毫秒一起重连，
// 把刚起来的 Server 再次打挂，然后全体同步重试 ——
// 形成稳定的失败节律，永远不会自愈。
func TestBackoffHasJitter(t *testing.T) {
	// min == max，基准间隔恒为 10s，于是所有差异都来自 jitter。
	b := newBackoff(10*time.Second, 10*time.Second)

	seen := make(map[time.Duration]bool, 100)
	for i := 0; i < 100; i++ {
		seen[b.Next()] = true
	}
	if len(seen) < 50 {
		t.Errorf("100 次退避只得到 %d 个不同值 —— jitter 太弱或没生效", len(seen))
	}
}

// TestBackoffReset 验证重连成功后计数清零。
//
// 不重置的后果：一个跑了三个月、历史上失败过很多次的 Agent，
// 会带着累积的退避等级运行。某次短暂断网后要等满 30s 才重连，
// 而它其实一直很健康 —— 用户会觉得"这东西偶尔要卡半分钟"。
func TestBackoffReset(t *testing.T) {
	b := newBackoff(time.Second, 30*time.Second)

	for i := 0; i < 10; i++ {
		b.Next()
	}
	if b.Attempts() == 0 {
		t.Fatal("尝试计数没有增加")
	}

	b.Reset()
	if b.Attempts() != 0 {
		t.Errorf("Reset 后 Attempts = %d，期望 0", b.Attempts())
	}
	if got := b.delay(); got != time.Second {
		t.Errorf("Reset 后基准间隔 = %v，期望回到最小值 1s", got)
	}
}

func TestBackoffHandlesBadInput(t *testing.T) {
	// 非正的 min、小于 min 的 max 都不该产生 panic 或非法间隔。
	b := newBackoff(0, 0)
	if d := b.Next(); d <= 0 {
		t.Errorf("零值配置下退避 = %v", d)
	}

	b = newBackoff(10*time.Second, time.Second) // max < min
	if d := b.Next(); d <= 0 {
		t.Errorf("max<min 时退避 = %v", d)
	}
}
