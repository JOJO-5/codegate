package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// 路径校验的哨兵错误。
var (
	// ErrPathEmpty 表示路径为空。
	ErrPathEmpty = errors.New("agent: 路径为空")
	// ErrPathNotAllowed 表示路径不在允许的工作区内。
	ErrPathNotAllowed = errors.New("agent: 路径不在允许的工作区内")
)

// Workspace 是路径白名单的执行者。
//
// ★★ 全项目**只有这一处**实现路径校验（架构 §6.5、文件传输文档 F9）。
//
// PTY 的 cwd 和文件 API 共用它。理由不是"复用代码"这种审美问题，
// 而是安全：两处各写一份校验必然随时间漂移（一边加了 symlink 检查、
// 另一边忘了），而**漂移出来的那个就是绕过口**。这种漏洞不会在
// 代码评审里显形，只会在某天被人用 `..\..\` 打穿。
//
// # 与"PTY 里能跑 cat /etc/passwd"的关系
//
// 这两件事不矛盾，别把它们混为一谈：
//
//   - PTY 里用户敲 `cat /etc/passwd` 是**用户自己操作自己的机器**，
//     该不该拦是用户的事，Agent 不越权替他决定。
//   - 文件 API 是**远程界面点出来的**，没有"用户自己敲"这层语义，
//     所以必须走白名单。
//
// 换句话说：白名单约束的是**远程界面的能力**，不是用户在终端里的自由。
type Workspace struct {
	// roots 是已归一化（绝对路径 + Clean + 去重）的根目录。
	// 由 Config.Prepare 保证。
	roots []string
}

// NewWorkspace 创建一个工作区校验器。
//
// roots 应当是 Config.Prepare 处理过的（绝对路径、已去重）。
// 传空切片不会报错，但任何 Resolve 都会失败 —— 这是刻意的：
// 「没有配置工作区」应该是"什么都访问不了"，而不是"什么都能访问"。
func NewWorkspace(roots []string) *Workspace {
	return &Workspace{roots: append([]string(nil), roots...)}
}

// Roots 返回允许的根目录副本。
func (w *Workspace) Roots() []string {
	return append([]string(nil), w.roots...)
}

// Resolve 把输入路径解析成绝对路径，并确认它落在某个允许的根之下。
//
// 三道防线，缺一不可：
//
//  1. **形状检查**：拒绝 UNC（`\\server\share`）与扩展前缀（`\\?\`）。
//     这两类路径会让后面所有的字符串前缀比较失去意义 ——
//     `\\?\C:\Work` 和 `C:\Work` 指向同一个目录，但字符串完全不同。
//
//  2. **前缀检查**：归一化后逐根比对，且必须**按路径段**比对。
//     朴素的 `strings.HasPrefix(p, root)` 会把 `C:\Workspace` 判成
//     在根 `C:\Work` 之下 —— 这是最经典的白名单绕过。
//
//  3. **符号链接解析**：`EvalSymlinks` 之后再比对一次。
//     没有这一步，根目录下的一个 symlink 就能指向任意位置，
//     前两道防线全部落空。
func (w *Workspace) Resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", ErrPathEmpty
	}
	if err := rejectExoticPath(p); err != nil {
		return "", err
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("agent: 无法把 %q 转成绝对路径: %w", p, err)
	}
	abs = filepath.Clean(abs)

	// 第二道防线：字面比对。
	if !w.within(abs) {
		return "", fmt.Errorf("%w: %s", ErrPathNotAllowed, abs)
	}

	// 第三道防线：解析 symlink / 8.3 短名后再比对一次。
	//
	// 只有在能解析出来时才比对 —— 路径可能尚不存在（新建文件、
	// 新建目录），此时 resolveReal 会返回最长可解析前缀 + 剩余部分，
	// 仍然是有意义的检查。
	real, err := resolveReal(abs)
	if err != nil {
		// 解析失败不放过：宁可拒绝，也不要"因为查不出来就假设它安全"。
		return "", fmt.Errorf("agent: 无法解析 %s 的真实路径: %w", abs, err)
	}
	if !w.within(real) {
		return "", fmt.Errorf("%w: %s 解析后指向 %s", ErrPathNotAllowed, abs, real)
	}
	return real, nil
}

// ResolveIn 把**相对路径**解析到指定根之下（文件 API 用）。
//
// 相对路径是文件 API 的入参格式（文件传输文档 §4）：绝对路径会暴露
// 用户名与盘符结构，而相对路径正好是后续所有接口的入参格式，
// 前端不用做任何转换。
func (w *Workspace) ResolveIn(root, rel string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrPathEmpty
	}

	rootAbs, err := w.Resolve(root)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(rel) == "" || rel == "." {
		return rootAbs, nil
	}

	// 相对路径不接受绝对路径 —— 这是调用方搞错了，不是"用户想访问那里"。
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: 期望相对路径，收到绝对路径 %q", ErrPathNotAllowed, rel)
	}
	if err := rejectExoticPath(rel); err != nil {
		return "", err
	}

	// Join 会自动 Clean，`..\..\x` 在这里被折叠成相对 root 的路径，
	// 随后 Resolve 的根检查会拦住逃逸。
	return w.Resolve(filepath.Join(rootAbs, rel))
}

// within 判断绝对路径是否落在某个根之下。
func (w *Workspace) within(abs string) bool {
	for _, root := range w.roots {
		if pathWithin(root, abs) {
			return true
		}
	}
	return false
}

// pathWithin 判断 p 是否在 root 之下（含 root 自身）。
//
// ★ 必须按**路径段**比对，不能直接 HasPrefix。
// `C:\Workspace` 以 `C:\Work` 为前缀，但它显然不在 `C:\Work` 里面。
// 加上分隔符再比，就把这种情况排除了。
func pathWithin(root, p string) bool {
	root = filepath.Clean(root)
	p = filepath.Clean(p)

	if pathEqual(root, p) {
		return true
	}

	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(normCase(p), normCase(prefix))
}

// pathEqual 按平台的大小写规则比较两个路径是否相同。
func pathEqual(a, b string) bool {
	return normCase(filepath.Clean(a)) == normCase(filepath.Clean(b))
}

// normCase 在大小写不敏感的文件系统上把路径转成小写。
func normCase(p string) string {
	if isCaseInsensitiveFS() {
		return strings.ToLower(p)
	}
	return p
}

// rejectExoticPath 拒绝 UNC 与扩展长度前缀。
//
// 为什么这两类必须**在解析之前**就拒掉，而不是留给后面的比对：
//
//   - `\\?\C:\Work\..\..\Windows` —— `\\?\` 前缀会**关闭 Win32 的
//     路径规范化**，`..` 不再被折叠。于是它字面上"看起来"在 C:\Work 下，
//     实际却指向别处。前缀比较对它完全失效。
//   - `\\localhost\C$\Windows` —— UNC 可以借本机的管理共享绕开盘符，
//     而且它根本不匹配任何以 `C:\` 开头的根。
//
// 白名单里的根永远是本地盘符路径，所以任何以 `\\` 开头的输入
// 都不可能是合法请求，直接拒绝最简单也最安全。
func rejectExoticPath(p string) error {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return fmt.Errorf("%w: 不接受 UNC 或 \\\\?\\ 前缀路径: %q", ErrPathNotAllowed, p)
	}
	return nil
}

// resolveReal 尽力把路径解析成"真实"路径（解开 symlink、8.3 短名）。
//
// 路径可能尚不存在，所以自底向上找**最长的已存在前缀**去解析，
// 剩下的部分原样接回去。这样「在根下新建一个深层文件」也能通过校验，
// 而「根下有个 symlink 指向 C:\Windows」仍然会被拦住。
func resolveReal(abs string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}

	parent := filepath.Dir(abs)
	if parent == abs {
		// 已经到根（`C:\` 或 `/`），没法再往上找了。
		// 到这一步说明连根都解析不了 —— 交给调用方处理。
		return abs, nil
	}

	resolvedParent, err := resolveReal(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}
