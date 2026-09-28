package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestWorkspace 建一个临时工作区并返回它的**真实**路径。
//
// 为什么必须 EvalSymlinks：t.TempDir() 在 macOS 上常落在 /var，
// 而 /var 是指向 /private/var 的 symlink。不解析的话，
// 后面所有"解析结果应等于期望值"的断言都会因为前缀不同而失败，
// 看起来像代码 bug，实际是测试夹具的问题。
func newTestWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("解析临时目录失败: %v", err)
	}
	return NewWorkspace([]string{real}), real
}

func TestResolveAllowsInsideRoot(t *testing.T) {
	w, root := newTestWorkspace(t)

	got, err := w.Resolve(filepath.Join(root, "a", "b.txt"))
	if err != nil {
		t.Fatalf("根目录内的路径被拒了: %v", err)
	}
	if want := filepath.Join(root, "a", "b.txt"); got != want {
		t.Errorf("解析结果 = %q，期望 %q", got, want)
	}

	// 根本身也应当被允许 —— 用户要能在根目录里开 shell。
	if _, err := w.Resolve(root); err != nil {
		t.Errorf("根目录自身被拒了: %v", err)
	}
}

func TestResolveRejectsOutsideRoot(t *testing.T) {
	w, root := newTestWorkspace(t)

	outside := filepath.Join(filepath.Dir(root), "elsewhere", "x.txt")
	if _, err := w.Resolve(outside); !errors.Is(err, ErrPathNotAllowed) {
		t.Errorf("根目录外的路径没有被拒（err=%v）", err)
	}
}

func TestResolveRejectsDotDotEscape(t *testing.T) {
	w, root := newTestWorkspace(t)

	if _, err := w.Resolve(filepath.Join(root, "..", "..", "etc")); !errors.Is(err, ErrPathNotAllowed) {
		t.Errorf("`..` 穿越没有被拒（err=%v）", err)
	}
}

// TestResolveRejectsSiblingPrefix 覆盖最经典的白名单绕过：
// 根是 `…/work`，而 `…/workspace` 以它的字符串为前缀。
//
// 如果 pathWithin 用了朴素的 strings.HasPrefix（没补分隔符），
// 这个测试会红。这是白名单实现里最容易犯、也最难在评审中看出来的错。
func TestResolveRejectsSiblingPrefix(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(base, "work")
	sibling := filepath.Join(base, "workspace")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	w := NewWorkspace([]string{root})
	if _, err := w.Resolve(filepath.Join(sibling, "secret.txt")); !errors.Is(err, ErrPathNotAllowed) {
		t.Errorf("前缀相同的兄弟目录没有被拒（err=%v）—— "+
			"通常意味着 pathWithin 里漏了分隔符比较", err)
	}
}

func TestResolveRejectsExoticPaths(t *testing.T) {
	w, _ := newTestWorkspace(t)

	cases := []struct {
		name string
		path string
	}{
		{"UNC 共享", `\\server\share\x.txt`},
		{"本机管理共享", `\\localhost\C$\Windows\System32`},
		{"扩展长度前缀", `\\?\C:\Windows\System32`},
		{"正斜杠 UNC", `//server/share/x.txt`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.Resolve(tc.path); !errors.Is(err, ErrPathNotAllowed) {
				t.Errorf("%s 没有被拒（err=%v）", tc.path, err)
			}
		})
	}
}

func TestResolveRejectsEmpty(t *testing.T) {
	w, _ := newTestWorkspace(t)

	for _, p := range []string{"", "   ", "\t"} {
		if _, err := w.Resolve(p); !errors.Is(err, ErrPathEmpty) {
			t.Errorf("空路径 %q 没有被拒（err=%v）", p, err)
		}
	}
}

// TestResolveRejectsSymlinkEscape 是第三道防线（EvalSymlinks）的回归测试。
//
// 没有它，根目录下的一个 symlink 就能指向任意位置，
// 前面所有字面比较全部落空。
func TestResolveRejectsSymlinkEscape(t *testing.T) {
	w, root := newTestWorkspace(t)

	outside := t.TempDir()
	link := filepath.Join(root, "escape")

	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("本机不允许创建 symlink（Windows 需要开发者模式或管理员权限）: %v", err)
	}

	if _, err := w.Resolve(filepath.Join(link, "x.txt")); !errors.Is(err, ErrPathNotAllowed) {
		t.Errorf("symlink 逃逸没有被拒（err=%v）", err)
	}
}

func TestResolveAllowsNonexistentPathUnderRoot(t *testing.T) {
	// 「在根下新建一个还不存在的深层文件」必须通过 ——
	// 否则上传功能没法工作。resolveReal 的自底向上解析就是为它写的。
	w, root := newTestWorkspace(t)

	deep := filepath.Join(root, "a", "b", "c", "new.txt")
	if _, err := w.Resolve(deep); err != nil {
		t.Errorf("根下尚不存在的路径被拒了: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ResolveIn：文件 API 的相对路径入口
// ---------------------------------------------------------------------------

func TestResolveInRelative(t *testing.T) {
	w, root := newTestWorkspace(t)

	got, err := w.ResolveIn(root, filepath.Join("sub", "file.txt"))
	if err != nil {
		t.Fatalf("正常的相对路径被拒: %v", err)
	}
	if want := filepath.Join(root, "sub", "file.txt"); got != want {
		t.Errorf("解析结果 = %q，期望 %q", got, want)
	}

	// 空串与 "." 都表示根目录自身。
	for _, rel := range []string{"", "."} {
		if got, err := w.ResolveIn(root, rel); err != nil || got != root {
			t.Errorf("ResolveIn(%q) = (%q, %v)，期望 (%q, nil)", rel, got, err, root)
		}
	}
}

func TestResolveInRejectsEscape(t *testing.T) {
	w, root := newTestWorkspace(t)

	for _, rel := range []string{
		"..",
		filepath.Join("..", "..", "Windows"),
		filepath.Join("sub", "..", "..", "escape"),
	} {
		if _, err := w.ResolveIn(root, rel); !errors.Is(err, ErrPathNotAllowed) {
			t.Errorf("相对路径 %q 的逃逸没有被拒（err=%v）", rel, err)
		}
	}
}

func TestResolveInRejectsAbsolute(t *testing.T) {
	// 相对路径的入参收到绝对路径是调用方搞错了，必须明确报错，
	// 而不是"聪明地"把它当成相对路径处理 —— 那会掩盖协议误用。
	w, root := newTestWorkspace(t)

	abs := filepath.Join(root, "x.txt")
	if _, err := w.ResolveIn(root, abs); !errors.Is(err, ErrPathNotAllowed) {
		t.Errorf("绝对路径被当成相对路径接受了（err=%v）", err)
	}
}

// ---------------------------------------------------------------------------
// 边界：空工作区
// ---------------------------------------------------------------------------

func TestEmptyWorkspaceDeniesEverything(t *testing.T) {
	// 「没有配置工作区」必须是"什么都访问不了"，而不是"什么都能访问"。
	w := NewWorkspace(nil)

	if _, err := w.Resolve(t.TempDir()); !errors.Is(err, ErrPathNotAllowed) {
		t.Errorf("空工作区居然允许了访问（err=%v）", err)
	}
}

func TestPathWithin(t *testing.T) {
	sep := string(filepath.Separator)
	root := filepath.Join("C:"+sep, "Work")

	cases := []struct {
		p    string
		want bool
	}{
		{root, true},
		{filepath.Join(root, "a"), true},
		{filepath.Join(root, "a", "b"), true},
		{root + sep + ".." + sep + "Other", false},
		{filepath.Join("C:"+sep, "Workspace"), false}, // ★ 前缀陷阱
		{filepath.Join("C:"+sep, "Work2"), false},
		{filepath.Join("C:"+sep, "Windows"), false},
	}
	for _, tc := range cases {
		if got := pathWithin(root, tc.p); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v，期望 %v", root, tc.p, got, tc.want)
		}
	}

	if runtime.GOOS == "windows" {
		// Windows 上大小写不敏感：同一个目录的不同写法必须判定为相同。
		if !pathWithin(root, strings.ToLower(root)) {
			t.Errorf("Windows 上 %q 与 %q 应视为同一路径", root, strings.ToLower(root))
		}
	}
}
