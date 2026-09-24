// Package pg 是唯一持久层：账号 / 配置 / Key / 分组 / 追踪 / Batches 都在 PostgreSQL。
package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultRetention 请求日志默认保留天数。
const DefaultRetention = 7

// DB 是连接池 + 迁移后的仓库。
type DB struct {
	pool *pgxpool.Pool
}

// Open 连接 DATABASE_URL 并跑迁移。url 空则返回 (nil, nil)。
func Open(ctx context.Context, url string) (*DB, error) {
	if url == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.MaxConns < 4 {
		cfg.MaxConns = 8
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	db := &DB{pool: pool}
	if err := db.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return db, nil
}

// Close 关闭连接池。
func (d *DB) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

// Ping 探活。
func (d *DB) Ping(ctx context.Context) error {
	if d == nil || d.pool == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return d.pool.Ping(ctx)
}

func queryTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 8*time.Second)
}
