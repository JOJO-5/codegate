// Command conpty-diag 是 ConPTY 的最小诊断程序。
//
// 它只做一件事：起一个进程，把从伪控制台读到的每一个字节原样打出来。
// 用来把"读不到输出"这类问题从测试框架里剥离出来单独观察 ——
// 测试框架有超时、有 goroutine、有断言，混在一起时很难看清
// 到底是 Read 阻塞了还是返回了 EOF。
//
// 用法：
//
//	go run ./cmd/conpty-diag
//	go run ./cmd/conpty-diag C:\Windows\System32\cmd.exe /c echo hi
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jojo/codegate/internal/terminal"
)

func main() {
	if err := terminal.Available(); err != nil {
		fmt.Fprintln(os.Stderr, "ConPTY 不可用:", err)
		os.Exit(1)
	}

	command := `C:\Windows\System32\cmd.exe`
	args := []string{"/c", "echo HELLO_FROM_CONPTY"}
	if len(os.Args) > 1 {
		command = os.Args[1]
		args = os.Args[2:]
	}

	fmt.Fprintf(os.Stderr, "=== 命令: %s %v ===\n", command, args)

	term := terminal.New()
	if err := term.Start(context.Background(), terminal.StartConfig{
		Command: command,
		Args:    args,
		Dir:     os.TempDir(),
		Cols:    80,
		Rows:    24,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "Start 失败:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "启动成功, PID=%d\n", term.PID())

	total := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				total += n
				fmt.Fprintf(os.Stderr, "[读 %d 字节] %q\n", n, buf[:n])
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, ">>> Read 返回: %v（累计 %d 字节）\n", err, total)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(12 * time.Second):
		fmt.Fprintf(os.Stderr, ">>> 12 秒内 Read 一直阻塞，累计只读到 %d 字节\n", total)
	}

	term.Close()
	fmt.Fprintf(os.Stderr, "=== 总计 %d 字节 ===\n", total)
}
