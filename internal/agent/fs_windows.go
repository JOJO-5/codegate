//go:build windows

package agent

import "strings"

// isCaseInsensitiveFS 表示本平台的文件系统是否大小写不敏感。
//
// 它的唯一用途是让路径白名单去重时用对比较方式：
// Windows 上 `C:\Work` 和 `c:\work` 是同一个目录，
// 如果按大小写敏感去重，同一个根会被登记两次，白名单里就多了一条
// 看起来不同、实际相同的规则 —— 排查权限问题时极具误导性。
func isCaseInsensitiveFS() bool { return true }

// normalizeCommandPath 把可执行文件路径里的正斜杠归一化成反斜杠。
//
// ★ 为什么必须做，而不是"让用户自己写对"：
//
// cmd.exe 解析自己的命令行、寻找 /c 或 /k 开关时，用的是**字符串扫描**，
// 而不是先剥掉 argv[0]。于是：
//
//	C:/Windows/System32/cmd.exe /c echo hi
//	                 ^^ 这个 "/c" 被当成开关先命中了
//
// 它把 `/cmd.exe` 读成「开关 /c + 尾巴 md.exe」，剩下的
// `md.exe /c echo hi` 成了要执行的命令。而 `md` 恰好是 cmd 的**内置命令**
// （mkdir 的别名），`/c` 对它不是合法路径参数 —— 于是终端里只留下一句
// 「命令语法不正确。」
//
// 实测（cmd/conpty-diag，Windows 11）：
//
//	C:\Windows\System32\cmd.exe /c echo X  → 打印 X            ✅
//	C:/Windows/System32/cmd.exe /c echo X  → 命令语法不正确。   ❌
//	裸 cmd /c echo X                        → 打印 X            ✅
//
// 这个坑的迷惑性在于：进程起来了、ConPTY 正常、终端里有输出，
// 但那行报错和用户配置的命令**毫无字面关系** —— 从界面上几乎不可能
// 反推回「配置里那个正斜杠」。所以由程序兜住，而不是写进文档指望人记住。
//
// Windows 上 `/` 与 `\` 对文件系统完全等价（CreateProcessW 两种都认），
// 所以归一化是纯收益。**只归一化 command，绝不碰 args** ——
// args 里的 `/` 是开关（`/c`、`/k`）或 Unix 风格参数，改了就是破坏。
func normalizeCommandPath(cmd string) string {
	if !strings.Contains(cmd, "/") {
		return cmd
	}
	return strings.ReplaceAll(cmd, "/", `\`)
}
