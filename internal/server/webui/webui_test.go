package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// get 直接调用 Handler()，**不经过 ServeMux**。
//
// 这是刻意的：真实部署里 Handler 挂在 mux 的 `/` 上，而 `/api/...`
// 由更具体的模式先接走。但这里要测的正是「如果 /api/... 真的落到了
// SPA handler 上，它会不会错误地返回 HTML」—— 所以必须绕开 mux，
// 把那个兜底防线单独压出来测。
func get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec
}

// TestAPIPrefixNeverFallsBackToHTML 是本包最重要的一条断言。
//
// 如果 API 路径落到 SPA 回落上，前端会拿到 HTTP 200 + 一段 HTML，
// 然后在 JSON.parse 处报错 —— 报错位置离真正原因（接口路径写错）
// 很远，是最难查的一类问题。所以这里逐条压住。
func TestAPIPrefixNeverFallsBackToHTML(t *testing.T) {
	for _, p := range []string{"/api", "/api/", "/api/v1/nope", "/api/v1/devices/x/nope"} {
		rec := get(t, p)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: 状态码 = %d，期望 404", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s: Content-Type = %q，期望 JSON", p, ct)
		}
		body := strings.ToLower(rec.Body.String())
		if strings.Contains(body, "<!doctype") || strings.Contains(body, "<html") {
			t.Errorf("%s: 回落成了 HTML —— 这正是本测试要防的那个 bug", p)
		}
		if !strings.Contains(rec.Body.String(), "not_found") {
			t.Errorf("%s: 响应体缺少错误码 not_found: %q", p, rec.Body.String())
		}
	}
}

// TestSPARoutesReturnIndexHTML 验证前端路由的深链接可用。
//
// 没有这条回落，用户在 `/devices/<id>` 上按刷新就会拿到 404 ——
// 因为服务端并不知道这个路径，它只存在于前端路由表里。
//
// 注意：这条断言在「前端未构建」时同样成立 —— 那种情况下伺服的是
// 内置说明页，它也是 HTML。断言只要求「回落到了一个 HTML 页面」，
// 不要求那个页面是真界面（那是 Built() 的职责）。
func TestSPARoutesReturnIndexHTML(t *testing.T) {
	paths := []string{
		"/",
		"/devices",
		"/devices/0192abcd-0000-7000-8000-000000000000",
		"/sessions/0192abcd-0000-7000-8000-000000000000",
		"/settings",
		"/some/deep/unknown/route",
	}
	for _, p := range paths {
		rec := get(t, p)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: 状态码 = %d，期望 200", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q，期望 HTML", p, ct)
		}
		body := strings.ToLower(rec.Body.String())
		if !strings.Contains(body, "<html") {
			t.Errorf("%s: 响应体不是 HTML: %.80q", p, rec.Body.String())
		}
	}
}

// TestMissingAssetReturns404NotHTML 验证「带扩展名却没命中」的路径不回落。
//
// 回落的话，浏览器会把 HTML 当 JS/CSS 执行，报错是
// "Unexpected token '<'" —— 真正的原因（漏传了 assets）被完全掩盖。
func TestMissingAssetReturns404NotHTML(t *testing.T) {
	for _, p := range []string{
		"/assets/index-deadbeefdeadbeef.js",
		"/assets/nope.css",
		"/missing.json",
		"/deep/path/nothing.js",
	} {
		rec := get(t, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: 状态码 = %d，期望 404（带扩展名的路径不该回落成 index.html）", p, rec.Code)
		}
	}
}

// TestPathTraversalIsContained 验证 Clean + 前导斜杠这套组合确实把路径锁在 dist 里。
func TestPathTraversalIsContained(t *testing.T) {
	for _, p := range []string{
		"/../../etc/passwd",
		"/../../../windows/win.ini",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/assets/../../../../etc/hosts",
	} {
		rec := get(t, p)
		body := rec.Body.String()

		// 只要没读到 dist 之外的真实文件就算通过。
		// 返回 index.html（200）或 404 都可以接受 —— 关键是**不能泄露**。
		if strings.Contains(body, "root:x:") || strings.Contains(body, "[fonts]") {
			t.Errorf("%s: 读到了 dist 之外的文件内容 —— 路径穿越未被拦住", p)
		}
	}
}

// TestIndexIsNotCacheable 验证外壳页不被缓存。
//
// index.html 是唯一文件名里没有内容哈希的产物。一旦被缓存，
// 前端发新版本后用户会继续用旧外壳，而它引用的 assets 已经不存在 ——
// 白屏，且刷新无效。这个 bug 每次发布都会复现一次，所以必须锁住。
//
// 内置说明页同样要满足这条：用户跑完 make web 之后刷新，
// 就该看到真界面，而不是被缓存的提示页挡着。
func TestIndexIsNotCacheable(t *testing.T) {
	rec := get(t, "/")
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("外壳页的 Cache-Control = %q，必须包含 no-cache", cc)
	}
}

// realIndexHTML 是一份「真实构建产物」的 index.html 样本。
const realIndexHTML = `<!doctype html>
<html lang="zh-CN"><head>
<script type="module" crossorigin src="/assets/index-abc123.js"></script>
<link rel="stylesheet" crossorigin href="/assets/index-def456.css">
</head><body><div id="app"></div></body></html>`

// placeholderIndexHTML 是一份占位页样本。
const placeholderIndexHTML = `<!doctype html>
<html lang="zh-CN"><body>前端未构建
<!-- CODEGATE_WEB_PLACEHOLDER -->
</body></html>`

// TestAnalyzeDistStates 把四种状态逐一定住。
//
// ★ 重点是 distIncomplete 那几条。它是唯一「看起来有前端、实际白屏」的状态：
// index.html 是真实产物（没有占位标记），但它引用的 assets 不在 embed 里。
// 这个状态在真实仓库里非常容易产生 —— `make web` 把真实 index.html 写到了
// 被版本库跟踪的路径上，而 `dist/assets/` 被 gitignore，于是
// 「提交了 index.html 却没提交资源」就成了一次安静的事故。
//
// 用 fstest.MapFS 而不是真实 distFS，是因为真实的 dist 只有「完整」和
// 「占位」两种，构造不出中间态 —— 而那正是最需要被测住的一种。
func TestAnalyzeDistStates(t *testing.T) {
	js := &fstest.MapFile{Data: []byte("// entry")}
	css := &fstest.MapFile{Data: []byte("/* style */")}

	cases := []struct {
		name string
		fsys fs.FS
		want distState
	}{
		{
			name: "完整产物",
			fsys: fstest.MapFS{
				"index.html":              &fstest.MapFile{Data: []byte(realIndexHTML)},
				"assets/index-abc123.js":  js,
				"assets/index-def456.css": css,
			},
			want: distBuilt,
		},
		{
			name: "占位页（全新 clone 的正常状态）",
			fsys: fstest.MapFS{
				"index.html": &fstest.MapFile{Data: []byte(placeholderIndexHTML)},
			},
			want: distPlaceholder,
		},
		{
			name: "★ 真实 index.html，JS 缺失（提交了 index 没提交 assets）",
			fsys: fstest.MapFS{
				"index.html":              &fstest.MapFile{Data: []byte(realIndexHTML)},
				"assets/index-def456.css": css,
			},
			want: distIncomplete,
		},
		{
			name: "★ 真实 index.html，assets 整个缺失",
			fsys: fstest.MapFS{
				"index.html": &fstest.MapFile{Data: []byte(realIndexHTML)},
			},
			want: distIncomplete,
		},
		{
			name: "★ index.html 没有任何 /assets/ 引用（来路不明的页面）",
			fsys: fstest.MapFS{
				"index.html": &fstest.MapFile{Data: []byte("<html><body>hi</body></html>")},
			},
			want: distIncomplete,
		},
		{
			name: "index.html 不存在",
			fsys: fstest.MapFS{},
			want: distMissing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := analyzeDist(tc.fsys)
			if got != tc.want {
				t.Errorf("analyzeDist = %s，期望 %s", got, tc.want)
			}
		})
	}
}

// TestIncompleteDistServesNoticeNotBrokenIndex 是本包第二重要的一条断言。
//
// 它防的是「因为 index.html 里没有占位标记，就把它伺服出去」。
// 那个 index.html 会去请求不存在的 /assets/*.js，浏览器白屏、
// 控制台一堆 404，而用户完全不知道要跑 make web。
func TestIncompleteDistServesNoticeNotBrokenIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(
			`<!doctype html><html><head>
<script type="module" src="/assets/index-gone.js"></script>
</head><body><div id="app">真界面</div></body></html>`)},
	}

	rec := httptest.NewRecorder()
	serveIndex(rec, fsys)
	body := rec.Body.String()

	if strings.Contains(body, "真界面") {
		t.Fatal("伺服了引用了不存在资源的 index.html —— 浏览器会白屏，这条断言就是为了防它")
	}
	if !strings.Contains(body, "make web") {
		t.Errorf("说明页必须告诉用户跑 make web，实际内容：%.160q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q，期望 HTML", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("说明页不该被缓存，Cache-Control = %q", cc)
	}
}

// TestServedIndexNeverReferencesMissingAssets 是不变量断言。
//
// 无论内嵌产物处于哪种状态，**伺服出去的页面都不能引用 embed 里不存在的资源**。
// 这条不依赖「现在是构建好的还是占位的」，所以在任何机器上、任何阶段都成立 ——
// 正是这种不变量才值得写死。
func TestServedIndexNeverReferencesMissingAssets(t *testing.T) {
	rec := httptest.NewRecorder()
	serveIndex(rec, distFS)

	for _, ref := range assetRefs(rec.Body.Bytes()) {
		name := strings.TrimPrefix(ref, "/")
		if _, err := fs.Stat(distFS, name); err != nil {
			t.Errorf("伺服的页面引用了 embed 里不存在的资源 %s —— 浏览器会拿到 404", ref)
		}
	}
}

// TestBuiltAgreesWithState 守住 Built() 与 analyzeDist 的一致性。
//
// 单独看有点同义反复，但它挡住的是一个真实的失误模式：
// 有人改了 Built() 的判断依据而没同步 analyzeDist（或反过来），
// 结果是「前端没构建」这件事不再被检测到，Server 静默地服务一个坏页面。
func TestBuiltAgreesWithState(t *testing.T) {
	state, _ := analyzeDist(distFS)
	if got := Built(); got != (state == distBuilt) {
		t.Errorf("Built() = %v，但 analyzeDist 判定为 %s —— 两者必须一致", got, state)
	}
	if s := DistState(); s == "" {
		t.Error("DistState() 返回空串，启动日志会失去排查线索")
	}
}

// TestAssetsAreCacheableWhenBuilt 验证带内容哈希的静态资源可以长期缓存。
//
// 内嵌的是占位文件时跳过 —— 那种状态下根本没有 assets 目录。
func TestAssetsAreCacheableWhenBuilt(t *testing.T) {
	if !Built() {
		t.Skipf("当前内嵌的不是完整产物（%s），跳过 —— 先跑 make web", DistState())
	}

	entries, err := fs.ReadDir(distFS, "assets")
	if err != nil {
		t.Fatalf("读 assets 失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("assets 是空的 —— 构建产物不完整")
	}

	name := entries[0].Name()
	rec := get(t, "/assets/"+name)
	if rec.Code != http.StatusOK {
		t.Fatalf("/assets/%s: 状态码 = %d，期望 200", name, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("/assets/%s 的 Cache-Control = %q，期望包含 immutable", name, cc)
	}
}
