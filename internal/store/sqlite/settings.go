package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetSetting 查询系统设置，实现 STO-15。
// 键不存在时返回 ("", false, nil)。
func (d *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	const query = `SELECT value FROM system_settings WHERE key = ?`
	var value string
	if err := d.db.QueryRowContext(ctx, query, key).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get setting %q: %w", key, err)
	}
	return value, true, nil
}

// PutSetting 设置或更新系统设置，实现 STO-15。
func (d *DB) PutSetting(ctx context.Context, key, value string) error {
	now := d.now().UnixMilli()
	const query = `
		INSERT INTO system_settings (key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at`

	if _, err := d.db.ExecContext(ctx, query, key, value, now); err != nil {
		return fmt.Errorf("put setting %q: %w", key, err)
	}
	return nil
}
