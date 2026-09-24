package pg

import (
	"encoding/json"
)

// UpsertBatch 按 id 覆盖写入批次 JSON。
func (d *DB) UpsertBatch(id, status string, payload []byte) error {
	if d == nil || d.pool == nil || id == "" {
		return nil
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if !json.Valid(payload) {
		payload = []byte("{}")
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	_, err := d.pool.Exec(ctx, `
		INSERT INTO batches (id, processing_status, payload, updated_at)
		VALUES ($1, $2, $3::jsonb, now())
		ON CONFLICT (id) DO UPDATE SET
			processing_status = EXCLUDED.processing_status,
			payload = EXCLUDED.payload,
			updated_at = now()`,
		id, status, payload,
	)
	return err
}

// ListBatchPayloads 返回全部批次 payload（JSON 字节）。
func (d *DB) ListBatchPayloads() ([][]byte, error) {
	if d == nil || d.pool == nil {
		return nil, nil
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	rows, err := d.pool.Query(ctx, `SELECT payload FROM batches ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}
