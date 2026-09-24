package pg

import (
	"encoding/json"
	"errors"

	"prism-2api/internal/persist"

	"github.com/jackc/pgx/v5"
)

var _ persist.Backend = (*DB)(nil)

// LoadDoc 读一条文档；不存在返回 (nil, nil)。
func (d *DB) LoadDoc(kind, id string) ([]byte, error) {
	if d == nil || d.pool == nil {
		return nil, nil
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	var raw []byte
	err := d.pool.QueryRow(ctx, `SELECT payload FROM documents WHERE kind=$1 AND id=$2`, kind, id).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return raw, nil
}

// SaveDoc 覆盖写入（payload 必须是 JSON）。
func (d *DB) SaveDoc(kind, id string, payload []byte) error {
	if d == nil || d.pool == nil {
		return nil
	}
	if len(payload) == 0 || !json.Valid(payload) {
		payload = []byte("{}")
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	_, err := d.pool.Exec(ctx, `
		INSERT INTO documents (kind, id, payload, updated_at)
		VALUES ($1, $2, $3::jsonb, now())
		ON CONFLICT (kind, id) DO UPDATE SET payload = EXCLUDED.payload, updated_at = now()`,
		kind, id, payload)
	return err
}

// DeleteDoc 删除一条文档。
func (d *DB) DeleteDoc(kind, id string) error {
	if d == nil || d.pool == nil {
		return nil
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	_, err := d.pool.Exec(ctx, `DELETE FROM documents WHERE kind=$1 AND id=$2`, kind, id)
	return err
}

// ListDocs 列出某 kind 的全部文档。
func (d *DB) ListDocs(kind string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if d == nil || d.pool == nil {
		return out, nil
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	rows, err := d.pool.Query(ctx, `SELECT id, payload FROM documents WHERE kind=$1`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		out[id] = raw
	}
	return out, rows.Err()
}
