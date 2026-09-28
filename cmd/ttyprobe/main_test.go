package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

// captureEmit 返回一个把事件写进缓冲区的 emit，以及那个缓冲区。
//
// 为什么不直接调 main()：main() 会向操作系统要控制台尺寸，而
// `go test` 的 stdout 在 Windows 上不是控制台句柄，那个调用必然失败。
// 所以「输入解析 → 事件输出」这段纯逻辑和「取尺寸」要分开测 ——
// 前者在这里，后者在 integration_test.go 里用真实 ConPTY 测。
func captureEmit() (func(string, ...any), *bytes.Buffer) {
	var mu sync.Mutex
	var buf bytes.Buffer
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&buf, format+"\n", args...)
	}, &buf
}

// feed 把 input 喂给 run()，返回它产生的事件流。
func feed(t *testing.T, input string) string {
	t.Helper()

	emit, buf := captureEmit()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = saved
		r.Close()
	})

	// 必须在 goroutine 里写：input 可能超过管道缓冲区（64 KB），
	// 而 run() 是同步读的，同一个 goroutine 里写会死锁。
	go func() {
		_, _ = io.WriteString(w, input)
		w.Close()
	}()

	run(emit)
	return buf.String()
}

func TestRunEcho(t *testing.T) {
	got := feed(t, "hello\n")
	if !strings.Contains(got, "ECHO hello") {
		t.Errorf("缺少 ECHO hello，实际:\n%s", got)
	}
}

func TestRunExitReportsZeroAndStops(t *testing.T) {
	got := feed(t, "exit\nnever\n")
	if !strings.Contains(got, "EXIT code=0") {
		t.Errorf("缺少 EXIT code=0，实际:\n%s", got)
	}
	// exit 之后必须停止处理：探针要能被可靠地关掉，
	// 否则测试结束时进程会挂在那里等 stdin。
	if strings.Contains(got, "ECHO never") {
		t.Errorf("exit 之后仍在处理输入，实际:\n%s", got)
	}
}

func TestRunCtrlCDiscardsPartialLine(t *testing.T) {
	// 0x03 在真实终端里会丢弃当前未提交的输入行。
	// 探针要模拟这个行为，否则测试没法区分「Ctrl+C 被当字符收到」
	// 和「Ctrl+C 什么都没发生，只是前面几个字符凑巧成了别的行」。
	got := feed(t, "ab\x03cd\n")
	if !strings.Contains(got, "CTRL_C byte=0x03") {
		t.Errorf("缺少 CTRL_C 事件，实际:\n%s", got)
	}
	if !strings.Contains(got, "ECHO cd") {
		t.Errorf("Ctrl+C 之后的新行没有形成 ECHO cd，实际:\n%s", got)
	}
	if strings.Contains(got, "ECHO ab") {
		t.Errorf("Ctrl+C 之前的半行没有被丢弃，实际:\n%s", got)
	}
}

func TestRunCRLFIsOneLine(t *testing.T) {
	// Windows 侧行尾是 \r\n。如果按「\r 和 \n 各自成行」处理，
	// 每行会多出一个空行，测试断言全部错位。
	got := feed(t, "a\r\nb\r\n")
	if n := strings.Count(got, "ECHO"); n != 2 {
		t.Errorf("\\r\\n 被当成了多行，得到 %d 个 ECHO（期望 2）:\n%s", n, got)
	}
}

func TestRunCROnlyIsOneLine(t *testing.T) {
	// raw mode 下 Enter 可能只产生 \r，没有 \n。
	got := feed(t, "a\rb\r")
	if n := strings.Count(got, "ECHO"); n != 2 {
		t.Errorf("纯 \\r 行尾未被识别，得到 %d 个 ECHO（期望 2）:\n%s", n, got)
	}
}

func TestRunEmptyLineIsAnEvent(t *testing.T) {
	// 空行是有效输入（用户在 shell 里按回车就是这个）。
	// 不能因为「行是空的」就跳过 —— 那会让回显测试少一次事件。
	got := feed(t, "\n")
	if !strings.Contains(got, "ECHO \n") {
		t.Errorf("空行没有产生 ECHO 事件，实际:\n%q", got)
	}
}

func TestRunPreservesMultibyteBytes(t *testing.T) {
	// 探针是字节透明的：不做任何 Unicode 规范化、不断词、不补空格。
	// 这行断言如果挂了，说明有人往 run() 里加了「整理」逻辑 ——
	// 而那正是 §16.3 明令禁止的。
	const in = "中文😀　全角ｆｕｌｌ"
	got := feed(t, in+"\n")
	if !strings.Contains(got, "ECHO "+in) {
		t.Errorf("多字节字符没有被原样保留，实际:\n%q", got)
	}
}

func TestRunWidthSamples(t *testing.T) {
	got := feed(t, "width\n")
	for _, want := range []string{
		"WIDTH ascii=", "WIDTH cjk=", "WIDTH emoji=", "WIDTH box=", "WIDTH mixed=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少宽度样本 %q，实际:\n%s", want, got)
		}
	}
}

func TestRunTruncatesAbsurdLine(t *testing.T) {
	// 1 MB 上限：探针不是数据通道。这里只验证「不会因为超长行而崩」，
	// 不验证具体截断位置（那是实现细节）。
	huge := strings.Repeat("x", 2<<20)
	got := feed(t, huge+"\n")
	if !strings.Contains(got, "ECHO ") {
		t.Errorf("超长行导致探针没有产生任何 ECHO，实际输出长度 %d", len(got))
	}
}
