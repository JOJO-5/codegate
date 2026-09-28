// Package storage 是 Server 的持久层。
//
// 设计要点（§12.2）：
//   - **Store 接口是唯一的抽象点**。上层（server/auth/device）只依赖接口，
//     不出现任何 SQL。这样换 PG 时改动收敛在一个包内。
//   - 模型层用 time.Time，DB 层存 Unix 毫秒整数（§13.2 第 2 条）。
//     转换只在 sqlite.go 里发生，上层看不到整数时间戳。
//   - 时间戳一律由**调用方传入**（`now` 参数），不在 SQL 里用 CURRENT_TIMESTAMP。
//     理由：测试需要可控时间；而且 Server 与 Agent 的时间基准要对齐。
package storage

import (
	"context"
	"errors"
	"time"
)

// 哨兵错误。上层用 errors.Is 判断，不要比较字符串。
var (
	// ErrNotFound 表示记录不存在。
	ErrNotFound = errors.New("storage: 记录不存在")
	// ErrConflict 表示违反唯一约束（邮箱重复、code 撞车等）。
	ErrConflict = errors.New("storage: 唯一约束冲突")
)

// ---- 模型 ----

// User 是账号。
type User struct {
	ID           string
	Email        string
	PasswordHash string // argon2id 编码串
	Role         string // user | admin
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RefreshToken 是不透明刷新令牌。
//
// ★ 存的是 sha256(token) 而不是 token 本身。DB 泄露时攻击者拿到哈希也换不到
// access token。FamilyID 用于「检测到重用 → 整族吊销」（§10.1）。
type RefreshToken struct {
	ID        string
	UserID    string
	TokenHash []byte
	FamilyID  string
	ExpiresAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
	UserAgent string
	IP        string
	CreatedAt time.Time
}

// Valid 判断令牌当前是否可用。三个条件缺一不可。
func (t *RefreshToken) Valid(now time.Time) bool {
	return t.RevokedAt == nil && t.UsedAt == nil && now.Before(t.ExpiresAt)
}

// Device 是一台已注册的 Agent 机器。
//
// UserID 为空串表示「尚未绑定到任何账号」—— DB 里存 NULL。
type Device struct {
	ID           string
	UserID       string
	Name         string
	Platform     string // windows | linux | darwin
	Arch         string // amd64 | arm64
	AgentVersion string
	PublicKey    []byte // Ed25519 公钥，32 字节
	LastSeenAt   *time.Time
	PairedAt     *time.Time
	RevokedAt    *time.Time
	CreatedAt    time.Time
}

// Paired 判断设备是否已绑定到某个用户。
func (d *Device) Paired() bool { return d.UserID != "" }

// PairingCode 是一次性的设备配对凭据。
//
// ★ 同时携带 device_id 与 public_key，因为 Agent 在 `agent.pair.begin` 时
// 还没被认证 —— 这些字段是它自称的，真正生效要等用户在浏览器上二次确认。
type PairingCode struct {
	ID           string
	CodeHash     []byte
	DeviceID     string
	PublicKey    []byte
	Name         string
	Platform     string
	Arch         string
	AgentVersion string
	AgentIP      string
	ExpiresAt    time.Time
	UsedAt       *time.Time
	UsedBy       string
	CreatedAt    time.Time
}

// Usable 判断配对码是否还能用。
func (p *PairingCode) Usable(now time.Time) bool {
	return p.UsedAt == nil && now.Before(p.ExpiresAt)
}

// SessionMeta 是 Server 侧的会话元数据视图。
//
// ★ **不是真相源**。真相源永远是 Agent 内存（§13.2 第 1 条）。
// 这张表存在的意义是：给设备页一个不用等 Agent 的快速首屏 + 留一份历史。
// 所以字段允许短暂过期，Agent 重连时会用 session.sync 覆盖。
type SessionMeta struct {
	ID             string
	DeviceID       string
	UserID         string
	Name           string
	Command        string
	Args           []string
	Cwd            string
	Status         string // starting|running|detached|exited|failed|terminated
	PID            int
	ExitCode       *int
	Cols           uint16
	Rows           uint16
	CreatedAt      time.Time
	StartedAt      *time.Time
	EndedAt        *time.Time
	LastAttachedAt *time.Time
	CreatedBy      string
}

// SessionRuntime 是会话的**运行态**字段集合。
//
// ★ 为什么需要单独一个类型：Agent 的心跳（`agent.heartbeat`）只上报
// status/cols/rows/pid —— 它的 payload 是 HeartbeatSession，**没有** name /
// command / args / cwd。如果拿心跳去走 UpsertSession，这些元数据会被空值覆盖。
//
// 这个坑在写测试时才暴露出来（TestSessionUpsertAndPrune 原本把 Args 弄丢了），
// 说明「按语义把写路径拆开」比「一个 Upsert 走天下」更可靠。
type SessionRuntime struct {
	Status   string
	Cols     uint16
	Rows     uint16
	PID      int
	ExitCode *int
	EndedAt  *time.Time
}

// AuditLog 是一条审计记录。
//
// ★ MetaJSON 里**绝不允许**出现 token / 密码 / 终端内容（§13.1 注释）。
type AuditLog struct {
	ID        string
	UserID    string
	DeviceID  string
	SessionID string
	Action    string // login.ok | login.fail | device.pair | session.create | ...
	Result    string // ok | denied | error
	IP        string
	UserAgent string
	MetaJSON  string
	CreatedAt time.Time
}

// ---- Store 接口 ----

// Store 是持久层的全部能力。
//
// 方法按域分组，顺序与 migrations/0001_init.sql 一致，便于对照。
type Store interface {
	// 生命周期
	Close() error
	Ping(ctx context.Context) error
	Migrate(ctx context.Context) error

	// ---- users ----
	CreateUser(ctx context.Context, u *User) error
	UserByEmail(ctx context.Context, email string) (*User, error)
	UserByID(ctx context.Context, id string) (*User, error)
	CountUsers(ctx context.Context) (int, error)
	// ListUsers 列出全部账号，按创建时间升序。
	//
	// 只有管理命令（codegate-server user list）用得到它 ——
	// 服务端运行期不需要「枚举所有账号」这个能力，
	// 而多一个能枚举账号的方法就多一个可能被误用的面。
	// 放在接口里是因为管理命令和 Server 共用同一个 Store 实现。
	ListUsers(ctx context.Context) ([]*User, error)
	UpdateUserPassword(ctx context.Context, id, hash string, now time.Time) error
	// SetUserDisabled 停用/启用一个账号。
	//
	// 停用是**立刻生效**的：requireAuth 每次请求都查库确认用户仍然有效
	// （§10.1 的代价与收益），所以不需要等 access token 过期。
	// 停用不删数据 —— 删用户会级联带走设备与审计记录，
	// 而审计记录恰恰是调查「这个账号做过什么」时唯一能看的线索。
	SetUserDisabled(ctx context.Context, id string, disabled bool, now time.Time) error

	// ---- refresh_tokens ----
	CreateRefreshToken(ctx context.Context, t *RefreshToken) error
	RefreshTokenByHash(ctx context.Context, hash []byte) (*RefreshToken, error)
	MarkRefreshTokenUsed(ctx context.Context, id string, at time.Time) error
	// RevokeRefreshTokenFamily 吊销整族令牌，返回受影响行数。
	// 检测到「已用过的令牌被重放」时调用 —— 这是唯一正确的响应（§10.1）。
	RevokeRefreshTokenFamily(ctx context.Context, familyID string, at time.Time) (int64, error)
	// RevokeUserRefreshTokens 吊销某用户全部刷新令牌（改密码 / 登出所有设备）。
	RevokeUserRefreshTokens(ctx context.Context, userID string, at time.Time) (int64, error)
	DeleteExpiredRefreshTokens(ctx context.Context, before time.Time) (int64, error)

	// ---- devices ----
	CreateDevice(ctx context.Context, d *Device) error
	DeviceByID(ctx context.Context, id string) (*Device, error)
	DevicesByUser(ctx context.Context, userID string) ([]*Device, error)
	RenameDevice(ctx context.Context, id, name string) error
	// BindDevice 把设备绑定到用户。已绑定时返回 ErrConflict（防止设备被抢绑）。
	BindDevice(ctx context.Context, id, userID string, at time.Time) error
	// DeleteDevice 解绑并删除设备记录（级联删除其 sessions）。
	DeleteDevice(ctx context.Context, id string) error
	TouchDeviceLastSeen(ctx context.Context, id string, at time.Time) error

	// ---- pairing_codes ----
	CreatePairingCode(ctx context.Context, p *PairingCode) error
	PairingCodeByHash(ctx context.Context, hash []byte) (*PairingCode, error)
	// MarkPairingCodeUsed 原子地标记配对码已用。已用时返回 ErrConflict ——
	// 这是防重放的关键：两个请求同时 confirm 时只有一个能成功。
	MarkPairingCodeUsed(ctx context.Context, id, userID string, at time.Time) error
	DeleteExpiredPairingCodes(ctx context.Context, before time.Time) (int64, error)

	// ---- sessions ----
	// UpsertSession 写入完整元数据。用于 session.sync / session.created
	// 这类携带完整 SessionSummary 的路径。
	UpsertSession(ctx context.Context, s *SessionMeta) error
	// UpdateSessionRuntime 只更新运行态字段，不碰 name/command/args/cwd。
	// 用于心跳对账。会话不存在时静默返回 nil（可能刚被 Prune 掉）。
	UpdateSessionRuntime(ctx context.Context, id string, rt SessionRuntime) error
	SessionByID(ctx context.Context, id string) (*SessionMeta, error)
	SessionsByDevice(ctx context.Context, deviceID string) ([]*SessionMeta, error)
	SessionsByUser(ctx context.Context, userID string, limit int) ([]*SessionMeta, error)
	// PruneSessions 删除该设备下不在 keep 列表里的会话，返回删除行数。
	// 用于 Agent 重连后的对账：Agent 说「我只有这些」，其余都是历史残留。
	PruneSessions(ctx context.Context, deviceID string, keep []string) (int64, error)

	// ---- audit_logs ----
	InsertAudit(ctx context.Context, a *AuditLog) error
	AuditLogsByUser(ctx context.Context, userID string, limit int) ([]*AuditLog, error)
	// AuditLogsByUserBefore 是 AuditLogsByUser 的游标版本：
	// 只返回 id 严格小于 beforeID 的记录（即「更早的」）。beforeID 为空表示从最新开始。
	//
	// ★ 游标用 id 而不是 created_at：created_at 会重复（同一毫秒内可能写入
	// 多条审计），按它翻页必然漏记录或重复记录。ID 是 UUIDv7（时间有序），
	// 既唯一又能表达「更早」。
	AuditLogsByUserBefore(ctx context.Context, userID, beforeID string, limit int) ([]*AuditLog, error)
}
