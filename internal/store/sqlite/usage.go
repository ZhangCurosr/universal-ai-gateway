package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// InsertUsageEvents 实现 STO-10：单事务批量插入，重复 request_id 静默跳过（幂等）。
//
// 用 ON CONFLICT(request_id) DO NOTHING 而不是 INSERT OR IGNORE：后者还会静默吞掉
// NOT NULL / CHECK 违例，导致计费事件被丢弃却不报错（AGENTS §3 #4）。
func (d *DB) InsertUsageEvents(ctx context.Context, events []store.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin insert usage events tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const query = `
		INSERT INTO usage_events (
			request_id, client_id, key_id, model, op,
			usage, cost_nano_usd, usage_source, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_id) DO NOTHING`

	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare insert usage event stmt: %w", err)
	}
	defer stmt.Close()

	for i := range events {
		ev := &events[i]
		usageJSON := "{}"
		if ev.Usage != nil {
			b, err := json.Marshal(ev.Usage)
			if err != nil {
				return fmt.Errorf("marshal usage for request %s: %w", ev.RequestID, err)
			}
			usageJSON = string(b)
		}

		source := ev.UsageSource
		if source == "" {
			source = store.UsageSourceUpstream
		}

		createdAt := ev.CreatedAt.UnixMilli()
		if ev.CreatedAt.IsZero() {
			createdAt = d.now().UnixMilli()
		}

		if _, err := stmt.ExecContext(ctx,
			ev.RequestID,
			ev.ClientID,
			ev.KeyID,
			ev.Model,
			ev.Op,
			usageJSON,
			ev.CostNanoUSD,
			source,
			createdAt,
		); err != nil {
			return fmt.Errorf("insert usage event %s: %w", ev.RequestID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage events: %w", err)
	}
	return nil
}

// SumCostByClient 实现 STO-11。
func (d *DB) SumCostByClient(ctx context.Context) (map[int64]int64, error) {
	const query = `SELECT client_id, SUM(cost_nano_usd) FROM usage_events GROUP BY client_id`
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sum cost by client: %w", err)
	}
	defer rows.Close()

	res := make(map[int64]int64)
	for rows.Next() {
		var clientID, sumCost int64
		if err := rows.Scan(&clientID, &sumCost); err != nil {
			return nil, fmt.Errorf("scan sum cost: %w", err)
		}
		res[clientID] = sumCost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sum cost: %w", err)
	}
	return res, nil
}

// QueryUsage 实现 STO-12。
func (d *DB) QueryUsage(ctx context.Context, q store.UsageQuery) ([]store.UsageSummary, error) {
	where := make([]string, 0, 3)
	args := make([]any, 0, 3)

	if !q.From.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, q.From.UnixMilli())
	}
	if !q.To.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, q.To.UnixMilli())
	}
	if q.ClientID != 0 {
		where = append(where, "client_id = ?")
		args = append(args, q.ClientID)
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}

	query := `
		SELECT client_id, model, COUNT(*), SUM(cost_nano_usd), json_group_array(usage)
		FROM usage_events` + whereClause + `
		GROUP BY client_id, model
		ORDER BY client_id ASC, model ASC`

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer rows.Close()

	summaries := make([]store.UsageSummary, 0)
	for rows.Next() {
		var (
			s             store.UsageSummary
			usageArrayStr sql.NullString
		)
		if err := rows.Scan(&s.ClientID, &s.Model, &s.Requests, &s.CostNanoUSD, &usageArrayStr); err != nil {
			return nil, fmt.Errorf("scan usage summary: %w", err)
		}
		s.Usage = make(map[string]int64)
		if usageArrayStr.Valid && usageArrayStr.String != "" {
			// json_group_array 把 TEXT 列编码成 JSON 字符串数组，每个元素是一行的 usage 对象文本。
			var rows []string
			if err := json.Unmarshal([]byte(usageArrayStr.String), &rows); err != nil {
				return nil, fmt.Errorf("decode usage array for client %d model %s: %w", s.ClientID, s.Model, err)
			}
			for _, raw := range rows {
				var m map[string]int64
				if err := json.Unmarshal([]byte(raw), &m); err != nil {
					return nil, fmt.Errorf("decode usage for client %d model %s: %w", s.ClientID, s.Model, err)
				}
				for unit, count := range m {
					s.Usage[unit] += count
				}
			}
		}
		summaries = append(summaries, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage summary: %w", err)
	}
	return summaries, nil
}
