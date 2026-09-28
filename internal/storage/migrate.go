package storage

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// migrations 目录随二进制内嵌。
//
// 用 embed 而不是读磁盘：部署时只有一个可执行文件，不会出现
// 「忘了拷 migrations 目录 → 服务起不来」这类事故。
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration 是一个已解析的迁移文件。
type migration struct {
	version int
	name    string
	sql     string
}

// Migrate 把数据库升级到最新版本。
//
// 自写 migrator（§12.2）而不是引框架：需求只有两条 —— 顺序执行、
// 幂等记录。为此拉一个依赖不划算，而且框架的抽象会掩盖 SQL 本身。
//
// 每个迁移在**独立事务**里执行：失败时已成功的迁移保持生效，
// 下次启动从断点继续，不会把整个 schema 回滚掉。
func (s *SQLite) Migrate(ctx context.Context) error {
	if err := s.ensureMigrationsTable(ctx); err != nil {
		return err
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	all, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range all {
		if applied[m.version] {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
		s.log.Info("已应用数据库迁移", "version", m.version, "name", m.name)
	}
	return nil
}

func (s *SQLite) ensureMigrationsTable(ctx context.Context) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("storage: 创建 schema_migrations 失败: %w", err)
	}
	return nil
}

func (s *SQLite) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("storage: 读取迁移记录失败: %w", err)
	}
	defer rows.Close()

	out := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("storage: 扫描迁移记录失败: %w", err)
		}
		out[v] = true
	}
	return out, rows.Err()
}

func (s *SQLite) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: 迁移 %d 开启事务失败: %w", m.version, err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后 Rollback 是 no-op

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("storage: 迁移 %d (%s) 执行失败: %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, nowMs()); err != nil {
		return fmt.Errorf("storage: 记录迁移 %d 失败: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: 迁移 %d 提交失败: %w", m.version, err)
	}
	return nil
}

// loadMigrations 读取并解析内嵌的迁移文件，按版本号升序返回。
//
// 文件命名约定：`<4 位版本>_<描述>.sql`，例如 `0001_init.sql`。
// 版本号必须唯一且递增 —— 重复时直接报错而不是静默取其一。
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("storage: 读取内嵌迁移目录失败: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("storage: 迁移版本 %d 重复（%s 与 %s）", version, prev, e.Name())
		}
		seen[version] = e.Name()

		raw, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("storage: 读取迁移 %s 失败: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: name, sql: string(raw)})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("storage: 没有找到任何迁移文件（内嵌资源可能损坏）")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	idx := strings.IndexByte(base, '_')
	if idx <= 0 {
		return 0, "", fmt.Errorf("storage: 迁移文件名 %q 不符合 `<版本>_<描述>.sql` 约定", filename)
	}
	v, err := strconv.Atoi(base[:idx])
	if err != nil || v <= 0 {
		return 0, "", fmt.Errorf("storage: 迁移文件名 %q 的版本号非法", filename)
	}
	return v, base[idx+1:], nil
}
