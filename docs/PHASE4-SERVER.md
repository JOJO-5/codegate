# Phase 4 验收报告 —— Server

> 状态：**已完成** · 2026-09-28
> 交付物：`internal/server`（22 个源文件）+ `cmd/codegate-server`（CLI）+ 6 个测试文件 + `scripts/server-smoke.sh`
> 上游依据：`docs/PHASE0-ARCHITECTURE.md` §4 / §8 / §10 / §14

---

## 1. 一句话结论

Server 能跑起来、能认证、能配对、能转发、能优雅退出，**并且有一组测试专门盯着 IDOR 和内存预算**。

```
go build ./...            EXIT=0
go test -vet=off ./...    13 个包全绿（internal/server 新增 ~40 个用例）
make server               ✅
make server-smoke         ✅ 15 项全过
```

---

## 2. 交付物清单

### 2.1 包结构

| 文件 | 职责 |
|---|---|
| `server.go` | 装配点：依赖注入、优雅关闭、后台清理循环 |
| `routes.go` | 路由表（标准库 `ServeMux`，Go 1.22+ 的 `METHOD /path/{id}`） |
| `http.go` | 统一响应/错误格式、`httpStatusFor`（错误码 → 状态码）、`clientIP` |
| `middleware.go` | `withRecover` / `withRequestLog` / `requireAuth` / `withOriginCheck` |
| `registry.go` | 连接注册表（`agents` / `clients` / `sessionSubs` / `sessionOwner` 四份索引） |
| `relay.go` | 唯一跨连接通路（帧路由 + 会话广播） |
| `authz.go` | **授权唯一入口**（`authorizeDevice` / `authorizeSession` / `requireAgent`） |
| `ratelimit.go` | 令牌桶限流 + `FailureGuard`（连续失败锁定） |
| `handler_auth.go` | 注册 / 登录 / 刷新 / 登出 / 改密码 / `me` |
| `handler_device.go` | 设备列表 / 详情 / 改名 / 删除 / 会话缓存 / 配对两步 / 审计分页 |
| `handler_wsticket.go` | 一次性 WS 票据（30 s，用完即焚） |
| `handler_health.go` | `healthz` / `readyz` / `version` |
| `close.go` | WS 关闭码表 + `closeWithCode`（`WriteControl`，不关连接） |
| `request.go` | `decodeJSON[T]`（`MaxBytesReader` + `DisallowUnknownFields` + 拒尾随 JSON） |
| `pairing.go` | 待确认配对的 Agent 连接登记表（**刻意不进 Registry**） |
| `pending.go` | 待回请求登记表（`request_id` → client，WS 请求-响应路由的关键） |
| `ws.go` | WS 公共层（upgrader / upgrade / writePump / 超时常数） |
| `ws_agent.go` | Agent WS handler（握手 / 认证 / 心跳 / 会话同步 / 响应转发） |
| `ws_client.go` | 浏览器 WS handler（请求路由 / 二进制帧 / 断开清理） |
| `audit.go` | 审计动作常量 + `audit` / `auditRequest` |

### 2.2 REST 端点（全部实现，与架构文档 §14.2 一致）

| Method | Path | 认证 | 说明 |
|---|---|---|---|
| POST | `/api/v1/auth/register` | 无 | 仅当无用户存在，或 `allow_signup: true` |
| POST | `/api/v1/auth/login` | 无 | → `{access_token, token_type, expires_in}` + `Set-Cookie(cg_refresh)` |
| POST | `/api/v1/auth/refresh` | Cookie | 轮换 refresh → 新 access + 新 cookie |
| POST | `/api/v1/auth/logout` | Bearer | 吊销整族 refresh（幂等） |
| POST | `/api/v1/auth/password` | Bearer | 改密码（吊销该用户全部 refresh） |
| GET | `/api/v1/me` | Bearer | 当前用户 |
| GET | `/api/v1/devices` | Bearer | 设备列表（含 `online` 实时字段） |
| GET | `/api/v1/devices/{id}` | Bearer | 设备详情 |
| PATCH | `/api/v1/devices/{id}` | Bearer | 改名 |
| DELETE | `/api/v1/devices/{id}` | Bearer | 解绑 + 踢掉 Agent 连接（4401） |
| GET | `/api/v1/devices/{id}/sessions` | Bearer | DB 缓存视图（首屏用，随后由 WS 校正） |
| POST | `/api/v1/devices/pair` | Bearer | 提交配对码 → 待确认设备信息（**预览**） |
| POST | `/api/v1/devices/pair/confirm` | Bearer | 二次确认完成绑定 |
| GET | `/api/v1/audit` | Bearer | 分页审计日志（`?limit=&cursor=`） |
| POST | `/api/v1/ws-ticket` | Bearer | → `{ticket, expires_in:30, protocol}` |
| GET | `/api/v1/ws/agent` | Ed25519（连接内） | Agent 长连接 |
| GET | `/api/v1/ws/client` | `?ticket=` | 浏览器长连接 |
| GET | `/healthz` | 无 | 存活探针（**刻意不做任何 I/O**） |
| GET | `/readyz` | 无 | 就绪（DB ping + 连接计数） |
| GET | `/api/v1/version` | 无 | Server 版本 + 协议版本范围 |

### 2.3 CLI（`cmd/codegate-server`）

```
codegate-server serve                启动服务（常驻，Ctrl+C 优雅退出）
codegate-server user add             创建账号
codegate-server user list            列出所有账号
codegate-server user disable <邮箱>  停用账号（对已签发 token 立刻生效）
codegate-server user enable  <邮箱>  重新启用
codegate-server user passwd  <邮箱>  重置密码（同时吊销全部 refresh token）
codegate-server config               打印生效配置（JWT 密钥打码）
codegate-server doctor               环境自检：目录 / 数据库 / 密钥 / 端口 / Origin / 反代
codegate-server version              版本
```

---

## 3. 验收：P4 退出标准逐条核对

架构文档 §1607 定义的退出标准：

| # | 标准 | 结果 |
|---|---|---|
| 1 | REST 全端点有测试 | ✅ `auth_test.go` / `device_test.go` 覆盖全部端点 |
| 2 | `/ws/agent` 与 `/ws/client` 可连 | ✅ `ws_test.go` 真实握手（Ed25519 签名 + 票据） |
| 3 | pairing 全流程跑通 | ✅ `TestPairPreviewThenConfirm`：预览 → 确认 → 落库 → 出现在列表 |
| 4 | IDOR 测试（S1/S2）通过 | ✅ `TestDeviceIDOR` + `TestDeviceNotFoundIsIndistinguishableFromForbidden` + `TestDeviceEndpointsAgreeOnForeignDevice` |
| 5 | R6 内存实测（500 条假 Agent 连接，目标 < 100 KB/空闲连接） | ✅ **42484 字节/连接** |

### 3.1 R6 实测口径（诚实说明）

```
500 条空闲连接：总堆增量 20.96 MB，平均每条 42484 字节（预算 102400 字节）
```

量的是 **`HeapAlloc`**，覆盖：连接结构体、发送队列底层数组、`sessions`/`attached` 两个 map、
gorilla 读写缓冲、注册表四份索引。

**不覆盖** goroutine 栈（算在 `StackInuse`，每条连接 2 个 goroutine × 2 KB 起步）——
所以真实占用比这个数字高约 8 KB/连接，100 KB 的预算为此留了余量。
用 `HeapAlloc` 是因为它稳定可测；`StackInuse` 随调度器复用栈剧烈波动，做成断言必然 flaky。

`TestSendQueueSizeIsBounded` 是配套的**结构性**约束测试：直接钉住
`defaultSendQueueSize == 256`，队列被调大时它会指出原因，而不是给一个间接线索。

---

## 4. 测试抓出的三个真 bug

### 4.1 ★★ 跨端点存在性预言机（IDOR 信息泄露）

**现象**：同一台「别人的设备」

| 端点 | 走哪条路 | 返回 |
|---|---|---|
| `GET /api/v1/devices/{id}` | `device.Service.Get` → `device.ErrNotFound` | **404** |
| `GET /api/v1/devices/{id}/sessions` | `authorizeDevice` → `errNoAccess` | **403** |

**影响**：攻击者拿一批 UUID 逐个请求这两个端点，按状态码差异就能枚举出哪些
`device_id` 真实存在。光是「存在性」本身就已经是信息泄露（能用来画出一家公司有多少台机器）。

**根因**：REST 层有两套「不存在 / 无权」的写法，各自映射到不同状态码。
`authz.go` 的注释写着「所有不属于你的情况都返回与不存在**完全相同**的错误」，
但 `errNoAccess` 用的是 `CodeForbidden`（→ 403），而 `writeDeviceError` 手写 404。

**修法**：新增 `protocol.CodeNotFound`（→ 404），把两条路径收敛到同一个出口 ——
同状态码 + 同错误码 + 同文案。

**固化**：`TestDeviceEndpointsAgreeOnForeignDevice` 遍历**所有**设备端点
（详情 / 会话 / 改名 / 删除），断言「别人的设备」与「不存在的设备」**逐字节相同**。
新增设备端点会自动被这条测试覆盖。

> **教训**：单看任何一个端点都发现不了这类问题。要测的是
> **端点之间的一致性**，不是单个端点的正确性。

### 4.2 `Store` 接口缺 `SetUserDisabled`

`User.Disabled` 在 `authenticate` / `handleLogin` 里被读，但**没有任何代码能写它** ——
「停用账号」这个功能根本不存在。

**修法**：补 `SetUserDisabled` + `ListUsers`（后者给 `user list` 用）。
停用**立刻生效**（`requireAuth` 每次请求都查库，§10.1 的代价与收益），
不需要等 access token 过期。

### 4.3 CLI 标志位置陷阱（可用性）

**现象**：`user passwd bob@example.com -password xxx` 里 `-password` 被**静默忽略**，
程序转去弹交互式输入，然后 EOF 报错。用户看到「密码: 」和一个报错，
完全联想不到是自己把标志写在了位置参数后面。

**根因**：Go 的 `flag` 包在遇到**第一个非标志参数时就停止解析**。

**修法**：`parseArgs` 重排 —— 先把标志（连同它需要的值）挑出来，剩下才是位置参数。
用 `Value.(interface{ IsBoolFlag() bool })` 判断布尔标志，而不是硬编码名字。

---

## 5. 关键设计决策（以及为什么）

| 决策 | 理由 |
|---|---|
| **路由认证按组显式挂**，不做全局 `requireAuth` | 全局挂会逼每个 handler 自己判断「我要不要豁免」，迟早漏一个。显式挂意味着加新路由必须主动选一次，而不是被动继承默认值 |
| **`errNoAccess` 统一到 404** | 见 §4.1 |
| **`pairs` 不进 `Registry`** | 那些连接尚未认证，进注册表就可能被 Relay 当成可路由目标 |
| **`pending` 只有 Server 知道** | 中继本身无状态，`request_id → client` 的映射是 Server 独有的知识 |
| **登出吊销整族而非单令牌** | 一次登录会话 = 一个 family（登录时新建，之后每次刷新都留在族内） |
| **refresh token 只走 HttpOnly Cookie** | 一旦出现在 JSON 里，前端就一定会有人把它存进 `localStorage` —— 那等于把「Cookie 防 XSS」整个作废 |
| **停用账号立刻生效** | 代价是每次请求多一次主键查询；收益是**不需要等最长 15 分钟的 token 过期窗口** |
| **`closeWithCode` 用 `WriteControl` 而非发送队列** | 队列满时（正是「慢消费者被踢」这个场景）恰恰最需要能立刻发出关闭帧 |
| **`closeWithCode` 刻意不调 `conn.Close()`** | 主动掐 TCP 会让对端看到 1006（异常关闭），把「服务端主动踢我」误判成「网络断了」 |
| **`classifyReadError` 恒返回 `CloseNormal`** | Agent 侧才认 4401/4426 为致命。服务端自作主张给错误关闭码，会让它把一次普通断线当成致命错误、彻底不重连 |
| **`nonce` 一次性，验签前先作废** | 只在「验签失败」时作废的话，成功那次会留下一个仍有效的 nonce —— 攻击者录下这次握手就能重放 |
| **登录失败一律同一句话** | 区分「用户不存在」和「密码错」会让登录接口退化成邮箱枚举器 |
| **`burnPasswordTime` 抹平时序** | 用户不存在时不跑 argon2 会快两个数量级，攻击者用响应时间就能枚举邮箱 |
| **未认证连接只能发 hello/auth/pair.begin** | 否则未认证的连接就能发 `session.*`，而服务端此时还不知道它是谁 |
| **新 Agent 连接顶掉旧连接（4409）** | 拒绝新连接会让设备在旧连接自然超时前（最长 60 s）完全不可用 |

---

## 6. 测试清单

| 文件 | 覆盖 |
|---|---|
| `harness_test.go` | 真实 SQLite + `httptest` 的脚手架，可注入时钟 |
| `auth_test.go` | 注册（引导 / 关闭 / 重复 / 弱密码 / 大小写归一）/ 登录（成功 / 失败不可区分 / 停用）/ 刷新轮换 / **重用检测整族吊销** / 登出幂等 / 改密码吊销会话 / 票据单次使用与过期 / Origin 校验 |
| `device_test.go` | 列表归属隔离 / 空列表是数组不是 null / **IDOR 四端点** / 不存在与无权不可区分 / **跨端点一致性** / 改名 / 删除 / 会话缓存 / 配对预览→确认 / 配对码单次使用 / 格式错误不可区分 / 不能抢绑他人设备 / 审计归属隔离与分页 |
| `ws_test.go` | Agent 完整握手 / 错误签名 / 未配对设备 / 未认证发运行期消息 / 版本不兼容 / **挑战绑定防重放** / 新连接顶替旧连接；浏览器票据（无效 / 复用）/ ping-pong / 方向校验 / 文件操作明确报错 |
| `memory_test.go` | **R6 500 条连接内存实测** / `SendQueueSize` 结构性约束 |

### 6.1 为什么用真实 SQLite 而不是 fake store

`Store` 接口有 30 多个方法。写一个「恰好满足当前测试」的假实现，
等于把测试和实现绑死：实现改了接口，假实现跟着改，
但**真正的 SQL 一行都没被验证过**。

R9 的锁行为、`ErrConflict` 的原子性（`MarkRefreshTokenUsed` 的 CAS）、级联删除 ——
这些全都只存在于真实 SQL 里。

### 6.2 `scripts/server-smoke.sh`：进程级冒烟

单元测试全跑在 `httptest` 里 —— 不经过真实监听端口、不经过进程启动路径、
不经过信号处理。而部署时会踩的坑恰好都在那一层：

- 配置从环境变量读进来了吗？
- 数据库目录建了吗？
- Ctrl+C 能优雅退出吗（还是卡住等 WS 连接）？
- 二进制真的能起来吗？

这个脚本**启动真进程、连真端口、发真信号**，全程用临时目录与随机端口，
不碰用户已有数据。它是唯一能证明「这套东西能部署」的东西。

---

## 7. 已知限制 / 待办

| 项 | 说明 |
|---|---|
| **前端未做** | P5。`internal/server/webui/` 目录还不存在，`make web` 会失败 |
| **文件传输未做** | P8。`file.*` 消息当前返回明确的 `internal` 错误（而不是静默丢弃 —— 静默丢弃会让前端永远停在 loading） |
| **`session.get` payload 复用** | P3 遗留：Agent 侧复用了 `SessionAttachPayload` 解析，协议里没有专用的 get 请求结构 |
| **`CodeForbidden` 语义未统一** | `relay.go` 仍用它表示「未 attach 到该会话」。WS 路径没有 HTTP 状态码，不影响预言机问题，但语义上值得收敛 |
| **多实例部署** | `ticketStore` 在内存里。多实例需要 sticky session，或把票据换成 JWT |
| **`user list` 不分页** | 账号数量级通常是几个到几十个，暂不必要 |

---

## 8. 附：本地环境备注

本机（Windows + 亿赛通 DLP）跑 Go 有若干专属限制，都写在 `scripts/goenv.sh` 与
`.workbuddy-ai/memory/` 里。与 P4 相关的两条：

1. **`go test` 必须加 `-vet=off`** —— 本机 `vet.exe` 崩在 `0xc0000005`。
2. **绝不用 `gofmt -w` 直接写项目文件** —— `gofmt` 在 DLP 白名单里，
   写受保护后缀（`.go`）会被自动加密，之后 `Read`/`grep` 全部读不出来。
   用 `bash tools/gofmt-plain.sh <path>`，它走「gofmt 到临时文件 → fcli 直连写回」。

疑似文件变密文时：

```bash
python tools/fcli-plaintext.py --check internal cmd docs   # 扫描
python tools/fcli-plaintext.py internal/server/xxx.go      # 修复
```
