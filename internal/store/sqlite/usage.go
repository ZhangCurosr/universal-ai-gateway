package sqlite

import (
	"context"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// InsertUsageEvents 实现 STO-10：单事务批量 INSERT OR IGNORE（request_id 幂等）。
func (d *DB) InsertUsageEvents(ctx context.Context, events []store.UsageEvent) error {
	// TODO(T02)
	return apperr.NotImplemented("T02")
}

// SumCostByClient 实现 STO-11。
func (d *DB) SumCostByClient(ctx context.Context) (map[int64]int64, error) {
	// TODO(T02)
	return nil, apperr.NotImplemented("T02")
}

// QueryUsage 实现 STO-12。
func (d *DB) QueryUsage(ctx context.Context, q store.UsageQuery) ([]store.UsageSummary, error) {
	// TODO(T02): created_at ∈ [From, To)，按 client_id, model 分组并排序。
	return nil, apperr.NotImplemented("T02")
}
