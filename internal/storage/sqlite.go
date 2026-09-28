package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	// 纯 Go 的 SQLite 驱动，注册为 "sqlite"。
	// 选它而不是 mattn/go-sqlite3 的理由：**无 cgo**，交叉编译友好（§12.3）。
	_ "modernc.org/sqlite"
)

// driverName 是驱动注册名。集中在这里，避免各处拼写不一致。
const driverName = "sqlite"

// SQLite 是 Store 的 SQLite 实现。
//
// 未导出字段 + 构造函数：调用方只能通过 OpenSQLite 拿到实例，
// 无法构造出一个 db 为 nil 的残废对象。
type SQLite struct {
	db   *sql.DB
	log  *slog.Logger
	path string
}

// SQLiteOptions 是打开数据库的参数。
type SQLiteOptions struct {
	// Path 是数据库文件路径。
	Path string
	// Logger 为空则用 slog.Default()。
	Logger *slog.Logger
	// MaxOpenConns 是连接池上限。
	//
	// ★ 默认 1，这是刻意的（R9）：SQLite 只允许一个写者，而 Go 的
	// database/sql 池 + SQLite 的经典故障就是并发写触发 `database is locked`。
	// 把池限制为 1 就从根上消除了这个问题。代价是查询串行化 ——
	// MVP 的写频率极低（登录、pair、会话元数据），这个代价可以忽略。
	MaxOpenConns int
}

// OpenSQLite 打开（必要时创建）数据库并完成 PRAGMA 配置。
//
// 注意：**不自动执行迁移**。迁移由调用方显式调 Migrate ——
// 让「打开数据库」和「改数据库结构」是两件分开的事，
// 否则一个只想读数据的命令（比如 status）也会去改 schema。
func OpenSQLite(ctx context.Context, o SQLiteOptions) (*SQLite, error) {
	if strings.TrimSpace(o.Path) == "" {
		return nil, errors.New("storage: 数据库路径为空")
	}
	// DSN 里 `?` 之后是参数区，路径里出现它会把文件名截断。
	// 静默接受这种路径会导致「数据写到了别的文件」这种极难排查的问题。
	if strings.ContainsAny(o.Path, "?#") {
		return nil, fmt.Errorf("storage: 数据库路径不能包含 ? 或 #: %q", o.Path)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxOpenConns <= 0 {
		o.MaxOpenConns = 1
	}

	db, err := sql.Open(driverName, buildDSN(o.Path))
	if err != nil {
		return nil, fmt.Errorf("storage: 打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(o.MaxOpenConns)
	db.SetMaxIdleConns(o.MaxOpenConns)
	// 0 = 不限制连接寿命。SQLite 是本地文件，没有「服务端主动断连」这回事，
	// 反复重建连接只会白白重跑 PRAGMA。
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: 连接数据库失败: %w", err)
	}

	return &SQLite{db: db, log: o.Logger, path: o.Path}, nil
}

// buildDSN 拼出带 PRAGMA 的 DSN。
//
// PRAGMA 放在 DSN 里而不是开库后逐条执行：连接池可能建立多条物理连接，
// 逐条执行只能保证「执行时那一条」被配置好。DSN 里的 _pragma 由驱动在
// **每条新连接**建立时应用，这才是正确的做法。
func buildDSN(path string) string {
	params := []string{
		"_pragma=journal_mode(WAL)",   // 读写并发，写不阻塞读（§13.3）
		"_pragma=busy_timeout(5000)",  // 遇到锁时等待而不是立刻报错
		"_pragma=foreign_keys(1)",     // SQLite 默认【关闭】外键，必须显式打开
		"_pragma=synchronous(NORMAL)", // WAL 下 NORMAL 已足够安全，比 FULL 快很多
	}
	return "file:" + filepath.ToSlash(path) + "?" + strings.Join(params, "&")
}

// Close 关闭数据库。
func (s *SQLite) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping 检查数据库可用性，供 /readyz 使用。
func (s *SQLite) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Path 返回数据库文件路径（doctor / 日志用）。
func (s *SQLite) Path() string { return s.path }

// ---------------------------------------------------------------------------
// 错误转换
// ---------------------------------------------------------------------------

// wrapErr 把驱动错误翻译成 storage 包的哨兵错误。
//
// ★ 这是「上层不出现 SQL」这条纪律的一部分：上层只需要
// `errors.Is(err, storage.ErrNotFound)`，不需要知道 SQLite 的错误码。
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	// SQLite 的约束错误信息形如：
	//   UNIQUE constraint failed: users.email
	//   FOREIGN KEY constraint failed
	// 匹配 "constraint failed" 能覆盖 UNIQUE / PRIMARY KEY / FOREIGN KEY / CHECK。
	// 用字符串匹配而不是错误码，是为了不把 modernc 的内部常量包
	// （modernc.org/sqlite/lib）引入本包 —— 那个包的 API 稳定性没有承诺。
	if strings.Contains(err.Error(), "constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// ---------------------------------------------------------------------------
// 时间转换（§13.2：DB 存 Unix 毫秒整数）
// ---------------------------------------------------------------------------

// nowMs 返回当前 Unix 毫秒。
func nowMs() int64 { return time.Now().UnixMilli() }

// toMs 把 time.Time 转成 Unix 毫秒。
func toMs(t time.Time) int64 { return t.UnixMilli() }

// fromMs 把 Unix 毫秒转成 time.Time（UTC）。
func fromMs(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// msPtr 把可空毫秒列转成 *time.Time。NULL → nil。
func msPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMs(v.Int64)
	return &t
}

// toMsArg 把 *time.Time 转成可直接绑定的参数。nil → NULL。
func toMsArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// timeArg 把 time.Time 转成绑定参数。
func timeArg(t time.Time) any { return t.UnixMilli() }
