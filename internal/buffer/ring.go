// Package buffer 提供会话输出用的定长环形缓冲。
//
// 设计要点（§7 / §25）：
//
//  1. **定长**：分配一次，之后零分配。终端输出是高频路径，
//     分块链表会带来持续的 GC 压力。
//
//  2. **单调序号**：Ring 维护一个只增不减的 Total（累计写入字节数）。
//     attach 时客户端带上自己的 lastSeq，就能只补差量，
//     而不是每次都重放整个 1 MB（§15.3）。
//
//  3. **永远可写**：Write 永不阻塞、永不返回错误。缓冲满了就覆盖最旧的数据。
//     这条是硬要求 —— 如果这里阻塞，PTY 读循环会被拖住，
//     进而把用户本地的 CLI 也卡死（R3）。
package buffer

import "sync"

// DefaultSize 是默认缓冲大小（1 MB），可用范围 256 KB - 10 MB（§7）。
const (
	DefaultSize = 1 << 20
	MinSize     = 256 << 10
	MaxSize     = 10 << 20
)

// Ring 是并发安全的定长环形缓冲。
//
// 并发模型：单写者（PTY 读循环）+ 多读者（各客户端的 attach/快照）。
// 用 RWMutex 而不是 atomic，是因为快照需要看到一致的切片视图。
type Ring struct {
	mu     sync.RWMutex
	buf    []byte
	size   int
	pos    int    // 下一个写入位置
	filled int    // 当前有效字节数（<= size）
	total  uint64 // 累计写入字节数，单调递增，永不重置
}

// New 创建一个容量为 size 字节的环形缓冲。
//
// size <= 0 时取 DefaultSize。
//
// ★ 这里【不做】256KB-10MB 的策略夹取 —— 那是配置层的职责（用 ClampSize）。
// 缓冲本身不该知道"多大算合理"，否则单元测试没法用小容量验证环绕逻辑。
func New(size int) *Ring {
	if size <= 0 {
		size = DefaultSize
	}
	return &Ring{
		buf:  make([]byte, size),
		size: size,
	}
}

// ClampSize 把配置里的容量夹到允许区间（§7：256 KB - 10 MB）。
// 供 config 层调用。
func ClampSize(size int) int {
	if size <= 0 {
		return DefaultSize
	}
	return clamp(size, MinSize, MaxSize)
}

// Size 返回容量。
func (r *Ring) Size() int { return r.size }

// Total 返回累计写入的字节数（即"下一个字节的序号"）。
//
// 客户端记录的就是这个值：下次 attach 时带上它，就能只拿增量。
func (r *Ring) Total() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.total
}

// Len 返回当前有效字节数。
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.filled
}

// Oldest 返回缓冲里最旧的那个字节的序号。
//
// 若 Oldest() > 客户端记录的 seq，说明中间的数据已经被覆盖，
// 客户端必须清屏后全量重放，而不是接着补。
func (r *Ring) Oldest() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.total - uint64(r.filled)
}

// Evicted 返回累计被覆盖掉的字节数。用于向用户提示"部分输出已丢弃"（R10）。
func (r *Ring) Evicted() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.total - uint64(r.filled)
}

// Write 写入数据并返回写入长度。
//
// 永不阻塞、永不失败：满了就覆盖最旧的数据。
// 实现 io.Writer，可以直接接 io.Copy。
//
// ★ 不变式：绝对序号为 a 的字节，物理下标恒为 a % size。
// 两个分支都必须维护它，否则 copyRange 会读到错位的数据。
// （这个不变式不是显然的 —— 第一版在超长写入分支上就写错了。）
func (r *Ring) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 单次写入 ≥ 容量：只保留最后 size 字节，前面无论如何都留不下。
	//
	// ★ 起点不是 r.pos，而是【写完之后】的 pos。
	//   维持不变式的推导：保留段的第一个字节，其绝对序号是 total+n-size；
	//   因为 n >= size，所以 total+n-size ≡ total+n (mod size)，
	//   于是它该落在 (total+n) % size —— 也就是写完之后的 pos。
	//   一开始写成"从 r.pos 开始写"，两者只在 n == size 时巧合相等，
	//   所以这个 bug 在大多数输入下都能躲过测试（见 TestRingOversizeWriteKeepsMapping）。
	if n >= r.size {
		kept := p[n-r.size:]
		r.total += uint64(n)
		r.pos = int(r.total % uint64(r.size))
		first := copy(r.buf[r.pos:], kept)
		copy(r.buf, kept[first:])
		r.filled = r.size
		return n, nil
	}

	// 先写 pos 到末尾，再绕回开头写剩下的
	first := copy(r.buf[r.pos:], p)
	if first < n {
		copy(r.buf, p[first:])
	}
	r.pos = (r.pos + n) % r.size
	r.filled += n
	if r.filled > r.size {
		r.filled = r.size
	}
	r.total += uint64(n)
	return n, nil
}

// Snapshot 返回缓冲里全部有效字节的副本。
func (r *Ring) Snapshot() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.copyRange(r.total-uint64(r.filled), r.total)
}

// Since 返回序号 seq 之后的全部字节副本。
//
// 返回值：
//   - data     —— 可用的字节
//   - truncated —— true 表示 seq 太旧、中间已有数据被覆盖，
//     调用方应清屏后按 Oldest() 重放，而不是接着拼
//
// 语义边界（与 §15.3 对齐）：
//
//	seq == 0          → 全量（等同 Snapshot）
//	seq >= Total()    → 空（客户端已经是最新的）
//	seq <  Oldest()   → 全量 + truncated=true
//	Oldest() <= seq < Total() → 精确增量
func (r *Ring) Since(seq uint64) (data []byte, truncated bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if seq == 0 {
		return r.copyRange(r.total-uint64(r.filled), r.total), false
	}
	if seq >= r.total {
		return nil, false
	}
	oldest := r.total - uint64(r.filled)
	if seq < oldest {
		return r.copyRange(oldest, r.total), true
	}
	return r.copyRange(seq, r.total), false
}

// Reset 清空缓冲。
//
// ★ 刻意不重置 total：序号一旦对外发布就必须单调，
// 否则客户端记录的 lastSeq 会在会话中途变得毫无意义。
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// ★ 只清 filled，既不动 pos 也不动 total。
	//   pos 必须恒等于 total % size，否则 copyRange 的"序号 → 物理下标"
	//   映射就会错位（写进去的数据读不出来）。
	//   total 更不能重置：序号一旦对外发布就必须单调。
	r.filled = 0
}

// copyRange 复制绝对序号区间 [from, to) 的字节。
// 调用方必须持有锁。
func (r *Ring) copyRange(from, to uint64) []byte {
	if from >= to {
		return nil
	}
	n := int(to - from)
	out := make([]byte, 0, n)

	// 绝对序号 → 环形下标
	start := int(from % uint64(r.size))
	remaining := n
	for remaining > 0 {
		chunk := min(r.size-start, remaining)
		out = append(out, r.buf[start:start+chunk]...)
		remaining -= chunk
		start = 0
	}
	return out
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
