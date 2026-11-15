package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// AggregateUsage 实现 STO-23（T79）。两次分组查询：先求请求数、估算请求数与成本，
// 再用 json_each 把用量展开为（单位，数量）按单位求和——SQL 中从不出现具体单位名，适配器报告什么就统计什么。
// 分组列用 GROUP BY 序号引用，SQL 只拼接固定的列表达式与占位符，不拼接任何值。
func (d *DB) AggregateUsage(ctx context.Context, q store.UsageAggQuery) ([]store.UsageBucket, error) {
	dims, args, err := aggDims(q)
	if err != nil {
		return nil, err
	}
	where, wargs := aggWhere(q)
	args = append(args, wargs...)
	n := len(dims)
	cols := make([]string, n)
	nums := make([]string, n)
	for i, dm := range dims {
		cols[i] = dm.expr
		nums[i] = strconv.Itoa(i + 1)
	}
	sel := strings.Join(cols, ", ")
	if n > 0 {
		sel += ", "
	}
	group, order := "", ""
	if n > 0 {
		group = " GROUP BY " + strings.Join(nums, ", ")
		order = " ORDER BY " + strings.Join(nums, ", ")
	}
	from := ` FROM usage_events e LEFT JOIN upstream_keys k ON k.id = e.key_id`

	// 1. 请求数、估算请求数、成本
	q1 := `SELECT ` + sel + `COUNT(*), COALESCE(SUM(CASE WHEN e.usage_source = 'estimated' THEN 1 ELSE 0 END), 0), COALESCE(SUM(e.cost_nano_usd), 0)` +
		from + where + group + order + ` LIMIT ` + strconv.Itoa(store.MaxUsageAggBuckets+1)
	rows, err := d.db.QueryContext(ctx, q1, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate usage: %w", err)
	}
	var out []store.UsageBucket
	index := map[string]int{}
	err = scanAgg(rows, n, func(vals []any, rest []any) error {
		b := store.UsageBucket{Usage: map[string]int64{}}
		fillDims(&b, dims, vals)
		b.Requests, b.EstimatedRequests, b.CostNanoUSD = *rest[0].(*int64), *rest[1].(*int64), *rest[2].(*int64)
		if b.Requests == 0 {
			return nil // 不分组且区间内没有事件时，聚合查询仍返回一行全 0
		}
		index[dimKey(vals)] = len(out)
		out = append(out, b)
		return nil
	}, 3)
	if err != nil {
		return nil, err
	}
	if len(out) > store.MaxUsageAggBuckets {
		return nil, store.ErrTooManyBuckets
	}
	if len(out) == 0 {
		return []store.UsageBucket{}, nil
	}

	// 2. 各单位之和
	q2 := `SELECT ` + sel + `j.key, COALESCE(SUM(CAST(j.value AS INTEGER)), 0)` +
		from + `, json_each(e.usage) j` + where + ` GROUP BY ` + strings.Join(append(nums, strconv.Itoa(n+1)), ", ")
	rows, err = d.db.QueryContext(ctx, q2, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate usage units: %w", err)
	}
	err = scanAgg(rows, n, func(vals []any, rest []any) error {
		i, ok := index[dimKey(vals)]
		if !ok {
			return nil // 第一次查询之后才写入的事件（两次查询之间的并发写入）
		}
		unit := rest[0].(*sql.NullString)
		if unit.Valid && unit.String != "" {
			out[i].Usage[unit.String] += *rest[1].(*int64)
		}
		return nil
	}, 0)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type aggDim struct {
	name string
	expr string
}

// aggDims 校验分组并返回列表达式与它们用到的参数（只有时间桶有参数）。
func aggDims(q store.UsageAggQuery) ([]aggDim, []any, error) {
	if !q.From.Before(q.To) || q.To.Sub(q.From) > store.MaxUsageAggRange {
		return nil, nil, fmt.Errorf("%w: range must be positive and at most %s", store.ErrInvalidArgument, store.MaxUsageAggRange)
	}
	if len(q.GroupBy) > store.MaxUsageAggGroups {
		return nil, nil, fmt.Errorf("%w: at most %d group dimensions", store.ErrInvalidArgument, store.MaxUsageAggGroups)
	}
	var dims []aggDim
	var args []any
	seen := map[string]bool{}
	for _, g := range q.GroupBy {
		if seen[g] {
			return nil, nil, fmt.Errorf("%w: duplicate group %q", store.ErrInvalidArgument, g)
		}
		seen[g] = true
		switch g {
		case store.GroupTime:
			var width int64
			switch q.Interval {
			case store.IntervalHour:
				if q.To.Sub(q.From) > store.MaxHourlyRange {
					return nil, nil, fmt.Errorf("%w: hourly buckets need a range of at most %s", store.ErrInvalidArgument, store.MaxHourlyRange)
				}
				width = time.Hour.Milliseconds()
			case store.IntervalDay:
				width = (24 * time.Hour).Milliseconds()
			default:
				return nil, nil, fmt.Errorf("%w: interval must be hour or day", store.ErrInvalidArgument)
			}
			loc := q.TZ
			if loc == nil {
				loc = time.UTC
			}
			// SPEC-GAP(T79)：时区偏移取区间起点的偏移；区间跨夏令时切换时，切换后的日界偏一小时。
			_, offSec := q.From.In(loc).Zone()
			off := int64(offSec) * 1000
			dims = append(dims, aggDim{g, "((e.created_at + ?) / ?) * ? - ?"})
			args = append(args, off, width, width, off)
		case store.GroupClient:
			dims = append(dims, aggDim{g, "e.client_id"})
		case store.GroupModel:
			dims = append(dims, aggDim{g, "e.model"})
		case store.GroupKey:
			dims = append(dims, aggDim{g, "e.key_id"})
		case store.GroupPool:
			dims = append(dims, aggDim{g, "COALESCE(k.pool, '')"})
		default:
			return nil, nil, fmt.Errorf("%w: unknown group %q (time, client, model, key, pool)", store.ErrInvalidArgument, g)
		}
	}
	return dims, args, nil
}

func aggWhere(q store.UsageAggQuery) (string, []any) {
	conds := []string{"e.created_at >= ?", "e.created_at < ?"}
	args := []any{q.From.UnixMilli(), q.To.UnixMilli()}
	if q.ClientID != 0 {
		conds = append(conds, "e.client_id = ?")
		args = append(args, q.ClientID)
	}
	if q.Model != "" {
		conds = append(conds, "e.model = ?")
		args = append(args, q.Model)
	}
	if q.Pool != "" {
		conds = append(conds, "k.pool = ?")
		args = append(args, q.Pool)
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// scanAgg 逐行扫描：前 n 列是分组值（统一按文本读出，便于作为 map 键），之后 nInt 列 int64（nInt 为 0 时是「单位 + 数量」两列）。
func scanAgg(rows *sql.Rows, n int, fn func(vals, rest []any) error, nInt int) error {
	defer rows.Close() //nolint:errcheck // 只读查询，关闭失败无可处理
	for rows.Next() {
		vals := make([]any, n)
		for i := range vals {
			vals[i] = new(sql.NullString)
		}
		var rest []any
		if nInt == 0 {
			rest = []any{new(sql.NullString), new(int64)}
		} else {
			for i := 0; i < nInt; i++ {
				rest = append(rest, new(int64))
			}
		}
		if err := rows.Scan(append(append([]any{}, vals...), rest...)...); err != nil {
			return fmt.Errorf("scan usage aggregate: %w", err)
		}
		if err := fn(vals, rest); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate usage aggregate: %w", err)
	}
	return nil
}

func dimKey(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = v.(*sql.NullString).String
	}
	return strings.Join(parts, "\x00")
}

func fillDims(b *store.UsageBucket, dims []aggDim, vals []any) {
	for i, dm := range dims {
		s := vals[i].(*sql.NullString).String
		switch dm.name {
		case store.GroupTime:
			if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
				b.Time = time.UnixMilli(ms).UTC()
			}
		case store.GroupClient:
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				b.ClientID = id
			}
		case store.GroupKey:
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				b.KeyID = id
			}
		case store.GroupModel:
			b.Model = s
		case store.GroupPool:
			b.Pool = s
		}
	}
}
