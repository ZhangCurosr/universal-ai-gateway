package sqlite

import (
	"context"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// InsertAuditRecords 实现 STO-13。
func (d *DB) InsertAuditRecords(ctx context.Context, recs []store.AuditRecord) error {
	// TODO(T02): 单事务批量插入；空切片返回 nil。
	return apperr.NotImplemented("T02")
}

// DeleteAuditBefore 实现 STO-14。
func (d *DB) DeleteAuditBefore(ctx context.Context, before time.Time) (int64, error) {
	// TODO(T02)
	return 0, apperr.NotImplemented("T02")
}
