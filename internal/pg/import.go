package pg

import (
	"context"
	"errors"

	"prism-2api/internal/persist"

	"github.com/jackc/pgx/v5"
)

// ImportLegacy 在 documents 为空时，把凭据目录里的旧 JSON 迁入 PostgreSQL。
// 只读文件、不回写。库里已有任意文档则跳过。
func ImportLegacy(ctx context.Context, d *DB, credDir string) (bool, error) {
	if d == nil || d.pool == nil {
		return false, nil
	}
	has, err := d.hasDocuments(ctx)
	if err != nil || has {
		return false, err
	}
	return persist.ImportLegacyDir(d, credDir)
}

func (d *DB) hasDocuments(ctx context.Context) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var n int
	err := d.pool.QueryRow(ctx, `SELECT 1 FROM documents LIMIT 1`).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
