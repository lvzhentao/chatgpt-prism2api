package pg

import (
	"encoding/json"
	"strconv"
	"strings"

	"prism-2api/internal/admin"
)

// InsertLog 写入一条请求追踪。
func (d *DB) InsertLog(e admin.LogEntry) error {
	if d == nil || d.pool == nil {
		return nil
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	var headers any
	if len(e.Headers) > 0 {
		headers = e.Headers
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO request_logs (
			time, ip, method, path, status, account, model,
			prompt_tokens, completion_tokens, total_tokens, latency_ms, error,
			client_key_id, fail_class, retry_count, request_id, stream, headers, request_body
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,
			$8,$9,$10,$11,$12,
			$13,$14,$15,$16,$17,$18,$19
		)`,
		e.Time, e.IP, e.Method, e.Path, e.Status, e.Account, e.Model,
		e.Prompt, e.Completion, e.Total, e.Latency, e.Error,
		e.ClientKeyID, e.FailClass, e.RetryCount, e.RequestID, e.Stream, headers, e.RequestBody,
	)
	return err
}

// QueryLogs 最新在前，过滤条件与 LogStore.Query 对齐。
func (d *DB) QueryLogs(limit int, f admin.LogFilter) ([]admin.LogEntry, error) {
	if d == nil || d.pool == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 5000 {
		limit = 100
	}
	ctx, cancel := queryTimeout()
	defer cancel()

	q := strings.Builder{}
	q.WriteString(`SELECT id, time, ip, method, path, status, account, model,
		prompt_tokens, completion_tokens, total_tokens, latency_ms, error,
		client_key_id, fail_class, retry_count, request_id, stream, headers, request_body
		FROM request_logs WHERE 1=1`)
	args := make([]any, 0, 7)
	n := 1
	if f.Model != "" {
		q.WriteString(" AND model = $")
		q.WriteString(strconv.Itoa(n))
		args = append(args, f.Model)
		n++
	}
	if f.Account != "" {
		q.WriteString(" AND account = $")
		q.WriteString(strconv.Itoa(n))
		args = append(args, f.Account)
		n++
	}
	if f.Path != "" {
		q.WriteString(" AND path ILIKE $")
		q.WriteString(strconv.Itoa(n))
		args = append(args, "%"+f.Path+"%")
		n++
	}
	if f.FailClass != "" {
		q.WriteString(" AND fail_class = $")
		q.WriteString(strconv.Itoa(n))
		args = append(args, f.FailClass)
		n++
	}
	switch f.Status {
	case "":
	case "4xx":
		q.WriteString(" AND status >= 400 AND status < 500")
	case "5xx":
		q.WriteString(" AND status >= 500 AND status < 600")
	default:
		status := f.Status
		if len(status) == 3 && status[2] == 'x' {
			prefix := int(status[0]-'0') * 100
			q.WriteString(" AND status >= $")
			q.WriteString(strconv.Itoa(n))
			args = append(args, prefix)
			n++
			q.WriteString(" AND status < $")
			q.WriteString(strconv.Itoa(n))
			args = append(args, prefix+100)
			n++
		} else if code, err := strconv.Atoi(status); err == nil {
			q.WriteString(" AND status = $")
			q.WriteString(strconv.Itoa(n))
			args = append(args, code)
			n++
		}
	}
	q.WriteString(" ORDER BY time DESC LIMIT $")
	q.WriteString(strconv.Itoa(n))
	args = append(args, limit)

	rows, err := d.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]admin.LogEntry, 0, limit)
	for rows.Next() {
		var e admin.LogEntry
		var headers []byte
		if err := rows.Scan(
			&e.ID, &e.Time, &e.IP, &e.Method, &e.Path, &e.Status, &e.Account, &e.Model,
			&e.Prompt, &e.Completion, &e.Total, &e.Latency, &e.Error,
			&e.ClientKeyID, &e.FailClass, &e.RetryCount, &e.RequestID, &e.Stream, &headers, &e.RequestBody,
		); err != nil {
			return nil, err
		}
		if len(headers) > 0 {
			_ = json.Unmarshal(headers, &e.Headers)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetLog 按主键取一条。
func (d *DB) GetLog(id int64) (*admin.LogEntry, error) {
	if d == nil || d.pool == nil {
		return nil, nil
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	row := d.pool.QueryRow(ctx, `SELECT id, time, ip, method, path, status, account, model,
		prompt_tokens, completion_tokens, total_tokens, latency_ms, error,
		client_key_id, fail_class, retry_count, request_id, stream, headers, request_body
		FROM request_logs WHERE id = $1`, id)
	var e admin.LogEntry
	var headers []byte
	if err := row.Scan(
		&e.ID, &e.Time, &e.IP, &e.Method, &e.Path, &e.Status, &e.Account, &e.Model,
		&e.Prompt, &e.Completion, &e.Total, &e.Latency, &e.Error,
		&e.ClientKeyID, &e.FailClass, &e.RetryCount, &e.RequestID, &e.Stream, &headers, &e.RequestBody,
	); err != nil {
		return nil, err
	}
	if len(headers) > 0 {
		_ = json.Unmarshal(headers, &e.Headers)
	}
	return &e, nil
}

// ComputeLogStats 全表聚合（受保留天数限制，表不会无限涨）。
func (d *DB) ComputeLogStats() (admin.Stats, error) {
	st := admin.Stats{
		ByAccount:       map[string]int{},
		ByModel:         map[string]int{},
		ByPath:          map[string]int{},
		ByFailClass:     map[string]int{},
		ByClientKey:     map[string]int{},
		TokensByAccount: map[string]int{},
		TokensByModel:   map[string]int{},
	}
	if d == nil || d.pool == nil {
		return st, nil
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	row := d.pool.QueryRow(ctx, `
		SELECT COUNT(*),
			-- 客户端主动断开（499/canceled）不是服务端错误：不计入错误率。
			-- 双条件兜底——status 未落库但 fail_class 落了的行（或反之）都能被剔除。
			COUNT(*) FILTER (WHERE status >= 400 AND status <> 499 AND fail_class <> 'canceled'),
			COALESCE(SUM(prompt_tokens),0),
			COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0),
			COALESCE(AVG(latency_ms),0)
		FROM request_logs`)
	var avg float64
	if err := row.Scan(&st.TotalRequests, &st.ErrorRequests, &st.TotalPrompt, &st.TotalCompletion, &st.TotalTokens, &avg); err != nil {
		return st, err
	}
	st.AvgLatency = int64(avg)
	if st.TotalRequests > 0 {
		st.ErrorRate = float64(st.ErrorRequests) / float64(st.TotalRequests)
	}

	fill := func(q string, dest map[string]int) error {
		rows, err := d.pool.Query(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				return err
			}
			if k != "" {
				dest[k] = n
			}
		}
		return rows.Err()
	}
	if err := fill(`SELECT account, COUNT(*) FROM request_logs WHERE account <> '' GROUP BY account`, st.ByAccount); err != nil {
		return st, err
	}
	if err := fill(`SELECT model, COUNT(*) FROM request_logs WHERE model <> '' GROUP BY model`, st.ByModel); err != nil {
		return st, err
	}
	if err := fill(`SELECT path, COUNT(*) FROM request_logs GROUP BY path`, st.ByPath); err != nil {
		return st, err
	}
	if err := fill(`SELECT fail_class, COUNT(*) FROM request_logs WHERE fail_class <> '' GROUP BY fail_class`, st.ByFailClass); err != nil {
		return st, err
	}
	if err := fill(`SELECT client_key_id::text, COUNT(*) FROM request_logs WHERE client_key_id IS NOT NULL GROUP BY client_key_id`, st.ByClientKey); err != nil {
		return st, err
	}
	if err := fill(`SELECT account, COALESCE(SUM(total_tokens),0) FROM request_logs WHERE account <> '' GROUP BY account`, st.TokensByAccount); err != nil {
		return st, err
	}
	if err := fill(`SELECT model, COALESCE(SUM(total_tokens),0) FROM request_logs WHERE model <> '' GROUP BY model`, st.TokensByModel); err != nil {
		return st, err
	}
	return st, nil
}

// ClearLogs 清空追踪表。
func (d *DB) ClearLogs() error {
	if d == nil || d.pool == nil {
		return nil
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	_, err := d.pool.Exec(ctx, `TRUNCATE request_logs`)
	return err
}

// PurgeOlderThan 删除超过 retainDays 的追踪（默认 7 天）。
func (d *DB) PurgeOlderThan(retainDays int) (int64, error) {
	if d == nil || d.pool == nil {
		return 0, nil
	}
	if retainDays <= 0 {
		retainDays = DefaultRetention
	}
	ctx, cancel := queryTimeout()
	defer cancel()
	tag, err := d.pool.Exec(ctx, `DELETE FROM request_logs WHERE time < now() - make_interval(days => $1)`, retainDays)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
