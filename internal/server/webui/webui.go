// Package webui 把前端构建产物嵌进 Server 二进制。
//
// # 为什么要嵌
//
// 部署形态是「一台有公网地址的机器上跑一个可执行文件」。如果前端产物是
// 磁盘上的一个目录，部署就多出两件事：同步目录、以及保证它和二进制版本一致。
// 两者都会出错，而且出错的方式很难查（新二进制 + 旧静态资源 = 白屏）。
//
// 嵌进去之后，「前端和它对端的 API 是同一个版本」成了编译期保证。
//
// # 为什么 dist/ 里有一个 .gitkeep
//
// `go:embed` 的 pattern 在编译期求值：如果 `dist/` 不存在、或者里面
// 一个可嵌入的文件都没有，**编译直接失败**。而 `dist/` 是构建产物、
// 不进版本库 —— 于是全新 clone 的仓库无法编译。
//
// 所以仓库里保留 `dist/.gitkeep`。注意 embed 默认**忽略**以 `.` 或 `_`
// 开头的文件（与 go 工具链的忽略规则一致），所以这里必须写
// `//go:embed all:dist` —— `all:` 前缀把点文件也纳入。少了它，
// pattern 匹配不到任何文件，编译失败。
//
// ★ 曾经的做法是「提交一个占位 index.html，内容是一句『前端未构建』」。
//   它被换掉是因为那个文件会被 `make web` 覆盖：真实构建的 index.html
//   必须落在同一个路径上。于是「提交前记得把占位页恢复回去」成了一条
//   只能靠人记住的规矩，而违反它的后果是**安静地**提交一个引用了
//   不存在资源的 index.html。现在未构建时伺服的是下面的 notBuiltPage
//   常量，`make web-clean` 也简化成一条 rm，这类失误不再可能发生。
//
// # 为什么 Built() 要校验资源存在性，而不只看 index.html 在不在
//
// 只看「index.html 存不存在」会漏掉一种**真实的**坏状态：
//
//	index.html 是真实构建的产物，但它引用的 assets/ 不在 embed 里
//
// 这个状态很容易产生：构建产物被部分拷贝、或者发布时漏传了 assets。
// 此时若 Built() 返回 true，Server 启动不警告，浏览器打开是一张白屏
// （index.html 引用 404），而日志里干干净净。
//
// 这正是本项目最忌讳的失败方式：**编译通过、测试通过、启动无警告、页面是坏的**。
//
// 所以 Built() 会解析 index.html 里所有 `/assets/...` 引用并逐个确认它们
// 真的在 embed 里。任何不一致（缺 index.html / 是占位页 / 引用的资源缺失）
// 都判为「未构建」，由 Handler 退化到一份说明页，并让 Server 打警告。
package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// distFS 是 embedded 去掉 "dist/" 前缀后的视图。
//
// 内部路径因此直接是 "index.html" / "assets/xxx.js"，
// 与 http.FS 的期望一致，省掉到处写前缀。
var distFS fs.FS = func() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// 只可能因为 embed 的目录名写错，属于编译期就能发现的问题。
		panic("webui: 无法定位 dist 子目录: " + err.Error())
	}
	return sub
}()

// placeholderMarker 是历史上占位 index.html 用过的标记串。
//
// 现在 dist/ 里不再有占位 index.html（见包注释），这条检查成了纯粹的
// 兜底：万一有人重新引入一个手写的 index.html 顶在 dist/ 里，
// 它不会被误当成真实产物伺服出去。
//
// 用「内容里有没有这句话」而不是「assets 目录在不在」来判断：
// 后者依赖 Vite 的输出布局，Vite 改一次输出结构这个判断就静默失效了。
const placeholderMarker = "CODEGATE_WEB_PLACEHOLDER"

// distState 是内嵌产物的完整性状态。
//
// 分成四种而不是一个 bool，是因为「怎么坏的」决定了要不要警告、
// 以及该伺服什么内容。合成 bool 会把这些信息丢掉。
type distState int

const (
	// distMissing：连 index.html 都读不到。embed 配置坏了，或 dist 被清空。
	distMissing distState = iota
	// distPlaceholder：读到了占位页。全新 clone 的正常状态，跑 make web 即可。
	distPlaceholder
	// distIncomplete：index.html 是真实产物，但它引用的资源不在 embed 里。
	// 这是最危险的一种 —— 看上去「有前端」，实际是白屏。
	distIncomplete
	// distBuilt：完整可用的前端产物。
	distBuilt
)

// String 让测试失败信息可读。
func (s distState) String() string {
	switch s {
	case distMissing:
		return "distMissing（读不到 index.html）"
	case distPlaceholder:
		return "distPlaceholder（占位页，未构建）"
	case distIncomplete:
		return "distIncomplete（index.html 引用的资源缺失）"
	case distBuilt:
		return "distBuilt（完整产物）"
	default:
		return "未知状态"
	}
}

// assetRefPattern 抓 index.html 里对构建产物的引用。
//
// 只认 `/assets/` 前缀：那是 Vite 的输出目录，也是唯一会被缓存成
// immutable 的一类路径。外部链接、favicon 之类不参与完整性判断 ——
// 它们缺失不会导致白屏。
var assetRefPattern = regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`)

// assetRefs 返回 index.html 引用的资源路径（以 `/` 开头）。
func assetRefs(indexHTML []byte) []string {
	matches := assetRefPattern.FindAllSubmatch(indexHTML, -1)
	refs := make([]string, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, string(m[1]))
	}
	return refs
}

// analyzeDist 判定一个 dist 目录的完整性状态，并返回 index.html 的内容。
//
// 接受 fs.FS 而不是直接用 distFS，是为了能在测试里喂 fstest.MapFS
// 把四种状态都构造出来 —— 否则 distIncomplete 这条分支永远测不到，
// 而它恰恰是引入这个函数要防的那个 bug。
func analyzeDist(fsys fs.FS) (distState, []byte) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return distMissing, nil
	}
	if bytes.Contains(data, []byte(placeholderMarker)) {
		return distPlaceholder, data
	}

	refs := assetRefs(data)
	if len(refs) == 0 {
		// 一个真实构建的 index.html 必然引用至少一个 /assets/ 资源
		// （Vite 一定会产出入口 JS）。没有引用说明它不是我们预期的产物，
		// 按「不完整」处理 —— 宁可不伺服，也不伺服一个来路不明的页面。
		return distIncomplete, data
	}

	for _, ref := range refs {
		name := strings.TrimPrefix(ref, "/")
		st, err := fs.Stat(fsys, name)
		if err != nil || st.IsDir() {
			return distIncomplete, data
		}
	}
	return distBuilt, data
}

// Built 报告内嵌的是否为完整可用的前端产物。
//
// Server 启动时用它决定要不要打一条警告。
func Built() bool {
	state, _ := analyzeDist(distFS)
	return state == distBuilt
}

// DistState 返回内嵌产物状态的文字描述，供启动日志使用。
//
// 导出它是为了让「为什么 Built() 是 false」在日志里可读 ——
// 只说「前端未构建」而不说「缺了哪个资源」，排查要绕远路。
func DistState() string {
	state, _ := analyzeDist(distFS)
	return state.String()
}

// notBuiltPage 是「前端不可用」时伺服的说明页。
//
// 用 Go 常量而不是内嵌文件：这段内容在任何一种坏状态下都必须能输出，
// 而坏状态本身就包括「内嵌文件读不出来」。
const notBuiltPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta name="color-scheme" content="dark">
<meta name="robots" content="noindex, nofollow">
<title>CodeGate — 前端未构建</title>
<style>
body{margin:0;padding:32px 20px;background:#0d1117;color:#e6edf3;
font:15px/1.7 ui-sans-serif,system-ui,"Segoe UI","Microsoft YaHei",sans-serif}
main{max-width:640px;margin:0 auto}
h1{font-size:19px;margin:0 0 4px}
.sub{color:#8b949e;font-size:13px;margin-bottom:24px}
pre{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:14px 16px;
overflow-x:auto;font-size:13px;font-family:ui-monospace,Consolas,monospace}
.hint{color:#8b949e;font-size:13px;margin-top:22px}
</style>
</head>
<body><main>
<h1>CodeGate 前端不可用</h1>
<div class="sub">这个二进制里没有完整的前端产物。</div>
<p>Server 本身是好的 —— 认证、设备、会话、WebSocket 都在跑，缺的只是浏览器界面。</p>
<pre>make web      # 构建前端（需要 Node 20+）
make server   # 重新编译，把产物嵌进二进制</pre>
<p class="hint">前端产物是用 <code>//go:embed</code> 在<strong>编译期</strong>嵌进二进制的，
所以 <code>make web</code> 之后必须重编才有界面。</p>
<p class="hint">只用 API 不需要前端：<code>/healthz</code>、<code>/api/v1/version</code>
以及 <code>/api/v1/*</code> 都正常工作。</p>
</main></body></html>
`

// Handler 返回服务前端的 http.Handler。
//
// 它做三件事：
//
//  1. **静态资源**：命中就返回，`/assets/` 下的带内容哈希，可以长期缓存。
//  2. **SPA 回落**：没命中的路径返回 `index.html`，让前端路由（vue-router
//     的 history 模式）自己处理。没有这一步，用户刷新 `/devices/xxx`
//     会得到 404 —— 因为服务端并不知道这个路径。
//  3. **`/api/` 前缀绝不回落**：见下面的注释，这条是防"接口拼错拿到 HTML"。
//
// 挂载方式是把返回的 handler 注册到 `ServeMux` 的 `/` 上。
// Go 1.22+ 的 ServeMux 按「更具体的模式优先」消歧，所以
// `/api/v1/...`、`/healthz` 这些显式注册的路径会先命中，
// 只有真正没人认领的路径才会落到这里。
func Handler() http.Handler {
	files := http.FileServer(http.FS(distFS))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ---- 1. /api/ 前缀不回落 ----
		//
		// ★ 这一条很关键。如果让它回落到 index.html，那么一个拼错的接口路径
		//   会拿到 HTTP 200 和一段 HTML。前端会试图 JSON.parse 它，
		//   然后在离真正原因很远的地方报一个"响应不是 JSON"——
		//   排查成本极高。明确 404 + JSON 才是正确的反馈。
		//
		// 精确匹配 "/api" 也要挡：它同样不是前端路由，而是一个几乎必然
		// 写错了的 API 路径。
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"接口不存在"}}`))
			return
		}

		// ---- 2. 静态资源 ----
		//
		// 先 Clean 再拼前导斜杠：Clean 会把 `..` 解析掉，
		// 而前缀 `/` 保证结果不会越出根目录。两者缺一不可。
		clean := path.Clean("/" + r.URL.Path)
		if clean != "/" {
			name := strings.TrimPrefix(clean, "/")
			if f, err := distFS.Open(name); err == nil {
				_ = f.Close()
				if st, err := fs.Stat(distFS, name); err == nil && !st.IsDir() {
					// Vite 产出的文件名里带内容哈希，内容变则文件名变，
					// 所以可以放心 immutable。
					if strings.HasPrefix(clean, "/assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					}
					// 克隆请求再改路径：不修改调用方的 r，避免影响外层中间件
					// （请求日志里记的应该是用户真正请求的路径）。
					r2 := r.Clone(r.Context())
					r2.URL.Path = clean
					files.ServeHTTP(w, r2)
					return
				}
			}
		}

		// ---- 3. SPA 回落 ----
		//
		// ★ 例外：**带扩展名**却没命中的路径不回落，直接 404。
		//
		// 理由：`/assets/index-<hash>.js` 找不到时，如果回落到 index.html，
		// 浏览器会拿到一段 HTML 当 JS 执行 —— 报错信息是
		// "Unexpected token '<'"，而真正的原因（资源文件名对不上、
		// 或者发布时漏传了 assets）被完全掩盖。
		//
		// 代价：前端路由里不能出现带点的路径段。本项目的路由参数只有
		// 设备 ID 和会话 ID（都是 UUID，只含十六进制和连字符），
		// 所以这个代价是零。
		if path.Ext(clean) != "" {
			http.NotFound(w, r)
			return
		}

		serveIndex(w, distFS)
	})
}

// serveIndex 输出前端外壳页。
//
// ★ 只有 distBuilt 才输出内嵌的 index.html。其余三种状态一律输出
// 内置说明页 —— 包括 distIncomplete（index.html 是真的、资源却缺失）。
// 那种情况下伺服内嵌的 index.html 会让浏览器去请求不存在的 JS，
// 结果是白屏 + 控制台一堆 404，而用户完全不知道要跑 make web。
//
// 接受 fsys 参数是为了可测：distIncomplete 这条分支只在「资源缺失」时走到，
// 而真实的 distFS 要么完整要么是占位页，构造不出这个中间态 ——
// 那正是这个分支最容易腐坏、也最需要被测住的原因。
func serveIndex(w http.ResponseWriter, fsys fs.FS) {
	state, data := analyzeDist(fsys)
	if state != distBuilt {
		serveNotBuilt(w)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// ★ index.html 必须**不缓存**。
	//
	// 它是唯一一个文件名里没有内容哈希的产物。如果它被缓存，
	// 用户在前端发新版本后会继续拿到旧的外壳，而旧外壳引用的是
	// 已经被删掉的 `assets/index-<旧hash>.js` —— 结果是白屏，
	// 而且刷新也没用（缓存还在）。这个 bug 每次发布都会复现一次。
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// serveNotBuilt 输出「前端不可用」的说明页。
func serveNotBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 说明页同样不能缓存：用户跑完 make web 之后刷新就该看到真界面，
	// 而不是被缓存的提示页挡住。
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(notBuiltPage))
}
