package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// SaveRuntimeState 实现 STO-18：单个事务内清空 key_runtime、task_pins 后批量插入。
func (d *DB) SaveRuntimeState(ctx context.Context, keys []store.KeyRuntime, pins []store.TaskPin) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save runtime state tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚无效果；失败路径的回滚错误无可处理
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_runtime`); err != nil {
		return fmt.Errorf("clear key_runtime: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_pins`); err != nil {
		return fmt.Errorf("clear task_pins: %w", err)
	}
	now := d.now().UnixMilli()
	if len(keys) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO key_runtime (key_id, cooldown_until, auth_failures, updated_at) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare key_runtime insert: %w", err)
		}
		defer stmt.Close() //nolint:errcheck // 语句随事务结束释放
		for _, k := range keys {
			if _, err := stmt.ExecContext(ctx, k.KeyID, millisOrZero(k.CooldownUntil), k.AuthFailures, now); err != nil {
				return fmt.Errorf("insert key_runtime %d: %w", k.KeyID, err)
			}
		}
	}
	if len(pins) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO task_pins (affinity_key, key_id, expires_at) VALUES (?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare task_pins insert: %w", err)
		}
		defer stmt.Close() //nolint:errcheck // 语句随事务结束释放
		for _, p := range pins {
			if _, err := stmt.ExecContext(ctx, p.AffinityKey, p.KeyID, p.ExpiresAt.UnixMilli()); err != nil {
				return fmt.Errorf("insert task_pin: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit runtime state: %w", err)
	}
	return nil
}

// LoadRuntimeState 实现 STO-19：只返回冷却未结束或有失败计数的 Key，以及未过期的固定记录。
func (d *DB) LoadRuntimeState(ctx context.Context, now time.Time) ([]store.KeyRuntime, []store.TaskPin, error) {
	ms := now.UnixMilli()
	rows, err := d.db.QueryContext(ctx,
		`SELECT key_id, cooldown_until, auth_failures FROM key_runtime WHERE cooldown_until > ? OR auth_failures > 0 ORDER BY key_id`, ms)
	if err != nil {
		return nil, nil, fmt.Errorf("load key_runtime: %w", err)
	}
	defer rows.Close() //nolint:errcheck // 只读查询
	var keys []store.KeyRuntime
	for rows.Next() {
		var k store.KeyRuntime
		var until int64
		if err := rows.Scan(&k.KeyID, &until, &k.AuthFailures); err != nil {
			return nil, nil, fmt.Errorf("scan key_runtime: %w", err)
		}
		if until > ms {
			k.CooldownUntil = fromMillis(until)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate key_runtime: %w", err)
	}

	prow, err := d.db.QueryContext(ctx, `SELECT affinity_key, key_id, expires_at FROM task_pins WHERE expires_at > ? ORDER BY affinity_key`, ms)
	if err != nil {
		return nil, nil, fmt.Errorf("load task_pins: %w", err)
	}
	defer prow.Close() //nolint:errcheck // 只读查询
	var pins []store.TaskPin
	for prow.Next() {
		var p store.TaskPin
		var exp int64
		if err := prow.Scan(&p.AffinityKey, &p.KeyID, &exp); err != nil {
			return nil, nil, fmt.Errorf("scan task_pin: %w", err)
		}
		p.ExpiresAt = fromMillis(exp)
		pins = append(pins, p)
	}
	if err := prow.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate task_pins: %w", err)
	}
	return keys, pins, nil
}

func millisOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
