package admin

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// Key 视图（ADM-29、ADM-30，任务卡 T65）：每把 Key 的用量与失败、每个池的容量与预计用完时间。只在控制面执行。

// KeyStatsItem 是 GET /admin/api/keys/stats 的一行（ADM-29）。
type KeyStatsItem struct {
	ID             int64            `json:"id"`
	Pool           string           `json:"pool"`
	Name           string           `json:"name"`
	Requests       int64            `json:"requests"`
	Usage          map[string]int64 `json:"usage"`
	CostNanoUSD    int64            `json:"cost_nano_usd"`
	Attempts24h    int64            `json:"attempts_24h"`
	Failures24h    map[string]int64 `json:"failures_24h"`
	FailureRate24h float64          `json:"failure_rate_24h"`
	LastFailure    *LastFailure     `json:"last_failure,omitempty"`
}

// LastFailure 是最近一次失败的尝试。
type LastFailure struct {
	Kind string `json:"kind"`
	At   string `json:"at"` // RFC 3339
}

// PoolView 是 GET /admin/api/pools 的一项（ADM-30）。
type PoolView struct {
	Pool           string     `json:"pool"`
	Protocol       string     `json:"protocol"`
	Keys           PoolKeys   `json:"keys"`
	Expiring7d     int        `json:"expiring_7d"`
	Requests24h    int64      `json:"requests_24h"`
	FailureRate24h float64    `json:"failure_rate_24h"`
	Quota          *PoolQuota `json:"quota,omitempty"`
}

// PoolKeys 是池中 Key 按状态的计数。available 与 T64 告警同口径（AL-01，冷却中的算可用）；cooling 是其中正在冷却的数量。
type PoolKeys struct {
	Total       int `json:"total"`
	Available   int `json:"available"`
	Cooling     int `json:"cooling"`
	Exhausted   int `json:"exhausted"`
	Expired     int `json:"expired"`
	Disabled    int `json:"disabled"`
	Quarantined int `json:"quarantined"`
}

// PoolQuota 是池的本期额度与预计用完时间（ADM-30）。
type PoolQuota struct {
	Unit            string  `json:"unit"`
	LimitPerKey     int64   `json:"limit_per_key"`
	Period          string  `json:"period"`
	Used            int64   `json:"used"`
	Left            int64   `json:"left"`
	NextReset       string  `json:"next_reset,omitempty"`
	BurnPerHour     float64 `json:"burn_per_hour"`
	RunsOutAt       string  `json:"runs_out_at,omitempty"`
	LastsUntilReset bool    `json:"lasts_until_reset"`
}

// maxStatsRange 与 store.MaxKeyUsageRange 一致。
const maxStatsRange = store.MaxKeyUsageRange

func (s *server) handleKeyStats(w http.ResponseWriter, r *http.Request) {
	now := s.d.Now().UTC()
	q := r.URL.Query()
	to, from := now, time.Time{}
	var err error
	if v := q.Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "invalid to parameter: RFC 3339 required"))
			return
		}
	}
	from = to.Add(-24 * time.Hour)
	if v := q.Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "invalid from parameter: RFC 3339 required"))
			return
		}
	}
	if !from.Before(to) || to.Sub(from) > maxStatsRange {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "from must be before to, and the range at most 31 days"))
		return
	}
	pool := q.Get("pool")
	if pool != "" && !s.configuredPools()[pool] {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("pool %q is not configured", pool)))
		return
	}
	keys, err := s.d.Store.ListUpstreamKeys(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	ids := make([]int64, 0, len(keys))
	for _, k := range keys {
		if pool == "" || k.Pool == pool {
			ids = append(ids, k.ID)
		}
	}
	usage, err := s.d.Store.SumUsageByKey(r.Context(), from, to, ids)
	if err != nil {
		writeError(w, err)
		return
	}
	byKey := make(map[int64]store.KeyUsage, len(usage))
	for _, u := range usage {
		byKey[u.KeyID] = u
	}
	stats := s.keyStats()
	items := make([]KeyStatsItem, 0, len(ids))
	for _, k := range keys {
		if pool != "" && k.Pool != pool {
			continue
		}
		u := byKey[k.ID]
		it := KeyStatsItem{ID: k.ID, Pool: k.Pool, Name: k.Name, Requests: u.Requests, Usage: u.Usage, CostNanoUSD: u.CostNanoUSD,
			Failures24h: map[string]int64{}}
		if it.Usage == nil {
			it.Usage = map[string]int64{}
		}
		if st, ok := stats[k.ID]; ok {
			it.Attempts24h, it.Failures24h, it.FailureRate24h = st.Attempts, st.Failures, round3(st.FailureRate())
			if st.LastFailure != "" {
				it.LastFailure = &LastFailure{Kind: st.LastFailure, At: st.LastFailedAt.UTC().Format(time.RFC3339)}
			}
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"keys": items, "from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339)})
}

func (s *server) keyStats() map[int64]scheduler.KeyStat {
	if s.d.Scheduler == nil {
		return nil
	}
	return s.d.Scheduler.KeyStats()
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func (s *server) handlePools(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	if cfg == nil || s.d.Scheduler == nil {
		writeJSON(w, http.StatusOK, map[string]any{"pools": []PoolView{}})
		return
	}
	now := s.d.Now()
	statuses := s.d.Scheduler.KeyStatus()
	stats := s.d.Scheduler.KeyStats()
	quotas := s.d.Scheduler.QuotaPools()
	idsByPool := map[string][]int64{}
	for _, st := range statuses {
		idsByPool[st.Pool] = append(idsByPool[st.Pool], st.ID)
	}
	var all []int64
	for _, ids := range idsByPool {
		all = append(all, ids...)
	}
	day, err := s.d.Store.SumUsageByKey(r.Context(), now.Add(-24*time.Hour), now, all)
	if err != nil {
		writeError(w, err)
		return
	}
	dayByKey := make(map[int64]store.KeyUsage, len(day))
	for _, u := range day {
		dayByKey[u.KeyID] = u
	}

	views := make([]PoolView, 0, len(cfg.Pools))
	for _, p := range cfg.Pools {
		v := PoolView{Pool: p.Name, Protocol: p.Protocol}
		var attempts, fails int64
		var used, left, burn int64
		q, hasQuota := quotas[p.Name]
		for _, st := range statuses {
			if st.Pool != p.Name {
				continue
			}
			v.Keys.Total++
			switch {
			case st.Persisted == scheduler.PersistedDisabled:
				v.Keys.Disabled++
			case st.Persisted == scheduler.PersistedQuarantined:
				v.Keys.Quarantined++
			case st.Expired:
				v.Keys.Expired++
			case st.QuotaExhausted:
				v.Keys.Exhausted++
			case st.Breaker == scheduler.BreakerOpen:
				v.Keys.Cooling++ // 熔断器打开：不算可用（AL-01），归入冷却
			default:
				v.Keys.Available++
				if st.CooldownRemaining > 0 {
					v.Keys.Cooling++
				}
				if hasQuota {
					left += max(st.QuotaLimit-st.QuotaUsed, 0)
				}
			}
			if st.Persisted == scheduler.PersistedReady && !st.Expired && !st.ExpiresAt.IsZero() && st.ExpiresAt.Before(now.Add(7*24*time.Hour)) {
				v.Expiring7d++
			}
			used += st.QuotaUsed
			ks := stats[st.ID]
			attempts += ks.Attempts
			for _, n := range ks.Failures {
				fails += n
			}
			u := dayByKey[st.ID]
			v.Requests24h += u.Requests
			if hasQuota {
				burn += u.Usage[q.Unit]
			}
		}
		if attempts > 0 {
			v.FailureRate24h = round3(float64(fails) / float64(attempts))
		}
		if hasQuota {
			v.Quota = poolQuota(q, used, left, burn, now)
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": views})
}

// poolQuota 实现 ADM-30 的预计用完时间：速度 = 最近 24 小时用量 ÷ 24。
func poolQuota(q scheduler.Quota, used, left, burn24h int64, now time.Time) *PoolQuota {
	pq := &PoolQuota{Unit: q.Unit, LimitPerKey: q.Limit, Period: q.Schedule.Period, Used: used, Left: left,
		BurnPerHour: math.Round(float64(burn24h)/24*100) / 100}
	reset := q.NextReset(now)
	if !reset.IsZero() {
		pq.NextReset = reset.UTC().Format(time.RFC3339)
	}
	if burn24h <= 0 {
		pq.LastsUntilReset = !reset.IsZero()
		return pq
	}
	runsOut := now.Add(time.Duration(float64(left) / (float64(burn24h) / 24) * float64(time.Hour)))
	if !reset.IsZero() && !runsOut.Before(reset) {
		pq.LastsUntilReset = true
		return pq
	}
	pq.RunsOutAt = runsOut.UTC().Format(time.RFC3339)
	return pq
}
