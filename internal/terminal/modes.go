package terminal

import (
	"bytes"
	"sort"
	"strconv"
	"sync"
)

// ModeTracker 跟踪一个会话输出流里当前处于 ON 状态的终端模式。
//
// # 为什么需要它
//
// R1 实测发现（docs/PHASE0-R1-CONPTY-FINDINGS.md §5）：三个目标 CLI 的
// 模式设置全部集中在启动头 300 字节内 ——
//
//	@  300  SET  备用屏幕+清屏(1049)
//	@  315  SET  字素簇处理(2027)
//	@  323  SET  括号粘贴(2004)
//	@  331  SET  鼠标-点击(1000)
//	...
//
// 用户断线 10 分钟后 attach，浏览器里是一个全新的终端实例：它在主缓冲区，
// 而应用一直在备用缓冲区里画。ring buffer 里早就没有 ?1049h 那一条了
// （1 MB 缓冲，启动头那 300 字节瞬间就被覆盖）。结果画面直接乱掉。
//
// # 它为什么不算"解析 CLI 业务内容"
//
// 它只记录模式的【存在性】，从不理解内容、不修改字节、不重排序列、
// 不区分是哪个 CLI。跟踪终端状态是传输层的必要职责 ——
// 与"理解 Claude Code 在说什么"是两件完全不同的事（§43 原则的注脚）。
//
// 并发安全：内部有锁，可从读循环直接调用。
type ModeTracker struct {
	mu sync.Mutex

	// modes 是当前处于 ON 状态的 DEC 私有模式。
	modes map[uint16]struct{}

	// extras 保存除 DEC 私有模式外、同样属于"终端状态"的设置序列。
	// key 是去重标识（如 "xtmodkeys"），value 是可直接重放的原始字节。
	extras map[string][]byte

	// ---- 解析状态机（只认 ESC [ ... <final>，其余一律跳过）----
	st  state
	csi []byte
}

type state uint8

const (
	stNormal state = iota
	stEsc
	stCsi
)

// maxCSILen 是单条 CSI 序列的最大长度。超过就放弃这条 ——
// 防止畸形输入把缓冲撑大（这是纯内存安全考量，与业务无关）。
const maxCSILen = 64

// xtmodkeysKey 是 XTMODKEYS 序列在 extras 里的 key。
//
// CSI > 4 ; Pv m 会改变终端的按键编码方式（OpenCode 实测会发）。
// 重新 attach 时如果不同步这一条，键盘输入可能就不对了。
const xtmodkeysKey = "xtmodkeys"

// preambleSkip 是不参与前导序列重放的模式。
var preambleSkip = map[uint16]struct{}{
	// 同步输出：★ 刻意不重放。
	//
	// 若重放时把它打开，而配对的 ?2026l 恰好落在 ring buffer 已被覆盖的
	// 区间里，客户端会永远停在同步模式 —— 表现是画面完全冻住，
	// 什么都不渲染。这是低概率但只在重连路径复现的严重 bug。
	// attach 流程的最后一步会无条件补一个 ?2026l 来兜底。
	2026: {},
}

// NewModeTracker 创建一个空的跟踪器。
func NewModeTracker() *ModeTracker {
	return &ModeTracker{
		modes:  make(map[uint16]struct{}, 8),
		extras: make(map[string][]byte, 2),
	}
}

// Scan 增量扫描一段输出，更新内部状态。
//
// 高频路径：只做字节状态机，无分配（除 CSI 累积缓冲在首次使用时增长一次）。
func (m *ModeTracker) Scan(p []byte) {
	if len(p) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, b := range p {
		switch m.st {
		case stNormal:
			if b == 0x1B {
				m.st = stEsc
			}

		case stEsc:
			switch b {
			case '[':
				m.st = stCsi
				m.csi = m.csi[:0]
			case 0x1B:
				// ESC ESC：重新开始，保持在 stEsc
			default:
				// 两字节转义（ESC 7 / ESC 8 / ESC ( B ...）：与模式无关
				m.st = stNormal
			}

		case stCsi:
			if b == 0x1B {
				// 序列未终结就来了新的 ESC：放弃旧的，重新开始
				m.st = stEsc
				continue
			}
			// CSI 的终结字节是 0x40-0x7E；参数是 0x30-0x3F，中间是 0x20-0x2F
			if b >= 0x40 && b <= 0x7E {
				m.csi = append(m.csi, b)
				m.handleCSI(m.csi)
				m.st = stNormal
				continue
			}
			if len(m.csi) < maxCSILen {
				m.csi = append(m.csi, b)
			} else {
				m.st = stNormal // 超长，放弃这条
			}
		}
	}
}

// handleCSI 处理一条完整的 CSI 序列（不含 ESC [ 前缀，含终结字节）。
// 调用方必须已持有锁。
func (m *ModeTracker) handleCSI(seq []byte) {
	if len(seq) < 2 {
		return
	}
	final := seq[len(seq)-1]
	params := seq[:len(seq)-1]

	// ---- DEC 私有模式：CSI ? P1 ; P2 ; ... h|l ----
	if params[0] == '?' {
		if final != 'h' && final != 'l' {
			return
		}
		on := final == 'h'
		for _, field := range bytes.Split(params[1:], []byte(";")) {
			if len(field) == 0 {
				continue
			}
			n, err := strconv.ParseUint(string(field), 10, 16)
			if err != nil {
				continue
			}
			if on {
				m.modes[uint16(n)] = struct{}{}
			} else {
				delete(m.modes, uint16(n))
			}
		}
		return
	}

	// ---- XTMODKEYS：CSI > 4 ; Pv m ----
	if params[0] == '>' && final == 'm' {
		fields := bytes.Split(params[1:], []byte(";"))
		if len(fields) == 2 && string(fields[0]) == "4" {
			if string(fields[1]) == "0" {
				delete(m.extras, xtmodkeysKey)
			} else {
				// 重建完整序列以便原样重放
				full := make([]byte, 0, len(seq)+2)
				full = append(full, 0x1B, '[')
				full = append(full, seq...)
				m.extras[xtmodkeysKey] = full
			}
		}
	}
}

// On 判断某个模式当前是否为 ON。
func (m *ModeTracker) On(mode uint16) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.modes[mode]
	return ok
}

// Active 返回当前所有 ON 的 DEC 私有模式，升序。
func (m *ModeTracker) Active() []uint16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]uint16, 0, len(m.modes))
	for mode := range m.modes {
		out = append(out, mode)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// InAltScreen 是 AltScreen() 的便捷判断。
func (m *ModeTracker) InAltScreen() bool { return m.On(1049) }

// AltScreen 返回是否处于备用屏幕。名字更直白，供上层阅读。
func (m *ModeTracker) AltScreen() bool { return m.On(1049) }

// Preamble 生成"重建当前终端状态"的前导序列。
//
// ★ 调用顺序有硬要求：**1049（备用屏幕）必须最先发**。
// 它决定后续所有绘制落在哪个缓冲区；顺序错了，画面会画到主缓冲区上。
//
// 返回值可以直接喂给客户端的 xterm.js。
func (m *ModeTracker) Preamble() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []byte

	// 1) 备用屏幕优先
	if _, ok := m.modes[1049]; ok {
		out = append(out, "\x1b[?1049h"...)
	}

	// 2) 其余 DEC 私有模式，升序保证确定性
	modes := make([]uint16, 0, len(m.modes))
	for mode := range m.modes {
		if mode == 1049 {
			continue
		}
		if _, skip := preambleSkip[mode]; skip {
			continue
		}
		modes = append(modes, mode)
	}
	sort.Slice(modes, func(i, j int) bool { return modes[i] < modes[j] })
	for _, mode := range modes {
		out = append(out, "\x1b[?"...)
		out = strconv.AppendUint(out, uint64(mode), 10)
		out = append(out, 'h')
	}

	// 3) extras（XTMODKEYS 等），按 key 排序保证确定性
	keys := make([]string, 0, len(m.extras))
	for k := range m.extras {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, m.extras[k]...)
	}

	return out
}

// Reset 清空全部状态。
//
// 用于会话重启（进程已退出、同一 session 对象要重新起一个进程的场景）。
// ★ 它只清模式，不影响 ring buffer 的序号 —— 那是另一回事。
func (m *ModeTracker) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modes = make(map[uint16]struct{}, 8)
	m.extras = make(map[string][]byte, 2)
	m.st = stNormal
	m.csi = m.csi[:0]
}
