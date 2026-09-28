package terminal

import (
	"bytes"
	"testing"
)

func TestModeTrackerBasic(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[?1049h\x1b[?2004h\x1b[?1000h"))

	if !m.AltScreen() {
		t.Error("应处于备用屏幕")
	}
	for _, mode := range []uint16{1049, 2004, 1000} {
		if !m.On(mode) {
			t.Errorf("模式 %d 应为 ON", mode)
		}
	}
	if m.On(2026) {
		t.Error("2026 不该是 ON")
	}
}

func TestModeTrackerReset(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[?1049h\x1b[?2004h"))
	m.Scan([]byte("\x1b[?1049l")) // 退出备用屏幕
	if m.AltScreen() {
		t.Error("收到 ?1049l 后不该还在备用屏幕")
	}
	if !m.On(2004) {
		t.Error("2004 不该被影响")
	}
}

// 一条 CSI 里带多个模式参数（实测 Codex/OpenCode 会这么发）。
func TestModeTrackerMultipleParams(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[?1000;1002;1003;1006h"))
	for _, mode := range []uint16{1000, 1002, 1003, 1006} {
		if !m.On(mode) {
			t.Errorf("模式 %d 应为 ON", mode)
		}
	}
	m.Scan([]byte("\x1b[?1003l"))
	if m.On(1003) {
		t.Error("1003 应被关掉")
	}
	if !m.On(1000) {
		t.Error("1000 不该被影响")
	}
}

// ★ 关键：转义序列完全可能被切在两次 Read 之间。
// 实测 R1 报告里 Codex/OpenCode 的序列都很短，但不代表永远如此。
func TestModeTrackerSplitAcrossScans(t *testing.T) {
	full := []byte("\x1b[?1049h\x1b[?2027h\x1b[?2004h")

	for split := 1; split < len(full); split++ {
		m := NewModeTracker()
		m.Scan(full[:split])
		m.Scan(full[split:])
		if !m.AltScreen() {
			t.Fatalf("在 %d 字节处切开后，1049 丢失", split)
		}
		if !m.On(2027) || !m.On(2004) {
			t.Fatalf("在 %d 字节处切开后，后续模式丢失", split)
		}
	}
}

// 逐字节喂也要正确（模拟最极端的切分）。
func TestModeTrackerByteByByte(t *testing.T) {
	m := NewModeTracker()
	for _, b := range []byte("\x1b[?1049h\x1b[?2004h") {
		m.Scan([]byte{b})
	}
	if !m.AltScreen() || !m.On(2004) {
		t.Fatalf("逐字节喂失败: alt=%v 2004=%v", m.AltScreen(), m.On(2004))
	}
}

// 非模式类的转义序列必须被忽略，且不能把状态机搞乱。
func TestModeTrackerIgnoresOtherSequences(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[2J\x1b[H\x1b[31m红色\x1b[0m"))
	if len(m.Active()) != 0 {
		t.Errorf("不该记录任何模式，实际 %v", m.Active())
	}
	// 之后仍能正常工作
	m.Scan([]byte("\x1b[?1049h"))
	if !m.AltScreen() {
		t.Error("被无关序列干扰后失效了")
	}
}

// 畸形输入不得 panic，也不得把已记录的状态搞丢。
func TestModeTrackerMalformedInput(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[?1049h"))

	m.Scan([]byte("\x1b["))                                                 // 未终结
	m.Scan([]byte("\x1b[?"))                                                // 未终结
	m.Scan([]byte("\x1b[?" + string(bytes.Repeat([]byte("9"), 200)) + "h")) // 超长，应被丢弃
	m.Scan([]byte("\x1b[?abc h"))                                           // 非数字参数
	m.Scan([]byte{0x1b})                                                    // 孤立 ESC

	if !m.AltScreen() {
		t.Error("畸形输入把已有状态搞丢了")
	}
}

// ★ Preamble 的顺序有硬要求：1049 必须最先发。
func TestPreambleAltScreenFirst(t *testing.T) {
	m := NewModeTracker()
	// 故意按"错误"顺序喂，验证输出仍然是 1049 在前
	m.Scan([]byte("\x1b[?2004h\x1b[?1000h\x1b[?2027h\x1b[?1049h"))

	p := string(m.Preamble())
	if len(p) == 0 {
		t.Fatal("Preamble 为空")
	}
	idxAlt := bytes.Index([]byte(p), []byte("\x1b[?1049h"))
	if idxAlt != 0 {
		t.Fatalf("1049 必须出现在最前面，实际 Preamble = %q", p)
	}
	for _, want := range []string{"\x1b[?1000h", "\x1b[?2004h", "\x1b[?2027h"} {
		if !bytes.Contains([]byte(p), []byte(want)) {
			t.Errorf("Preamble 缺少 %q，实际 %q", want, p)
		}
	}
}

// ★ 2026（同步输出）绝不能进 Preamble。
//
// 若重放时把它打开，而配对的 ?2026l 落在 ring buffer 已被覆盖的区间里，
// 客户端会永远停在同步模式 —— 画面完全冻住。attach 流程最后会无条件补一个
// ?2026l 兜底，但 Preamble 本身不该引入这个问题。
func TestPreambleSkipsSyncOutput(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[?1049h\x1b[?2026h\x1b[?2004h"))

	p := string(m.Preamble())
	if bytes.Contains([]byte(p), []byte("2026")) {
		t.Fatalf("★ Preamble 不该包含 2026，实际 %q", p)
	}
	// 但状态本身要如实记录（调用方需要知道它当前是 ON）
	if !m.On(2026) {
		t.Error("状态记录不该被跳过")
	}
}

func TestPreambleEmpty(t *testing.T) {
	m := NewModeTracker()
	if p := m.Preamble(); len(p) != 0 {
		t.Errorf("空跟踪器的 Preamble 应为空，实际 %q", p)
	}
}

// Preamble 必须是确定性的（便于测试与日志比对）。
func TestPreambleDeterministic(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[?1006h\x1b[?1000h\x1b[?2004h\x1b[?1049h\x1b[?2027h"))
	first := string(m.Preamble())
	for i := 0; i < 20; i++ {
		if got := string(m.Preamble()); got != first {
			t.Fatalf("第 %d 次 Preamble 不一致:\n%q\n%q", i, got, first)
		}
	}
}

// XTMODKEYS 会改变按键编码方式，重连时必须同步，否则键盘输入会不对。
func TestModeTrackerXTMODKEYS(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[>4;1m"))

	p := string(m.Preamble())
	if !bytes.Contains([]byte(p), []byte("\x1b[>4;1m")) {
		t.Fatalf("Preamble 应包含 XTMODKEYS，实际 %q", p)
	}

	// 归零表示恢复默认，应从 Preamble 里去掉
	m.Scan([]byte("\x1b[>4;0m"))
	if p := string(m.Preamble()); bytes.Contains([]byte(p), []byte(">4;")) {
		t.Fatalf("XTMODKEYS 归零后不该还在 Preamble 里: %q", p)
	}
}

// Active 返回升序，便于日志比对。
func TestActiveSorted(t *testing.T) {
	m := NewModeTracker()
	m.Scan([]byte("\x1b[?1049h\x1b[?25h\x1b[?1000h\x1b[?2004h"))
	got := m.Active()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Active 未升序: %v", got)
		}
	}
}
