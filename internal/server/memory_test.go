package server

import (
	"runtime"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// R6：空闲连接的内存上限
// ---------------------------------------------------------------------------

// memBudgetPerIdleConn 是每条空闲连接允许占用的堆内存上限（R6）。
//
// 这个数字不是拍出来的，它决定了 SendQueueSize 的取值：
//
//	队列深度 1024 × 每条消息引用约 24 字节  = 24 KB 只是队列本身，
//	再加实际排队的数据（平均 4 KB/条）→ 单连接上限 4 MB。
//	1000 条连接就是 4 GB —— 一台 8 GB 的机器直接 OOM。
//
// 降到 256 之后上限约 1 MB，而**实际占用取决于瞬时排队量**（空闲时接近 0）。
// 100 KB 的预算留了足够余量覆盖 goroutine 栈、读写缓冲、注册表索引。
const memBudgetPerIdleConn = 100 << 10 // 100 KB

// TestR6IdleConnectionMemory 实测 500 条空闲连接的平均堆占用。
//
// # 测量口径（诚实说明）
//
// 这里量的是 **HeapAlloc**，也就是 Go 堆上的字节。它覆盖了：
// 连接结构体、发送队列的底层数组、sessions/attached 两个 map、
// gorilla 的读写缓冲、注册表的四份索引。
//
// 它**不**覆盖 goroutine 栈（那算在 StackInuse 里，每条连接 2 个
// goroutine × 2 KB 起步）。所以真实占用比这个数字高约 8 KB/连接 ——
// 100 KB 的预算为这个差值留了余量。用 HeapAlloc 是因为它稳定可测；
// StackInuse 会随调度器复用栈而剧烈波动，做成断言必然 flaky。
//
// # 为什么必须实测而不是「看代码算」
//
// 每条连接的占用散落在五个地方：AgentConn/ClientConn 结构体本身、
// 发送队列（channel 的底层数组）、sessions/attached 两个 map、
// gorilla 的读缓冲与写缓冲、以及注册表的四份索引。
// 逐个加起来既容易漏，也容易算错 —— 而漏掉的往往是占比最大的那个。
//
// 更关键的是：**代码演进会悄悄破坏这个预算**。有人把队列调回 1024、
// 或者给 ClientConn 加一个 per-connection 的大 map，功能测试全绿，
// 内存却在部署之后才爆。这条测试就是那个哨兵。
func TestR6IdleConnectionMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("内存实测在 -short 模式下跳过")
	}

	e := newTestEnv(t)
	token := e.signup(t, "jojo@example.com", "correct-horse-battery")

	// ★ 放开 WS 建连限流。
	//
	// 生产配置是 60/分钟、突发 30，而 500 条连接全部来自 127.0.0.1 ——
	// 限流器会正确地把它挡在 30 条。那是它该做的事，
	// 但会让这条测试测不到内存。这里只是把闸门开大，不是取消它
	// （TestSendQueueSizeIsBounded 之外还有专门的限流测试）。
	e.srv.wsLimit = NewRateLimiter(1e6, 1e6, e.clock.Now)

	const conns = 500

	// ---- 基线：先建 20 条，让各种一次性分配（连接池、TLS 状态等）
	//      先发生掉，避免它们被摊进「每条连接」的成本里 ----
	warmup := make([]*websocket.Conn, 0, 20)
	for range 20 {
		ws := e.openClientWS(t, token)
		warmup = append(warmup, ws)
	}
	for _, ws := range warmup {
		_ = ws.Close()
	}
	// 关闭是异步的，等一下让服务端的清理协程跑完。
	waitFor(t, func() bool { return e.srv.Registry().ClientCount() == 0 })

	base := measureHeap(t)

	// ---- 建立 500 条空闲连接 ----
	conns500 := make([]*websocket.Conn, 0, conns)
	for range conns {
		conns500 = append(conns500, e.openClientWS(t, token))
	}
	defer func() {
		for _, ws := range conns500 {
			_ = ws.Close()
		}
	}()

	waitFor(t, func() bool { return e.srv.Registry().ClientCount() == conns })

	used := measureHeap(t)
	perConn := (used - base) / conns

	t.Logf("500 条空闲连接：总堆增量 %.2f MB，平均每条 %d 字节（预算 %d 字节）",
		float64(used)/(1<<20), perConn, memBudgetPerIdleConn)

	// 负值说明测量被 GC 时机或后台分配干扰了。这时给出警告而不是失败 ——
	// 一个会随机失败的测试比没有测试更糟（团队会习惯性地重跑它）。
	if perConn <= 0 {
		t.Skipf("堆测量结果为 %d 字节/连接（负值或零），说明噪声压过了信号；"+
			"本次不判定，请在有 -count=1 且机器空闲时重跑", perConn)
	}

	if perConn > memBudgetPerIdleConn {
		t.Fatalf("空闲连接占用 %d 字节/条，超出预算 %d 字节。\n"+
			"最可能的原因：SendQueueSize 被调大了，或给连接结构体加了 per-connection 的大字段。\n"+
			"1000 条连接时这个超支会放大 1000 倍。",
			perConn, memBudgetPerIdleConn)
	}
}

// openClientWS 用一张新票据建立一条浏览器 WS 连接。
func (e *testEnv) openClientWS(t *testing.T, token string) *websocket.Conn {
	t.Helper()

	resp := e.post(t, "/api/v1/ws-ticket", token, nil)
	if resp.Status != 200 {
		t.Fatalf("申请票据失败: %d %s", resp.Status, resp.Body)
	}
	ticket := resp.Str(t, "ticket")

	ws, _ := e.wsDial(t, "/api/v1/ws/client?ticket="+ticket, nil)
	if ws == nil {
		t.Fatal("建立浏览器 WS 失败")
	}
	return ws
}

// measureHeap 返回当前存活堆的字节数。
//
// ★ 连续两次 GC 再读：第一次回收可达对象，第二次把第一次产生的
// 终结器/清扫残留也收掉。只做一次 GC 的话，ReadMemStats 会稳定地
// 多报一截 —— 而那一截会被摊到每条连接上，让结果偏高。
func measureHeap(t *testing.T) uint64 {
	t.Helper()
	runtime.GC()
	runtime.GC()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// 给异步的清理协程一点时间落定，避免把「刚关闭的连接」算进基线。
	time.Sleep(20 * time.Millisecond)
	runtime.GC()

	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// TestSendQueueSizeIsBounded 把 R6 的**结构性**约束单独钉住。
//
// 上面那条测试测的是「结果」，这一条测的是「原因」——
// 当队列深度被改大时，它会直接指出问题所在，而不是给一个
// 「每条连接多占了 N 字节」的间接线索。
func TestSendQueueSizeIsBounded(t *testing.T) {
	// 256 是 R6 的结论值。调大它之前请先重跑 TestR6IdleConnectionMemory
	// 并确认 500 条连接的实测值仍在预算内。
	const want = 256

	if defaultSendQueueSize != want {
		t.Fatalf("defaultSendQueueSize = %d，R6 要求 %d。\n"+
			"调大队列会按「连接数 × 深度 × 消息大小」放大内存："+
			"深度 1024 时 1000 条连接的上限是 4 GB。",
			defaultSendQueueSize, want)
	}

	// 配置里显式给的值也必须被尊重（而不是被默认值覆盖）。
	e := newTestEnv(t)
	if got := e.srv.Registry().QueueSize(); got != want {
		t.Fatalf("Registry.QueueSize() = %d，期望 %d（来自 Config.SendQueueSize）", got, want)
	}

	// 非法值（0 / 负数）必须回落到默认值，而不是造出一个 0 容量的 channel ——
	// 那会让每一次 TrySend 都失败，表现为「所有消息都被丢弃」。
	reg := NewRegistry(0)
	if got := reg.QueueSize(); got != want {
		t.Fatalf("QueueSize(0) = %d，期望回落到默认值 %d", got, want)
	}
	reg = NewRegistry(-1)
	if got := reg.QueueSize(); got != want {
		t.Fatalf("QueueSize(-1) = %d，期望回落到默认值 %d", got, want)
	}
}
