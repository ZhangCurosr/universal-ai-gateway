package main

import (
	"context"
	"log/slog"
	"reflect"
	"sort"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// quotaLedger 在结算之后把用量计入调度器的周期额度（QT-12，T63）。内核不感知额度。
type quotaLedger struct {
	ledger.Ledger
	sched *scheduler.Scheduler
}

// Settle 先结算，再计入额度：无论结算是否出错都计入，因为上游已经产生了用量。
func (q quotaLedger) Settle(r *ledger.Reservation, ev store.UsageEvent) error {
	err := q.Ledger.Settle(r, ev)
	q.sched.RecordUsage(ev.KeyID, ev.Usage)
	return err
}

// quotaUsageStore 是重算额度用量需要的存储能力（STO-21）。
type quotaUsageStore interface {
	SumKeyUsage(ctx context.Context, unit string, since time.Time, keyIDs []int64) (map[int64]int64, error)
}

// recomputeQuotaUsage 从计费事件重算 pools 中各池本周期的额度用量（QT-10、QT-11）；pools 为 nil 时重算全部声明了额度的池。
// 失败只记 Warn：计数从内存现值继续，不影响服务。
func recomputeQuotaUsage(ctx context.Context, st quotaUsageStore, sched *scheduler.Scheduler, pools []string, log *slog.Logger) {
	all := sched.QuotaPools()
	if pools == nil {
		for name := range all {
			pools = append(pools, name)
		}
		sort.Strings(pools)
	}
	for _, name := range pools {
		q, ok := all[name]
		if !ok {
			continue
		}
		ids := sched.PoolKeyIDs(name)
		for attempt := 0; attempt < 2; attempt++ { // 重算期间跨过重置时刻时再算一次
			start := q.PeriodStart(time.Now())
			used, err := st.SumKeyUsage(ctx, q.Unit, start, ids)
			if err != nil {
				log.Warn("quota usage recompute failed; counting from the current value", "pool", name, "error", err)
				break
			}
			if sched.SetQuotaUsage(name, start, used) {
				exhausted := 0
				for _, n := range used {
					if n >= q.Limit {
						exhausted++
					}
				}
				log.Info("quota usage restored", "pool", name, "keys", len(ids), "keys_with_usage", len(used),
					"exhausted", exhausted, "period_start", start.UTC().Format(time.RFC3339))
				break
			}
		}
	}
}

// quotaPoolsToRecompute 实现 QT-11：新加额度，或额度的单位、周期、重置时刻变化的池需要重算；
// 只改上限或策略时保留计数。
func quotaPoolsToRecompute(oldCfg, newCfg *config.Config) []string {
	old := map[string]*config.QuotaConfig{}
	if oldCfg != nil {
		for _, p := range oldCfg.Pools {
			old[p.Name] = p.Quota
		}
	}
	out := []string{}
	for _, p := range newCfg.Pools {
		n := p.Quota
		if n == nil {
			continue
		}
		o := old[p.Name]
		if o == nil || o.Unit != n.Unit || o.Period != n.Period || !reflect.DeepEqual(o.Reset, n.Reset) {
			out = append(out, p.Name)
		}
	}
	return out
}
