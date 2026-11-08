package sqlite

import (
	"context"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// GetSetting 实现 STO-15：不存在时返回 ("", false, nil)。
func (d *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	// TODO(T01): 查询 system_settings。
	return "", false, apperr.NotImplemented("T01")
}

// PutSetting 实现 STO-15：upsert。
func (d *DB) PutSetting(ctx context.Context, key, value string) error {
	// TODO(T01): INSERT ... ON CONFLICT(key) DO UPDATE。
	return apperr.NotImplemented("T01")
}
