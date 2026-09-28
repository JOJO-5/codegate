package buffer

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestRingBasic(t *testing.T) {
	r := New(64)
	if _, err := r.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if r.Total() != 5 {
		t.Errorf("Total = %d, 期望 5", r.Total())
	}
	if r.Len() != 5 {
		t.Errorf("Len = %d, 期望 5", r.Len())
	}
	if r.Oldest() != 0 {
		t.Errorf("Oldest = %d, 期望 0", r.Oldest())
	}
	if got := string(r.Snapshot()); got != "hello" {
		t.Errorf("Snapshot = %q", got)
	}
}

func TestRingSince(t *testing.T) {
	r := New(64)
	r.Write([]byte("0123456789")) // Total=10, Oldest=0

	tests := []struct {
		name          string
		seq           uint64
		want          string
		wantTruncated bool
	}{
		{"seq=0 要全量", 0, "0123456789", false},
		{"seq 在中间", 4, "456789", false},
		{"seq 正好是最旧", 0, "0123456789", false},
		{"seq 等于 Total 表示已最新", 10, "", false},
		{"seq 超过 Total", 99, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, trunc := r.Since(tt.seq)
			if string(got) != tt.want {
				t.Errorf("Since(%d) = %q, 期望 %q", tt.seq, got, tt.want)
			}
			if trunc != tt.wantTruncated {
				t.Errorf("Since(%d) truncated = %v, 期望 %v", tt.seq, trunc, tt.wantTruncated)
			}
		})
	}
}

// 环绕后序号到物理下标的映射必须仍然正确。
func TestRingWrap(t *testing.T) {
	r := New(8)
	r.Write([]byte("abcdef")) // pos=6, total=6
	r.Write([]byte("ghij"))   // 环绕：写满并覆盖前两个
	// 现在应保留最后 8 个字节 "cdefghij"
	if r.Total() != 10 {
		t.Fatalf("Total = %d, 期望 10", r.Total())
	}
	if r.Oldest() != 2 {
		t.Fatalf("Oldest = %d, 期望 2", r.Oldest())
	}
	if got := string(r.Snapshot()); got != "cdefghij" {
		t.Fatalf("Snapshot = %q, 期望 cdefghij", got)
	}
	// 差量读：从序号 2 开始应拿到全部有效数据
	if got, _ := r.Since(2); string(got) != "cdefghij" {
		t.Errorf("Since(2) = %q", got)
	}
	// 从序号 5 开始应拿到 "fghij"
	if got, _ := r.Since(5); string(got) != "fghij" {
		t.Errorf("Since(5) = %q, 期望 fghij", got)
	}
}

// ★ 回归测试：单次写入超过容量时，物理下标映射曾经写错。
//
// 第一版把保留的数据 copy 到 buf[0]，但 pos 没跟着走，
// 导致「物理下标 = 绝对序号 % 容量」这个不变式被破坏，
// 表现为 Since() 返回错位的数据（"cdefghcd" 而不是 "cdefghij"）。
func TestRingOversizeWriteKeepsMapping(t *testing.T) {
	r := New(8)
	n, err := r.Write([]byte("abcdefghij")) // 10 字节写进 8 字节的环
	if err != nil || n != 10 {
		t.Fatalf("Write 返回 %d, %v", n, err)
	}
	if r.Total() != 10 {
		t.Errorf("Total = %d, 期望 10（total 记的是调用方写入的字节数，不是保留的）", r.Total())
	}
	if r.Oldest() != 2 {
		t.Errorf("Oldest = %d, 期望 2", r.Oldest())
	}
	if got := string(r.Snapshot()); got != "cdefghij" {
		t.Fatalf("Snapshot = %q, 期望 cdefghij", got)
	}
	if got, _ := r.Since(2); string(got) != "cdefghij" {
		t.Errorf("Since(2) = %q, 期望 cdefghij", got)
	}
	if got, _ := r.Since(7); string(got) != "hij" {
		t.Errorf("Since(7) = %q, 期望 hij", got)
	}
}

// 被覆盖的区间必须如实报告 truncated，客户端据此决定清屏重放。
func TestRingTruncated(t *testing.T) {
	r := New(8)
	r.Write([]byte("0123456789")) // 保留 "23456789"，Oldest=2

	got, trunc := r.Since(1) // 序号 1 已经被覆盖
	if !trunc {
		t.Fatal("seq 早于 Oldest 时必须报 truncated")
	}
	if string(got) != "23456789" {
		t.Errorf("truncated 时应返回全部可用数据，实际 %q", got)
	}

	// 边界：正好等于 Oldest 不算截断
	if _, trunc := r.Since(2); trunc {
		t.Error("seq 等于 Oldest 不该算截断")
	}
}

func TestRingEvicted(t *testing.T) {
	r := New(8)
	r.Write([]byte("0123456789"))
	if r.Evicted() != 2 {
		t.Errorf("Evicted = %d, 期望 2", r.Evicted())
	}
}

// Reset 清空数据但【不重置序号】—— 序号一旦对外发布就必须单调。
func TestRingResetKeepsSeqMonotonic(t *testing.T) {
	r := New(64)
	r.Write([]byte("hello"))
	before := r.Total()
	r.Reset()
	if r.Len() != 0 {
		t.Errorf("Reset 后 Len = %d, 期望 0", r.Len())
	}
	if r.Total() != before {
		t.Errorf("★ Reset 把 Total 从 %d 改成了 %d —— 这会破坏客户端的 lastSeq",
			before, r.Total())
	}
	// 重置后还能继续用
	r.Write([]byte("world"))
	if got := string(r.Snapshot()); got != "world" {
		t.Errorf("Reset 后写入异常: %q", got)
	}
}

func TestRingEmptyReads(t *testing.T) {
	r := New(64)
	if got := r.Snapshot(); got != nil {
		t.Errorf("空缓冲 Snapshot 应为 nil，实际 %q", got)
	}
	if got, trunc := r.Since(0); got != nil || trunc {
		t.Errorf("空缓冲 Since(0) 应为 (nil,false)，实际 (%q,%v)", got, trunc)
	}
	if n, err := r.Write(nil); n != 0 || err != nil {
		t.Errorf("写空切片应为 (0,nil)，实际 (%d,%v)", n, err)
	}
}

// 连续大量小写入后，快照必须与"最后 size 字节"完全一致。
func TestRingFuzzAgainstReference(t *testing.T) {
	const size = 32
	r := New(size)

	var ref []byte // 参考实现：一个无界切片
	for i := 0; i < 500; i++ {
		chunk := []byte(strings.Repeat(string(rune('a'+i%26)), 1+i%7))
		r.Write(chunk)
		ref = append(ref, chunk...)

		want := ref
		if len(want) > size {
			want = want[len(want)-size:]
		}
		if got := r.Snapshot(); !bytes.Equal(got, want) {
			t.Fatalf("第 %d 次写入后不一致\n got %q\nwant %q", i, got, want)
		}
		if r.Total() != uint64(len(ref)) {
			t.Fatalf("第 %d 次写入后 Total=%d, 期望 %d", i, r.Total(), len(ref))
		}
	}
}

// 差量读取的语义必须和参考实现一致。
func TestRingSinceMatchesReference(t *testing.T) {
	const size = 24
	r := New(size)
	var ref []byte

	for i := 0; i < 200; i++ {
		chunk := []byte(fmt.Sprintf("[%03d]", i))
		r.Write(chunk)
		ref = append(ref, chunk...)

		total := uint64(len(ref))
		oldest := uint64(0)
		if len(ref) > size {
			oldest = total - size
		}

		for _, seq := range []uint64{0, oldest, total, total - 1, total + 5} {
			if total == 0 {
				continue
			}
			got, trunc := r.Since(seq)

			var wantFrom uint64
			wantTrunc := false
			switch {
			case seq == 0:
				wantFrom = oldest
			case seq >= total:
				wantFrom = total
			case seq < oldest:
				wantFrom = oldest
				wantTrunc = true
			default:
				wantFrom = seq
			}
			want := ref[wantFrom:]

			if !bytes.Equal(got, want) {
				t.Fatalf("i=%d seq=%d\n got %q\nwant %q", i, seq, got, want)
			}
			if trunc != wantTrunc {
				t.Fatalf("i=%d seq=%d truncated=%v, 期望 %v", i, seq, trunc, wantTrunc)
			}
		}
	}
}

// 并发：单写者 + 多读者。必须跑 -race。
func TestRingConcurrent(t *testing.T) {
	r := New(4096)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			r.Write([]byte(fmt.Sprintf("chunk-%04d;", i)))
		}
	}()

	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				switch i % 4 {
				case 0:
					_ = r.Snapshot()
				case 1:
					_, _ = r.Since(uint64(i))
				case 2:
					_ = r.Total()
					_ = r.Oldest()
				case 3:
					_ = r.Len()
				}
			}
		}(k)
	}
	wg.Wait()

	if r.Len() > r.Size() {
		t.Fatalf("Len=%d 超过容量 %d", r.Len(), r.Size())
	}
}

func TestClampSize(t *testing.T) {
	if got := ClampSize(0); got != DefaultSize {
		t.Errorf("ClampSize(0) = %d", got)
	}
	if got := ClampSize(1); got != MinSize {
		t.Errorf("ClampSize(1) = %d, 期望 %d", got, MinSize)
	}
	if got := ClampSize(1 << 30); got != MaxSize {
		t.Errorf("ClampSize(1GB) = %d, 期望 %d", got, MaxSize)
	}
	if got := ClampSize(1 << 20); got != 1<<20 {
		t.Errorf("ClampSize(1MB) = %d, 不该被改动", got)
	}
}
