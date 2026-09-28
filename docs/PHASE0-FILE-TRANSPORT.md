# CodeGate — Workspace 文件传输设计（浏览 / 预览 / 下载 / 上传）

> 状态：Draft v1 · 2026-09-28 · 提出人：JOJO
> 关联：`docs/PHASE0-ARCHITECTURE.md` §18（Workspace）、§33（Relay 原则）
> **这是对 MVP 范围的一次扩展**，规范第 30 条原本把"文件管理"列在不做清单里。本文定义允许做的**最小子集**。

---

## 0. 一句话

**这不是文件管理器，是 Workspace File Transport。**

和 PTY 传输在架构上是**同一件事**：把本机的一个字节源，经 Server 中转，搬到浏览器；反过来也一样。

| | PTY 传输 | 文件传输 |
|---|---|---|
| 字节源 | PTY 的输出 | 磁盘上的一个文件 |
| 标识 | SessionID | TransferID |
| 分块 | 按 PTY 的读粒度（任意） | 按固定块（默认 64 KB） |
| 生命周期 | 进程活着就一直在 | 传输结束就完 |
| Server 的角色 | 透明中继 | **完全一样** |

所以复用是自然的：同一条 WS、同一套鉴权、同一个 Relay、同一套背压策略。

---

## 1. 边界声明（防止它长成 IDE）

JOJO 要的是四件事：**能看文件、能预览、能下载、能上传**。就这四件。

| 做 | 不做 |
|---|---|
| 列目录（懒加载） | 文件编辑、保存 |
| 看单个文件的元信息 | 重命名、删除、移动、复制 |
| 预览文本 / 图片 | 权限、所有者、时间戳修改 |
| 下载到本地 | 全盘搜索 |
| 上传（写进 allowed roots） | 解压、打包、断点续传 |
| — | 与 PTY 的 cwd 联动（未来可做） |
| — | 代码编辑器、Git GUI |

**为什么这条边界必须写死**：文件 API 一旦开了"写"的口子，加"编辑"只要再加一个接口，加"删除"也一样 —— 半年后它就变成 Web IDE 了，而那正是规范第 30 条明确排除的东西。**要加，得单独提出来讨论。**

---

## 2. 关键决策

| # | 决策点 | 推荐方案 | 备选 | 理由 |
|---|---|---|---|---|
| F1 | 走哪条连接 | **复用已有的 client WebSocket** | 单开 `/ws/files` | 已有鉴权、Relay、重连、限流。单开一条等于把握手/鉴权/重连/限流全部重写一遍，且要维护两套连接状态。**代价**：必须做队列隔离，见 F4 |
| F2 | 16 字节标识字段 | **把它从 "SessionID" 上升为 "StreamID"** —— 终端帧里是 SessionID，文件帧里是 TransferID | 新增字段 / 改帧头布局 | 字节布局零改动，解析器不用动，只是协议语义的定义变了。改帧头要递增协议版本，不值得 |
| F3 | 字节走 JSON 还是二进制帧 | **二进制帧**，新增 `0x10 FILE_DATA` | Base64 塞进 JSON | 和终端数据同一个理由：+33% 体积、多一次编解码、无法表达任意字节 |
| F4 | 与终端数据抢带宽 | **独立队列 + 独立 ack 窗口**，终端帧优先 | 共用一条队列 | 下载一个 50 MB 的文件时，用户还在敲键盘。共用队列会让按键延迟飙到秒级 —— 这是不可接受的 |
| F5 | 流控 | **ack 窗口**（在途最多 8 块） | 无流控 / 定时器节流 | ack 窗口是最简可靠的方案：天然适配任何速度的消费者，不需要预估带宽，也不需要实现滑动窗口的复杂度 |
| F6 | 预览怎么判"能不能预览" | Agent 读前 64 KB，**只看有没有 NUL 字节 + 探测编码** | 前端猜 / 全量读 | 这是传输层必要的判断，不是"理解业务"。**不做语法解析、不做高亮、不做格式化** —— 那些是前端的事 |
| F7 | 上传写入方式 | **写临时文件 → 原子改名** | 直接写目标 | 直接写目标，中途失败就留下半个文件，把用户原来的文件毁了 |
| F8 | 上传覆盖 | 默认**拒绝覆盖**，要覆盖得显式传 `overwrite: true` | 默认覆盖 | 远程界面误操作的概率远高于本地；多一次确认的成本很低 |
| F9 | Server 是否落盘 | **绝不落盘、绝不缓存整文件** | 临时文件中转 | 「不上传服务器」是 JOJO 的明确要求，也是这个设计的卖点。见 §6 |
| F10 | 实现阶段 | **新增 Phase 8**（原 P8–P10 顺延为 P9–P11） | 塞进现有 Phase | 它依赖 P3（workspace 校验）、P4（relay）、P5（前端），插在 P7 之后最自然；且能赶在 P9 安全加固之前，被安全测试覆盖 |

---

## 3. 传输模型

### 3.1 StreamID 泛化

20 字节帧头保持不变：

```
┌─────────┬────────┬────────┬──────────────────────┬───────────────┐
│ Version │  Type  │ Flags  │      StreamID        │    Payload    │
│  1 byte │ 1 byte │ 2 byte │      16 bytes        │    N bytes    │
└─────────┴────────┴────────┴──────────────────────┴───────────────┘
```

那 16 字节的含义**按 Type 解释**：

| Type | StreamID 的含义 |
|---|---|
| `0x01` STDIN / `0x02` STDOUT / `0x03` BUFFER | SessionID |
| `0x10` FILE_DATA | TransferID |

**这不是 hack，是把字段的本意说清楚** —— 它本来就是"这一帧属于哪个流"。终端和文件都是流。

### 3.2 帧类型扩展

| 值 | 名称 | 方向 | 说明 |
|---|---|---|---|
| `0x01` | STDIN | C → A | 已有 |
| `0x02` | STDOUT | A → C | 已有 |
| `0x03` | BUFFER | A → C | 已有 |
| `0x10` | **FILE_DATA** | **双向** | 文件字节块 |
| `0x11` | **FILE_FINAL** | 双向 | 最后一块（也可以不用，见下） |

Flags 新增：

| 位 | 名称 | 说明 |
|---|---|---|
| bit0 | `BUFFER_END` | 已有 |
| bit1 | `DROPPED` | 已有 |
| bit2 | **`FINAL`** | 本帧是该传输的最后一块 |

**推荐用 flag 而不是单独的帧类型**（`0x11`）—— 少一个类型就少一处分支，而且 `FINAL` 天然可以和别的 flag 组合。

### 3.3 ★ 每传输方向锁（必须实现）

`FILE_DATA` 是**双向**的，静态方向表表达不了。所以：

```go
// internal/server/filetransfer.go
type transferKind uint8
const (
	kindDownload transferKind = iota // Agent 读盘 → 浏览器
	kindUpload                       // 浏览器 → Agent 写盘
)

type transfer struct {
	ID        uuid.UUID
	Kind      transferKind
	DeviceID  uuid.UUID
	UserID    uuid.UUID
	Path      string
	Size      int64
	Bytes     int64   // 已传输
	Window    int     // 在途未 ack 的块数
	CreatedAt time.Time
}
```

Server 收到 `FILE_DATA` 时：
1. 用 StreamID 查出 transfer
2. **校验发出方向与 transfer.Kind 一致**（下载：只能 Agent 发；上传：只能浏览器发）
3. 方向不符 → **立即断开连接 + 审计**

> 这条不能省。没有它，一个被 XSS 拿到的浏览器会话就能往用户磁盘上任意写文件（只要它猜到一个 download 的 transfer_id）。

---

## 4. 协议消息

### 4.1 新增控制消息

| type | 方向 | payload | 说明 |
|---|---|---|---|
| `file.list` | B→S→A | `{path}` | 列目录 |
| `file.listed` | A→S→B | `{path, entries[]}` | 目录内容 |
| `file.stat` | B→S→A | `{path}` | 元信息 |
| `file.stat.result` | A→S→B | `{path, size, mtime, mode, is_dir, is_symlink, is_binary, encoding}` | |
| `file.read` | B→S→A | `{path, offset, length}` | 请求下载/预览 |
| `file.read.begin` | A→S→B | `{transfer_id, size, is_binary, encoding, mtime}` | 传输即将开始 |
| `file.ack` | 双向 | `{transfer_id, received_bytes}` | 流控窗口 |
| `file.write` | B→S→A | `{path, size, overwrite}` | 请求上传 |
| `file.write.ready` | A→S→B | `{transfer_id}` | Agent 已建好临时文件，可以发字节了 |
| `file.write.done` | A→S→B | `{path, size, sha256}` | 落盘完成 |
| `file.cancel` | 双向 | `{transfer_id, reason}` | 取消传输 |
| `file.error` | 双向 | `{transfer_id, code, message}` | 复用 `ErrorPayload` |

**目录项结构**：

```go
type FileEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`      // 相对 allowed root 的路径，不是绝对路径
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`  // Unix ms
	Mode     string `json:"mode,omitempty"`
	Symlink  bool   `json:"symlink,omitempty"`
}
```

> **`Path` 返回相对路径**，不回绝对路径。理由：绝对路径会暴露用户名、盘符结构；而且相对路径正好是后续所有 API 的入参格式，前端不用做转换。

### 4.2 下载时序

```
Browser                    Server                       Agent
   │  file.read{path,off,len} │                            │
   ├─────────────────────────>├───────────────────────────>│
   │                          │        file.read.begin     │  打开文件
   │                          │<───────────────────────────┤
   │     file.read.begin      │                            │
   │<─────────────────────────┤                            │
   │                          │                            │
   │                          │   FILE_DATA (chunk 1..8)   │  在途窗口 8
   │                          │<───────────────────────────┤
   │   FILE_DATA (透传)        │                            │
   │<─────────────────────────┤                            │
   │  file.ack{received_bytes}│                            │
   ├─────────────────────────>├───────────────────────────>│  窗口 -1，补发一块
   │          ...重复...       │                            │
   │                          │   FILE_DATA[FINAL]         │
   │                          │<───────────────────────────┤
   │   FILE_DATA[FINAL]       │                            │
   │<─────────────────────────┤                            │
```

### 4.3 上传时序

```
Browser                    Server                       Agent
   │ file.write{path,size,overwrite}                        │
   ├─────────────────────────>├───────────────────────────>│
   │                          │      file.write.ready      │  校验路径
   │      file.write.ready    │                            │  建临时文件
   │<─────────────────────────┤                            │
   │   FILE_DATA (chunk 1..8) │                            │
   ├─────────────────────────>├───────────────────────────>│  append
   │  file.ack                │                            │
   │<─────────────────────────┤<───────────────────────────┤
   │          ...重复...       │                            │
   │   FILE_DATA[FINAL]       │                            │
   ├─────────────────────────>├───────────────────────────>│  原子改名
   │                          │      file.write.done       │
   │      file.write.done     │                            │
   │<─────────────────────────┤                            │
```

**注意**：`file.ack` 在上传时由 **Agent** 发（它才是接收方），Server 透传给浏览器。

---

## 5. 流控（ack 窗口）

**规则**：发送方在途未确认的块数 ≤ `window`（默认 8，即 512 KB）。

- 每发一块，`window--`
- 收到 `file.ack{received_bytes}`，按已确认的块数 `window += n`
- `window == 0` 时**暂停发送**，等 ack
- 超时（默认 30s）无 ack → 判定对端卡死 → 取消传输 + 通知双方

**为什么选 ack 窗口而不是别的**：

| 方案 | 问题 |
|---|---|
| 无流控，能发多快发多快 | 浏览器慢 → Server 内存涨 → 丢帧 → 文件损坏。**不可接受** |
| 固定速率节流 | 要预估带宽；网好的时候浪费，网差的时候还是堵 |
| 滑动窗口（TCP 式） | 功能对，但要实现拥塞控制、重传、乱序重组 —— **为 MVP 过度设计** |
| **ack 窗口** | ✅ 最简、天然自适应、不依赖带宽估计。慢就慢，但一定不坏 |

**块大小 64 KB 的理由**：单块最大不超过 `max_frame_size`（默认 64 KB），所以一块正好一帧，不用在块和帧之间再做切分。**简单优先。**

---

## 6. 「不上传服务器」的落地保证

JOJO 的原话是"不上传服务器，只是传输字节"。落到代码上是四条**可检查**的约束：

| # | 约束 | 怎么保证 |
|---|---|---|
| 1 | Server **不落盘** | `internal/server` 里禁止出现任何写文件的调用。**在 CI 里加一条 lint 规则**：该包 import `os` 后不允许出现 `os.Create` / `os.OpenFile` / `os.WriteFile`（测试文件除外） |
| 2 | Server **不缓存整文件** | 转发路径上只持有**当前一帧**（≤ 64 KB）。不允许出现 `bytes.Buffer` 累积整个文件 |
| 3 | Server **不解析内容** | 除了读 20 字节帧头做路由，不碰 payload。代码评审红线：relay 路径里不允许出现 `string(payload)` |
| 4 | 日志**不记录内容** | 运行日志不记文件内容；**文件名只写进 `audit_logs`**（追溯需要），不写进普通日志 |

**第 4 条的理由值得说明**：文件名本身可能很敏感（`裁员名单.xlsx`、`私钥.pem`）。但完全不记录又无法追溯"谁在什么时候下载了什么"。折中是：**审计表记路径，运行日志不记** —— 审计表有访问控制，运行日志会被随手贴进 issue。

**这条约束的实际价值**：即使 Server 被攻破或内存被 dump，攻击者也只能拿到**瞬时的一块 64 KB**，拿不到完整文件。这是"不上传服务器"这句话真正的安全含义。

---

## 7. 安全模型（本节最重要）

### 7.1 一个必须先说清楚的对比

| | PTY 路径 | 文件 API 路径 |
|---|---|---|
| 谁在"决定"要读这个文件 | **用户自己在 shell 里敲的** | **远程界面上点出来的** |
| CodeGate 该不该拦 | 不该。用户 `cd /etc && cat passwd` 是他操作自己的机器 | **必须拦**。远程请求直接读磁盘，没有"用户自己敲"这层语义 |
| 校验强度 | cwd 只是"默认起点" | **必须走 allowed_workspaces 白名单** |

**所以**：用户在 PTY 里绕过文件白名单是**可接受的**（等价于他坐在电脑前操作）；而文件 API 的白名单是防"远程界面被误用、被钓鱼、被 XSS 利用"，不是防用户自己。

这条要写进 `SECURITY.md`，否则以后会有人觉得"PTY 都能读，为什么文件 API 不让读"，然后把校验删掉。

### 7.2 路径校验：只写一次

**必须复用同一个函数**，PTY 的 cwd 校验和文件 API 用同一个：

```go
// internal/agent/workspace.go
//
// ResolveInRoot 把一个用户提供的相对路径解析成绝对路径，
// 并保证结果落在 roots 中的某一个之内。
//
// 拒绝：.. 穿越、符号链接逃逸、\\?\ 与 \\.\ 前缀、UNC、8.3 短名、
// 备用数据流（file:stream）、空字节、超长路径。
func ResolveInRoot(roots []string, rel string) (abs string, err error)
```

**为什么强调"只写一次"**：两处实现必然漂移。第一版可能一样，第二版有人修了 cwd 那处的 bug 忘了文件 API —— 于是文件 API 就成了绕过口。**安全校验代码的重复是漏洞的温床。**

**Windows 特有的坑**（全部要测）：

| 输入 | 期望 |
|---|---|
| `../../Windows/System32/config` | 拒绝 |
| `D:\Projects\..\..\Windows` | 拒绝 |
| `\\?\C:\Windows` | 拒绝（`\\?\` 绕过路径规范化） |
| `\\.\PhysicalDrive0` | 拒绝（设备命名空间） |
| `\\server\share\x` | 拒绝（UNC） |
| `C:\PROGRA~1\x` | 拒绝（8.3 短名） |
| `file.txt:secret` | 拒绝（NTFS 备用数据流） |
| `link_to_etc\passwd`（link 指向 root 外） | 拒绝（`EvalSymlinks` 后再校验） |
| `C:\Projects` vs `c:\projects` | 大小写不敏感比较（统一 lowercase 后比前缀） |

### 7.3 其他限制

| 项 | 默认值 | 说明 |
|---|---|---|
| 预览字节数 | 64 KB | 超过就只给前 64 KB + `truncated: true` |
| 单文件下载上限 | 100 MB | 可配 `max_file_size` |
| 单文件上传上限 | 100 MB | 同上 |
| 上传默认覆盖 | **否** | 要覆盖显式传 `overwrite: true` |
| 同时进行的传输数 | 每连接 3 | 防开 100 个下载把连接打爆 |
| 限流 | 独立令牌桶 | 文件操作 60 次/分钟/用户 |
| 审计 | 全部记录 | `file.list` / `file.read` / `file.write` 都写 `audit_logs`（含路径、不含内容） |

### 7.4 安全测试清单（并入 Phase 9）

| # | 测试 | 期望 |
|---|---|---|
| FS1 | 路径含 `..` | 拒绝 |
| FS2 | 符号链接指向 root 外 | 拒绝 |
| FS3 | `\\?\C:\Windows` / UNC / 8.3 / ADS | 拒绝 |
| FS4 | 用户 A 读用户 B 设备的文件 | `forbidden` |
| FS5 | 浏览器发 `FILE_DATA` 到一个 **download** 类型的 transfer | **断开连接**（方向锁） |
| FS6 | 上传覆盖已存在文件但不带 `overwrite` | 拒绝 |
| FS7 | 上传 200 MB 文件 | 拒绝（超上限） |
| FS8 | 传输中途断开，检查 Agent 侧 | 临时文件被清理，目标文件未被破坏 |
| FS9 | 上传后校验 sha256 | 一致 |
| FS10 | 在 `internal/server` 里 grep 写文件调用 | 零命中 |

---

## 8. 前端设计

### 8.1 位置

设备详情页新增一个 **Files** 标签页（和 Sessions 并列）。不单独做一级页面 —— 文件是"这台设备的"，跟会话是同一个层级。

### 8.2 布局

```
┌─────────────────────────────────────────────────────────┐
│ JOJO-PC   ● Online    Windows 11   Agent 0.1.0          │
├─────────────────────────────────────────────────────────┤
│  [ Sessions ]  [ Files ]                                │
├──────────────────┬──────────────────────────────────────┤
│ 📁 Projects      │  src/main.go            2.4 KB       │
│   📁 CodeGate    │  2026-09-28 14:22                    │
│     📁 internal  │  ────────────────────────────────    │
│       📄 main.go │  package main                        │
│     📄 go.mod    │                                       │
│   📁 TestTools   │  import (                             │
│ 📄 README.md     │      "fmt"                            │
│                  │  )                                    │
│                  │                                       │
│  [上传]           │  [下载]  [复制路径]                    │
└──────────────────┴──────────────────────────────────────┘
```

- 左：懒加载目录树。**点开才请求**，不预加载整棵树
- 右：预览面板
  - 文本 → 等宽字体，前 64 KB
  - 图片（png/jpg/gif/webp/svg）→ `<img>`（blob URL）
  - 其他 → "此文件不可预览" + 下载按钮

### 8.3 移动端（Q5 优先级高，这里同样要管）

| 项 | 处理 |
|---|---|
| 布局 | 窄屏改成"列表 → 点进文件 → 全屏预览"，不做左右分栏 |
| 下载 | iOS Safari 对 `Blob` + `<a download>` 支持有限 → 优先用 **Web Share API**（`navigator.share({files})`），退到 `a.download`。**必须真机实测** |
| 上传 | `<input type="file">` 能调起系统文件选择器（含相册、iCloud Drive） |
| 大文件 | 不要一次 `arrayBuffer()` 全载入内存 —— 手机内存小。用 `File.slice()` 分块读，和上传的块协议天然对上 |

### 8.4 进度与取消

- 传输中显示进度条（`received_bytes / size`）
- **必须有取消按钮** —— 手机上下载一个 50 MB 的文件发现选错了，只能干等是很糟的体验
- 取消 → 发 `file.cancel` → Agent 关文件句柄 / 删临时文件

---

## 9. 里程碑（插入为新 Phase 8）

| 子阶段 | 内容 | 验收 |
|---|---|---|
| 8a | `agent/workspace.go` 的 `ResolveInRoot` + 全部路径穿越测试 | FS1–FS3 全绿 |
| 8b | Agent 侧 `file.list` / `file.stat` / `file.read` | 用 WebSocket 客户端能列出目录、下载文件 |
| 8c | Server 侧 Relay 透传 + transfer 注册表 + 方向锁 | FS5 通过；Server 内存不随文件大小增长 |
| 8d | `file.write` 上传 + 临时文件 + 原子改名 | FS6–FS9 全绿 |
| 8e | 前端 Files 页（桌面） | 能浏览、预览、下载、上传 |
| 8f | 移动端适配 + 真机验证 | iOS/Android 各测一遍下载与上传 |
| 8g | 流控与背压压测 | 下载 100 MB 文件的同时终端交互延迟 < 200ms |

**原 P8（安全加固）顺延为 P9，原 P9（Docker）→ P10，原 P10（文档 Release）→ P11。**

---

## 10. 待确认

| # | 问题 | 我的建议 |
|---|---|---|
| 1 | 允许浏览的范围是 `allowed_workspaces` 还是整个盘？ | **只允许 allowed_workspaces**。要看别的目录，用户改本机配置 —— 这和 Q2 的命令白名单同一个哲学 |
| 2 | 要不要支持删除/重命名？ | **不做**。见 §1 的边界声明 |
| 3 | 单文件上限 100 MB 够吗？ | 够。要传大文件应该用别的手段（`scp` / 网盘），不该让 CodeGate 变成传输工具 |
| 4 | 上传要不要显示在会话里（比如往 PTY 里提示）？ | **不要**。两条通路保持独立，耦合会带来一堆边界情况 |
| 5 | 下载能不能直接落进 PTY 的 cwd？ | 不能。文件 API 的路径是显式的，不做隐式推断 |
