// Package store 是 PostgreSQL 存储层：连接池、内嵌迁移、各领域的数据访问。
//
// 约定：
//   - 未找到返回 ErrNotFound；唯一冲突返回 ErrConflict；调用方据此翻译成用户提示。
//   - 所有方法接收 ctx；不持有业务逻辑（权限判定在 perm 包，编排在 tools/chat）。
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// 哨兵错误。
var (
	ErrNotFound = errors.New("记录不存在")
	ErrConflict = errors.New("记录已存在")
)

// Store 持有连接池。
type Store struct {
	pool *pgxpool.Pool
}

// Pool 暴露底层连接池，供跨包的健康检查、高级查询与集成测试使用。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Open 连接数据库并执行迁移。
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("数据库不可达: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("迁移: %w", err)
	}
	return s, nil
}

// Close 释放连接池。
func (s *Store) Close() { s.pool.Close() }

// Ping 探活数据库连接（SELECT 1）。/healthz 用它区分存活/不存活，让负载均衡器/部署
// 能据实判断——死 200 会让流量继续打到一个 DB 已断的实例。
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `SELECT 1`)
	return err
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		// 单迁移单事务：要么全部生效要么全部回滚。
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("执行 %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AppliedMigrations(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// wrapErr 把驱动层错误翻译成哨兵错误。
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrConflict
	}
	return err
}
