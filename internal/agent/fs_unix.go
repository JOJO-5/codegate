//go:build !windows

package agent

// isCaseInsensitiveFS 见 fs_windows.go 的说明。
//
// macOS 的默认文件系统（APFS/HFS+）也是大小写不敏感的，但可以配置成敏感，
// 且 Linux 一定是敏感的。这里统一返回 false —— 宁可少去重（多一条规则
// 不会造成安全漏洞），也不要在大小写敏感的平台上错误合并两个真实不同的目录。
func isCaseInsensitiveFS() bool { return false }

// normalizeCommandPath 在非 Windows 平台上是恒等函数。
//
// 见 fs_windows.go 里的完整说明：那个「/c 被路径里的 /cmd.exe 抢走」的坑
// 是 cmd.exe 独有的，Unix 的 shell 没有这个问题，正斜杠本来就是正确写法。
func normalizeCommandPath(cmd string) string { return cmd }
