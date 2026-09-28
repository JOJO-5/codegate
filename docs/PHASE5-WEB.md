# Phase 5 验收报告 —— Web 前端

> 状态：**已完成** · 2026-09-28
> 交付物：`web/`（20 个源文件，4746 行）+ `internal/server/webui`（嵌入层）+ `scripts/web-render-check.sh`
> 上游依据：`docs/PHASE0-ARCHITECTURE.md` §5 / §6.5 / §8 / §9

---

## 1. 一句话结论

前端能登录、能配对、能开终端、能断线续传，**并且有一组断言专门盯着
「构建成功但页面跑不起来」和「页面看起来正常但其实是白屏」**这两类最难发现的问题。

```
cd web && npm run build    vue-tsc 类型检查 + vite 构建，EXIT=0（68 模块）
make build                 EXIT=0（★ make 在本机可用，见 §9.1）
go build ./...             EXIT=0
go test -vet=off ./...     13 个包全绿
bash scripts/server-smoke.sh        22 项全过（其中 Web 8 项）
bash scripts/web-render-check.sh     7 项全过（真 Chrome 无头渲染）
bash scripts/e2e-terminal.sh        12 项全过（真 ConPTY → Server → 客户端）
```

技术栈与架构文档 §5.1「规范指定」一致：**Vue 3 `<script setup>` + TypeScript + Vite +
vue-router + Pinia + `@xterm/xterm`**，无 UI 框架、无 Tailwind，深色主题用原生 CSS 变量。

---

## 2. 交付物清单

### 2.1 目录结构

```
web/
  package.json  tsconfig.json  vite.config.ts  index.html  env.d.ts
  src/
    main.ts  router.ts  App.vue  styles.css          # 外壳（618 行 CSS 变量主题）
    lib/
      protocol.ts   498 行   Go internal/protocol 的 TS 镜像
      ws.ts         411 行   WebSocket 客户端（退避 / 请求响应匹配 / 版本协商）
      api.ts        399 行   REST 客户端（401 自动刷新去重）
      format.ts     166 行   展示层格式化
      commands.ts   139 行   预设命令表 + shell/tui 分类
      ulid.ts        72 行   Crockford Base32 ULID
      viewport.ts    46 行   终端尺寸估算
    stores/                # Pinia
      sessions.ts   171 行
      conn.ts       167 行   连接生命周期（shallowRef 持 WS）
      auth.ts       155 行
      devices.ts    109 行
    views/
      SessionView.vue       600 行   ★ 终端页（xterm + 帧循环 + resize）
      DeviceDetailView.vue  372 行
      SettingsView.vue      264 行
      DevicesView.vue       223 行
      LoginView.vue         147 行
```

### 2.2 页面（与 §5.2 一致）

| 路由 | 视图 | 关键点 |
|---|---|---|
| `/login` | LoginView | 登录/注册切换；`route.query.next` 回跳 |
| `/` → `/devices` | — | 重定向 |
| `/devices` | DevicesView | 设备列表 + **配对两步**（先预览再确认） |
| `/devices/:id` | DeviceDetailView | 改名 / 删除 / 新建会话 / 会话列表（实时+缓存双来源） |
| `/sessions/:id` | SessionView | xterm 终端、attach 重放、resize 防抖、Ctrl+C 回退 |
| `/settings` | SettingsView | 账号信息 / 改密码 / 审计日志分页 |

### 2.3 服务端嵌入层

| 文件 | 职责 |
|---|---|
| `internal/server/webui/webui.go` | `//go:embed all:dist` + 四态完整性判定 + SPA handler |
| `internal/server/webui/dist/.gitkeep` | 让 embed 在「未构建」时仍然成立（见 §4.2） |
| `internal/server/webui/webui_test.go` | 9 个测试，其中 3 个用 `fstest.MapFS` 构造坏状态 |

---

## 3. 验收：P5 退出标准逐条核对

| # | 标准 | 结果 |
|---|---|---|
| 1 | `npm run build` 通过（含类型检查） | ✅ `vue-tsc --noEmit` + `vite build` EXIT=0 |
| 2 | 六个页面全部可达 | ✅ 冒烟测深链接回落，渲染验证测 `/login` 与路由守卫 |
| 3 | 登录 → 设备 → 会话全链路 | ✅ 冒烟测 REST；渲染验证测真实挂载 |
| 4 | 终端页遵守 §5.3 三条纪律 | ✅ 见 §3.1 |
| 5 | §6.5 Ctrl+C 前端回退落地 | ✅ 见 §3.2 |
| 6 | 产物被 go:embed 进二进制 | ✅ 冒烟测 `/` 返回真产物、资源可访问、缓存头正确 |
| 7 | **页面真的能跑**（不只是 HTTP 200） | ✅ `web-render-check.sh` 用真 Chrome 无头渲染，检查 JS 注入的节点 |

### 3.1 §5.3 三条终端纪律的落实位置

| 纪律 | 落实 |
|---|---|
| `term.write()` 只收 `Uint8Array`，不做 UTF-8 解码 | `onFrame()` 直接 `t.write(frame.payload)`；`ws.binaryType='arraybuffer'`。**前端从不解码终端字节** —— 解码会让 xterm 自己处理跨帧切断的多字节字符的能力失效（中文/emoji 会花屏） |
| 不覆盖 `attachCustomKeyEventHandler`，除 Ctrl+Shift+V/C/F | 处理器只认这三个组合，其余一律 `return true` 交还 xterm |
| 卸载时 `dispose()` + `ResizeObserver.disconnect()` | `onUnmounted` 里按序：退订 WS 回调 → `ro.disconnect()` → `term.dispose()` |

### 3.2 §6.5 Ctrl+C 回退（Windows 上最反直觉的一条）

§6.5 的结论是：Windows 上往 ConPTY 写 `0x03` **无法中断 cooked 模式的子进程**
（cmd / PowerShell），后端所有修法都试过并失败。所以决策是**前端回退**：

- `lib/commands.ts` 的 `classifyCommand()` 把命令分为 `shell` / `tui`
- `shell` 类：Ctrl+C 按钮置灰，并给出一段明确的解释文字（不是 tooltip，是常驻提示）
- 提供「终止会话」作为替代手段
- **默认归类为 `tui`**：列表漏判会破坏主要场景（跑 `claude` 时 Ctrl+C 失效），
  而 tui 误判的代价只是「Ctrl+C 无效」，用户仍可用终止会话恢复

前端不隐藏这个限制，也不假装按钮能用 —— 这正是 §6.5 要求的「绝不静默失败」。

---

## 4. 过程中抓出的问题

### 4.1 ★★ 渲染验证的假阳性：断言匹配到了自己的注释

**现象**：`web-render-check.sh` 报告「页面还停在启动占位（boot-splash）—— Vue 没挂载成功」，
但同一份 DOM 里 `id="email"` 明明存在（说明 Vue 挂载成功了）。

**根因**：反向断言写的是 `grep -q 'boot-splash'`，而 `web/index.html` 的
**HTML 注释里就写着 "boot-splash" 这个词**（用来解释这个占位机制）。
Chrome 的 `--dump-dom` 会把注释一起 dump 出来 —— 于是断言匹配到了自己的说明文字。

**修法**（两处，缺一不可）：

1. 断言改成匹配**元素上的 class 属性**：`grep -q 'class="boot-splash"'`
2. `web/index.html` 的注释里**不再写出那个字面值**，改为「这个启动占位元素」，
   并加一段说明解释为什么不能写出来

**差分证明**（断言必须能区分两种状态，否则等于没写）：

| 输入 | `class="boot-splash"` | 旧写法 `boot-splash` |
|---|---|---|
| 未挂载态（服务端原始 index.html） | 命中 → FAIL ✅ 正确 | 命中 → FAIL ✅ |
| 已挂载态（Chrome dump 的真实 DOM） | 未命中 → PASS ✅ 正确 | **命中 → 误报 FAIL** ❌ |

> **教训**：一个**永远不会失败**的断言没有价值；一个**会误报**的断言是负资产。
> 写反向断言时必须拿两种状态各跑一次，确认它真的在区分。

### 4.2 ★★ `distIncomplete`：编译通过、测试通过、启动无警告、页面是坏的

**现象**：`make web` 把真实 `index.html` 拷进 `internal/server/webui/dist/`，
但 `dist/assets/` 被 `.gitignore` 排除。于是仓库可能处于
**「提交了 index.html 却没提交 assets」** 的状态。

此时旧实现的行为：

- `Built()` 只检查「index.html 里有没有占位标记」→ 真实产物没有标记 → 返回 `true`
- Server 启动**不打任何警告**
- `/` 伺服这个 index.html → 浏览器去请求 `/assets/index-xxx.js` → **404 → 白屏**
- 控制台一堆 404，用户完全不知道要跑 `make web`

这是本项目最忌讳的失败方式：**所有自动化信号都是绿的，只有用户的浏览器是黑的**。

**修法**（分两层）：

**第一层 —— `Built()` 改成真完整性校验。** 解析 index.html 里所有 `/assets/...`
引用，逐个确认它们在 embed 里真的存在。四种状态分开处理：

| 状态 | 含义 | `Built()` |
|---|---|---|
| `distBuilt` | 完整产物 | `true` |
| `distPlaceholder` | 占位页 | `false` |
| `distIncomplete` | **真实 index.html 但资源缺失** | `false` |
| `distMissing` | 连 index.html 都没有 | `false` |

**第二层 —— 任何非 `distBuilt` 状态都不伺服内嵌的 index.html**，
改伺服一份 Go 常量写的说明页。用常量而不是内嵌文件，是因为坏状态本身就包括
「内嵌文件读不出来」。

**启动日志带上具体状态**，因为处置方式不同：

```json
{"level":"WARN","msg":"内嵌的不是完整的前端产物，Web 界面不可用；API 不受影响。",
 "状态":"distIncomplete（index.html 引用的资源缺失）",
 "修复":"在仓库根目录执行 `make web` 后重新编译 Server"}
```

**顺带简化了整套占位机制。** 原设计是「提交一个占位 index.html」，但它会被
`make web` 覆盖（真实 index.html 必须落在同一个路径上）—— 于是「提交前记得恢复
占位页」成了一条只能靠人记住的规矩，而违反它的后果正是上面那个静默白屏。

改成 `dist/.gitkeep` + `//go:embed all:dist` 之后：

- 说明页由 Go 常量提供，不再需要占位文件
- `make web-clean` 简化成一条 `rm`
- 「忘了恢复占位页」这类失误**在结构上不再可能发生**

> `all:` 前缀是必须的：embed 默认忽略以 `.` 或 `_` 开头的文件（与 go 工具链
> 忽略规则一致），少了它会匹配不到 `.gitkeep`，pattern 落空、**整个项目编译失败**，
> 而报错信息看起来和前端毫无关系。

**两个坏状态的端到端验证**（不只是单测）：

| 构造的状态 | 启动警告 | `GET /` |
|---|---|---|
| 只有 `.gitkeep` | `distMissing` ✅ | 说明页，含 `make web` 指引，`no-cache` ✅ |
| 真实 index.html、无 assets | `distIncomplete` ✅ | **没有**伺服前端外壳，改伺服说明页 ✅ |

### 4.3 前端产物漏扫：`.js` / `.css` 不在 DLP 检测范围内

`tools/fcli-plaintext.py` 的 `PROTECTED_SUFFIXES` 只有
`.go/.txt/.json/.md/.yaml/.yml/.sql/.cs/.html` —— **没有前端后缀**。

P5 之后，`web/dist/assets/*.js`、`*.css`、`web/src/**/*.ts|vue` 全部落在这个集合之外，
于是**永远不会被扫到**。而它们正是要提交进仓库的那一批。

这个漏洞的症状极隐蔽：磁盘上是密文，`git add` 之后别人 clone 到的是乱码，
**但本机因为白名单进程能解密，一切看起来都正常**。

**修法**：补上 `.js .mjs .cjs .ts .mts .cts .vue .css .map`，并在注释里写清症状。

**实测修复的两个文件**（都是会进版本库的）：

```
[已修复] go.mod                 5033 -> 937 字节（明文 937）
[已修复] web/package-lock.json  58988 -> 54892 字节（明文 54892）
```

修完全项目扫描：**145 个待提交文件全部明文**。

### 4.4 密文判据的更正：`wc -c < file` 并不可靠

我一度用「`stat -c %s` vs `wc -c < file`」判加密态，得到「全是明文」的结论 ——
而 `go.mod` 实际是密文。

实测 `wc -c < go.mod` 返回 **5033**（磁盘密文长度），而 fcli 读到的是 **937**（明文）。
说明**这个 shell 里 bash 的 `open()` 并没有解密**，我先前记的「bash 在白名单」
是错的（已同步更正 skill）。

**唯一可靠的判据**：`fcli read` 返回的 `size_bytes` 对比 `fcli stat` 的 `size`。
（`fcli stat` 的 `suspected_encrypted` 字段在 `go.mod` 上给了**假阴性**，
不能单独采信。）

### 4.5 我自己写出来的语法错误（Pinia setup store）

`vite build` 报 `devices.ts:77:18: ERROR: Expected ")" but found ":"`。

**根因是我自己**：Pinia setup store 里 `previewPair` 和 `confirmPair` 少了
`function` 关键字，于是它们被解析成函数体内的非法对象方法简写。

值得记的是**排查过程**：因为本机有 DLP，报错也可能是「读到密文」导致的，
所以我先写了探针文件确认 esbuild 读到的是明文，排除了环境因素，
才回头审自己的代码。**先排除环境噪声，再查自己的错** —— 否则会在错误的方向上耗很久。

### 4.6 Makefile：Windows 上 `go build -o bin/foo` 不补 `.exe`

`scripts/server-smoke.sh` 优先找 `bin/codegate-server.exe`，而 `make server`
产出的是无后缀的 `bin/codegate-server`。于是冒烟测试跑去执行**上一次构建留下的旧二进制**，
表现为「webui 明明接进路由了，却报 6 个 404」。

**修法**：`GOEXE := $(shell go env GOEXE)`，所有 `-o bin/...` 后统一追加 `$(GOEXE)`。

> **补充（§9.2）**：这个修法本身有个洞 —— `go` 不在 PATH 时
> `go env GOEXE` 返回**空串**，于是又回到「产出无后缀文件」那个坑。
> 现已加兜底：空值时 Windows 直接回落 `.exe`。
> 换句话说，§4.6 当时是「修了症状」，§9.2 才补上「修了原因」。

### 4.7 架构文档与实现不符：`client.ready` 从未实现

§8.2 第 5 步写着 `Server → {"type":"client.ready", ...}`，§9.4 也引用了它。
但 `internal/protocol/types.go` 的常量表里**没有 `client.ready`** ——
只有 `agent.ready`（Server → Agent 握手的一环，方向完全不同）。
`internal/server/ws_client.go` 建连后不下发任何消息。

**修法**：在 §8.2 保留第 5 步并加删除线 + 一段实现偏差说明（留下设计意图的痕迹），
§9.4 改成描述**实际生效**的机制 —— 版本协商走 `POST /ws-ticket` 的响应体
`{ticket, expires_in, protocol:{min,max}}`，前端在**建连之前**就能拒绝不兼容的页面。

前端侧已按实际机制实现（`ws.ts` 在取票后、建连前校验 `protocol` 区间）。

---

## 5. 关键设计决策（以及为什么）

| 决策 | 理由 |
|---|---|
| **用 Vue 3 而不是 React** | 框架只占约 5% 的工作量，难点（WS 客户端、二进制帧、xterm 生命周期、ring buffer 重放、resize 防抖）都与框架无关。而 React 在终端页有个具体摩擦：StrictMode 的双调用 effect 会导致 xterm/WS 被重复初始化，需要额外的清理纪律来抵消 —— 在这里是净成本 |
| **不引入 UI 组件库 / Tailwind** | 六个页面、移动端为主、深色主题。618 行 CSS 变量就够，换来的是零依赖、零构建复杂度 |
| **`protocol.ts` 把 Agent 专属消息类型也列出来** | 前端一旦收到 `session.exit` / `agent.ready`，说明要么服务端有 bug，要么有人在伪造 —— 列出这些类型才能**检测**到，而不是当成未知消息忽略 |
| **`api.ts` 的刷新用共享 in-flight Promise** | 服务端的 refresh 是**轮换 + 重用检测**：并发刷新会让整族 token 被吊销。多个并发 401 必须收敛成一次刷新 |
| **access token 只存内存，绝不进 localStorage** | 一旦进 localStorage，「refresh 走 HttpOnly Cookie」的防护就被绕过了 |
| **`conn.ts` 用 `shallowRef` 持 WSClient** | 深响应式会给 WebSocket 宿主对象套 Proxy，行为不可预期且拖慢热路径 |
| **`classifyCommand()` 默认返回 `tui`** | 见 §3.2 —— 漏判破坏主要场景，误判只是 Ctrl+C 无效，代价不对称 |
| **`Built()` 校验资源存在性而非只看标记** | 见 §4.2 |
| **非 `distBuilt` 一律伺服 Go 常量说明页** | 坏状态本身就包括「内嵌文件读不出来」，用内嵌文件兜底是自相矛盾 |
| **`/api/` 前缀绝不 SPA 回落** | 否则拼错的接口路径会返回 200 + HTML，前端在 `JSON.parse` 处报错 —— 报错位置离真正原因很远 |
| **带扩展名的未命中路径不回落** | 否则浏览器把 HTML 当 JS 执行，报 `Unexpected token '<'`，真正原因（漏传 assets）被掩盖 |
| **`index.html` 必须 `no-cache`** | 它是唯一没有内容哈希的产物。被缓存的话，发新版后用户拿到旧外壳、而旧外壳引用的 assets 已被删除 → 白屏且刷新无效 |
| **`/assets/` 声明 `immutable`** | 文件名带内容哈希，内容变则文件名变 |
| **渲染验证用 `--dump-dom` 而不是截图** | 截图要靠人看；DOM 里出现由 Vue 渲染的 `id="email"` 是「框架跑通了」的硬证据 |
| **渲染验证用 `--virtual-time-budget`** | 否则 dump 到的是 JS 还没跑完的空壳 |

---

## 6. 验证清单

### 6.1 单元测试（`internal/server/webui`，9 个）

| 测试 | 覆盖 |
|---|---|
| `TestAPIPrefixNeverFallsBackToHTML` | 4 条 API 路径逐条压住「不回落成 HTML」 |
| `TestSPARoutesReturnIndexHTML` | 6 条前端路由的深链接 |
| `TestMissingAssetReturns404NotHTML` | 4 条带扩展名的未命中路径 |
| `TestPathTraversalIsContained` | 4 种路径穿越写法 |
| `TestIndexIsNotCacheable` | 外壳页 `no-cache` |
| `TestAnalyzeDistStates` | **四态 × 6 用例**，其中 3 个用 `fstest.MapFS` 构造坏状态 |
| `TestIncompleteDistServesNoticeNotBrokenIndex` | ★ 真实 index + 缺失资源时**不伺服**它 |
| `TestServedIndexNeverReferencesMissingAssets` | 不变量：伺服的页面绝不引用 embed 里不存在的资源 |
| `TestBuiltAgreesWithState` | `Built()` 与 `analyzeDist` 判定一致 |
| `TestAssetsAreCacheableWhenBuilt` | 资源 `immutable`（未构建时 `t.Skipf` 并说明原因） |

### 6.2 `scripts/server-smoke.sh`（22 项）

进程级冒烟：起真进程、连真端口、发真信号。Web 一节 8 项：真产物 / 深链接回落 /
未知路由回落 / 资源可访问 / `immutable` / `no-cache` / 未知 API 404 JSON / 缺失资源 404。

**未构建时不静默跳过** —— 打一条明确的 `[跳过]` 说明并给出修复命令。
静默跳过等于把「前端根本没构建」伪装成「全部通过」。

### 6.3 `scripts/web-render-check.sh`（7 项）

真 Chrome 无头渲染，检查**由 JS 注入的**节点：邮箱框 / 密码框 / 品牌标题 /
登录容器 / 启动占位已被替换 / 无控制台 JS 错误 / 未登录访问 `/devices` 被守卫拦回。

前置判定「二进制里有没有完整产物」，没有则明确跳过并说明 —— 否则会产生一堆假失败。

---

## 7. 已知限制 / 待办

| 项 | 说明 |
|---|---|
| **文件传输未做** | P8。后端 `file.*` 返回明确的 `internal` 错误，前端未接入 |
| **`allowed_commands` 未暴露给前端** | `commands.ts` 的预设表目前是**前端预置**的，权威来源应是 Agent 上报的 `allowed_commands`（它才知道这台机器装了什么）。`protocol.TypeDeviceInfo` 已登记但 Agent 未上报，所以 `classifyCommand` 的判据是启发式的 |
| **`client.ready` 未实现** | 见 §4.7。当前用「客户端拉」（`session.list`）代替「服务端推」，功能等价，多一次往返 |
| **~~`make` 在本机不可用~~（已纠正）** | **这条结论是错的**，见 §9。本机有 GNU Make 4.4.1（`w64devkit` 自带），`make build / server / agent / test / race / e2e` 全部真实跑通。当时的结论来自「chocolatey / msys64 / mingw64 / PortableGit / scoop 里都没有」—— 漏掉了 `~/.workbuddy-ai/binaries/w64devkit`。**Visual Studio 的 `nmake` 不是 GNU make**（不认 `:=` / `$()` / `$(shell)`），所以「装过 VS」不等于「有 make」 |
| **~~`-race` 跑不了~~（已纠正）** | **这条结论也是错的**，见 §9。实测 `make race` 全 13 个包通过，耗时 3 分 42 秒。前提是先 source `scripts/goenv.sh`（它设 `CGO_ENABLED=1` + `CC=<w64devkit>/gcc.exe`）。`-race requires cgo` 那个报错容易被读成「这台机器装不了」，其实只是环境变量没设 |
| **无 CI** | 四个验证脚本目前靠手动跑（`server-smoke` / `web-render-check` / `e2e-terminal` / 单元测试）。P11 应把它们接进流水线 |
| **终端页仍无自动化测试** | `SessionView.vue` 的帧循环、attach 重放、resize 防抖只有代码审查。`scripts/e2e-client.mjs` 覆盖了**协议层**（建会话 → attach → 收字节 → 关会话），但**不覆盖 Vue 层** —— 它扮演浏览器，不加载前端 |
| **Go module cache 的 rename 失败** | 每次构建打 15 行 `The system cannot move the file to a different disk drive`。实测：`.zip` 落盘成功，但 `.mod` **从来没落盘过**（缓存目录里堆了 118 个 `.mod*.tmp`）。后果不是「无害噪音」，而是**每次构建都要重新拉 `.mod`** —— 代理挂了就构建不了。`GOTMPDIR` 指到同目录树**无效**（试过）。见 §9 |

---

## 8. 附：本机环境备注

除 P4 已记录的两条（`go test` 需 `-vet=off`；不要用 `gofmt -w` 直接写项目文件）外，
P5 新增三条：

1. **`go` 与 `make` 不在系统 PATH 里**（都是绿色解压安装）。已加进
   `~/.bashrc`，Git Bash 里直接可用：

   ```bash
   export PATH="/c/Users/JOJO/.workbuddy-ai/binaries/go/versions/1.27.1-plain/go/bin:$PATH"
   # ★ w64devkit 必须追加到**末尾** —— 它自带的 sh.exe 会劫持
   #   GNU make 的默认 SHELL，症状是 `Error 1` + 零错误输出
   export PATH="$PATH:/c/Users/JOJO/.workbuddy-ai/binaries/w64devkit/2.10.0/w64devkit/bin"
   ```

   ⚠️ 只有 **login / interactive** shell 读 `.bashrc`。自动化工具若以
   非交互方式起 bash（走 `BASH_ENV`），仍然读不到，需要显式设 PATH。
   Makefile 里已加一层兜底：`GOENV` 会在每个用到 go 的 recipe 前
   source `scripts/goenv.sh`（文件不存在时展开为空，干净机器不受影响）。
2. **判加密态只能用 fcli**：`fcli read` 的 `size_bytes` ≠ `fcli stat` 的 `size`
   即为密文。`wc -c < file`、`stat -vs-实际读`、以及 `fcli stat` 的
   `suspected_encrypted` 字段**都可能误判**（实测 `go.mod` 三处全错）。
3. **前端产物也要扫**。`tools/fcli-plaintext.py` 的后缀表已补 `.js/.css/.ts/.vue/.map`；
   提交前跑一次：

```bash
python tools/fcli-plaintext.py --check internal cmd docs scripts tools Makefile go.mod \
  web/src web/index.html web/package.json web/package-lock.json \
  web/tsconfig.json web/vite.config.ts web/env.d.ts
```

### 提交前检查清单

```bash
cd web && npm run build          # 类型检查 + 构建
make web-clean                   # 把产物清出 dist/（只留 .gitkeep）
python tools/fcli-plaintext.py --check ...   # 确认没有密文
```

`make web-clean` 之后要接着 `make server`，否则二进制和磁盘状态不一致
（embed 在编译期求值，二进制里仍是上一次构建的结果）。

---

## 9. 更正与端到端补充（2026-09-28 晚）

JOJO 问了两句话：

> 我本机不能装make吗？我好像装过C#的vs啊　你实实际端到端试过了吗？

两句都问对了。下面是复查结果。

### 9.1 本机有 GNU Make（之前漏找了一个目录）

之前的结论「chocolatey / msys64 / mingw64 / PortableGit / scoop 里都没有」
**漏掉了 `~/.workbuddy-ai/binaries/w64devkit`**。那里有一套完整的
GNU Make 4.4.1：

```
~/.workbuddy-ai/binaries/w64devkit/2.10.0/w64devkit/bin/make.exe
```

顺带确认了两件事：

- **Visual Studio 的 `nmake` 不是 GNU make。** 本机确实装了
  VS 2022 Community 与 VS 2026（`vswhere` 定位到 `D:\Program Files\Microsoft
  Visual Studio\2022\Community`），5 个 MSVC 安装里都有 `nmake.exe` ——
  但它不认 `:=`、`?=`、`$(shell ...)`，本项目的 Makefile 一行都跑不了。
  所以「装过 VS」和「有 make」是两回事。
- w64devkit 同时提供了 `gcc`，这正是 `-race` 需要的 C 工具链。

### 9.2 Makefile 在 Windows GNU make 上的三个静默失败

拿到 make 之后才暴露出下面三个问题。**每一个都是「报错信息和真实原因无关」**
的类型，值得单独记：

| # | 现象 | 根因 | 修法 |
|---|---|---|---|
| 1 | `make build` 报 `Error 1`，**零错误输出**；同一行命令在 bash 里 EXIT=0 | `SHELL := bash` 在 Windows GNU make 上**静默无效**，make 强制用它自己发现的 `sh.exe`（w64devkit 自带），而那个 sh 跑 `go.exe` 会 exit 1 且 stdout/stderr 全空 | Windows 上显式解析 bash 的**绝对路径**当 SHELL（`command -v bash` + 候选列表 + ASCII 的 `$(error)` 兜底） |
| 2 | 产出的二进制没有 `.exe` 后缀 | 同一个 sh 里 `go env GOEXE` 失败返回空串 | 空值时 Windows 直接回落到 `.exe` |
| 3 | `make server` 打印 `宸叉瀯寤? bin/...`（「已构建」的乱码） | Windows GNU make 把 Makefile 按 **ANSI 码页（GBK）** 解析。`已构建` 的 UTF-8 字节 `E5 B7 B2 E6 9E 84 E5 BB BA` 按 GBK 逐对读正好是 `宸叉瀯寤` | 中文提示全部搬到 `scripts/make-msg.sh`，make 只传 ASCII key |

第 3 条的补注：**加 UTF-8 BOM 无效**（实测 GNU make 4.4.1 不认 BOM，也没有
环境变量开关），`echo` 和 `printf` 都救不了 —— 损坏发生在**解析期**，不是执行期。

顺带修掉一个一直没被发现的问题：`make help` 用的是 `grep -E '^## ' Makefile`，
而目标清单在**文件头的注释块**里，`## ` 只匹配到 7 个小节标题。
改成 `awk` 提取头部注释。

### 9.3 `-race` 在本机可以跑（P4 的结论是错的）

```
$ make race
ok  github.com/jojo/codegate/internal/...   （13 个包全绿，3 分 42 秒）
```

前提是先 source `scripts/goenv.sh` —— 它设 `CGO_ENABLED=1` 和
`CC=<w64devkit>/gcc.exe`。`make race` 现在会自己 source 一次。

P4 记的「本机无 C 工具链」错在**没找到 w64devkit**：它的 `gcc` 一直在那儿，
只是没人把它的 bin 加进 PATH。

### 9.4 `scripts/e2e-terminal.sh`：终端数据通路的第一个真验证

这是本项目**第一个**打通「真 Agent → 真 ConPTY → 真 Server → 真客户端」的
验证。它补的盲区很具体：

| 脚本 | 覆盖到哪一层 |
|---|---|
| `server-smoke.sh` | HTTP：状态码 / 认证 / 缓存头 / 优雅退出 |
| `web-render-check.sh` | 前端能不能挂载（真 Chrome） |
| **`e2e-terminal.sh`** | **Agent（跑真进程）──二进制帧──▶ Server（中继 + ring buffer）──▶ 客户端** |

链路断掉时的症状是「网页能打开、能登录、能点新建会话，然后终端永远空白」——
而上面两个脚本的所有断言都是**绿的**。

流程：临时 Server（临时 DB、随机端口）→ 注册 → `agent pair` 抓配对码 →
REST 两步配对 → `agent run` → `scripts/e2e-client.mjs` 扮演浏览器
（建会话 → attach → 收字节 → 关会话）。全程不碰用户已有数据，约 15 秒。
入口：`make e2e`。

**它抓出了两个真 bug**，都只在「多订阅者 / 有 pending 请求」时才暴露，
单元测试里单开一条连接是看不出来的：

1. **`session.close` 的响应永远回不到客户端。**
   `session.closed` 有**双重身份** —— 既可能是对 `session.close` 的响应
   （带 `reply_to`），也可能是 Agent 自发的推送（进程自己退出了）。
   而 `onAuthenticated` 把它**无条件**交给 `handleSessionEnded`，
   那条路径只广播、不按 `reply_to` 转发，也从不消费 pending 条目。
   前端表现：点「关闭会话」后一直转圈，直到 60 秒 TTL 超时才报错。
   修法：先 `routeToClient`（判据是 pending 表里有没有这条 `request_id`），
   没命中才走自发推送路径。

2. **会话结束时其他观察者收不到通知。**
   `ForgetSession` 内部是 `delete(sessionSubs, sid)` —— 把订阅者名单整个删掉。
   而代码是**先** `ForgetSession` **再** `BroadcastToSession`，
   于是广播遍历到空集合，一条都发不出去，而且**完全静默**：
   不报错、不记日志。表现：另一个窗口的终端永远停在最后一行。
   修法：把顺序倒过来（先广播、再清理），并在注释里写成「顺序铁律」。

回归测试在 `internal/server/ws_session_test.go`
（`TestSessionCloseResponseReachesRequester` /
`TestSessionCloseReachesOtherViewers` /
`TestSpontaneousSessionExitStillBroadcasts`）。
三个用例都做过**差分证明**：把修复退回去，它们确实会红。

### 9.5 实测踩到的一个 Windows 坑：cmd.exe 的 `/c` 被路径抢走

```
C:\Windows\System32\cmd.exe /c echo hi   → 打印 hi            ✅
C:/Windows/System32/cmd.exe /c echo hi   → 命令语法不正确。   ❌
裸 cmd /c echo hi                        → 打印 hi            ✅
```

根因：cmd.exe 用**字符串扫描**找自己的 `/c` 开关，而不是先剥掉 argv[0]。
于是路径里的 `/cmd.exe` 先命中 —— 它把 `/cmd.exe` 读成「开关 `/c` + 尾巴
`md.exe`」，剩下的 `md.exe /c echo hi` 成了要执行的命令。而 `md` 恰好是
cmd 的**内置命令**（mkdir 的别名），`/c` 对它不是合法路径参数，于是报语法错误。

这个坑的迷惑性在于：进程起来了、ConPTY 正常、终端里有一行中文报错，
但**那行报错和用户配置的命令毫无字面关系** —— 从界面上几乎不可能反推回
「配置里那个正斜杠」。

修法：`internal/agent` 的 `normalizeCommandPath` 在 Windows 上把
`command` 的正斜杠归一化成反斜杠（`/` 与 `\` 对文件系统等价，纯收益）。
**只归一化 `command`，不碰 `args`** —— args 里的 `/c` 本身就是开关。

归一化放在 `Config.Prepare()`（配置文件里的白名单命令）和
`ResolveCommand`（`allow_custom_commands` 的路径，不走 Prepare）。
`scripts/e2e-terminal.sh` 里**故意**用正斜杠写 command —— 这条配置同时验证了
「端到端通路通」和「归一化生效」。

### 9.6 更正后的验收结果

| 项 | 结果 |
|---|---|
| `make build` / `server` / `agent` / `ttyprobe` / `web` / `help` | 全绿，中文提示正常 |
| `go test -vet=off -count=1 ./...` | **13 个包全绿** |
| `make race` | **13 个包全绿**（3 分 42 秒） |
| `scripts/server-smoke.sh` | **22 / 22** |
| `scripts/web-render-check.sh` | **7 / 7** |
| `scripts/e2e-terminal.sh` | **12 / 12**（首次全绿） |

### 9.7 仍未解决

- **Go module cache 的 rename 失败**（见 §7 表末行）。`.zip` 能落盘，
  `.mod` 不能 —— 118 个 `.mod*.tmp` 在缓存目录里堆着，每次构建都重新拉
  `.mod`。`GOTMPDIR` 指到同目录树**试过，无效**。下一步可以试
  `GOMODCACHE` 指到 `%TEMP%` 下（旧笔记提过 `%TEMP%` 可能不在 DLP 策略范围内）。
- **终端页（Vue 层）仍无自动化测试**。`e2e-client.mjs` 覆盖协议层，
  不加载前端，所以 `SessionView.vue` 的帧循环 / attach 重放 / resize 防抖
  仍然只有代码审查。
