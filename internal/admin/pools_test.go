package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// poolsEnv：openai-pool 声明额度（credits 100/天），三把 Key：a 用 30，b 用尽，c 停用；anthropic-pool 一把 Key 无额度。
func poolsEnv(t *testing.T) (*testEnv, map[string]int64) {
	t.Helper()
	env := newTestEnv(t)
	pools := append([]config.PoolConfig(nil), env.cfg.Pools...)
	pools[0].Quota = &config.QuotaConfig{Unit: "credits", Limit: 100, Period: config.QuotaPeriodDay, Strategy: config.QuotaDrain}
	env.scheduler.SetPools(scheduler.PoolsFromConfig(pools))
	ids := map[string]int64{}
	for _, k := range []struct{ pool, name string }{{"openai-pool", "a"}, {"openai-pool", "b"}, {"openai-pool", "c"}, {"anthropic-pool", "d"}} {
		rec := env.req("POST", "/admin/api/keys", CreateKeyRequest{Pool: k.pool, Name: k.name, Secret: "sk-pools-" + k.name + "-0000001"}, env.adminTok)
		var out struct {
			Key KeyView `json:"key"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out) // 解析失败时下面的断言会报告
		ids[k.name] = out.Key.ID
	}
	env.req("PATCH", fmt.Sprintf("/admin/api/keys/%d", ids["c"]), `{"state":"disabled"}`, env.adminTok)
	env.scheduler.RecordUsage(ids["a"], map[string]int64{"credits": 30})
	env.scheduler.RecordUsage(ids["b"], map[string]int64{"credits": 100})
	// 最近 24 小时的计费事件：a 24 点、b 24 点（另有一条 25 小时前的不计入）
	now := *env.now
	ev := func(req string, key int64, at time.Time, credits int64) store.UsageEvent {
		return store.UsageEvent{RequestID: req, ClientID: env.adminID, KeyID: key, Model: "gpt-4o", Op: "call",
			Usage: map[string]int64{"credits": credits}, CostNanoUSD: credits * 10, UsageSource: store.UsageSourceUpstream, CreatedAt: at}
	}
	if err := env.store.InsertUsageEvents(context.Background(), []store.UsageEvent{
		ev("r1", ids["a"], now.Add(-time.Hour), 20), ev("r2", ids["a"], now.Add(-2*time.Hour), 4),
		ev("r3", ids["b"], now.Add(-3*time.Hour), 24), ev("r4", ids["a"], now.Add(-25*time.Hour), 500),
	}); err != nil {
		t.Fatal(err)
	}
	// 尝试统计：a 一次成功、一次 429
	for _, kind := range []protocol.OutcomeKind{protocol.OutcomeSuccess, protocol.OutcomeRateLimited} {
		l, err := env.scheduler.Acquire(scheduler.AcquireRequest{Pool: "openai-pool"})
		if err != nil {
			t.Fatal(err)
		}
		if l.Key.ID != ids["a"] {
			t.Fatalf("drain picked %d, want a", l.Key.ID)
		}
		l.Finish(protocol.Outcome{Kind: kind, RetryAfter: time.Second})
	}
	return env, ids
}

// ADM-30：池的状态计数、额度、消耗速度与预计用完时间；没有额度的池不输出 quota
func TestPoolsView(t *testing.T) {
	env, _ := poolsEnv(t)
	rec := env.req("GET", "/admin/api/pools", nil, env.adminTok)
	var out struct {
		Pools []PoolView `json:"pools"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Pools) != 2 {
		t.Fatalf("pools: %d %s", rec.Code, rec.Body.String())
	}
	p := out.Pools[0]
	if p.Pool != "openai-pool" || p.Keys != (PoolKeys{Total: 3, Available: 1, Cooling: 1, Exhausted: 1, Disabled: 1}) {
		t.Fatalf("keys = %+v", p.Keys)
	}
	if p.Requests24h != 3 || p.FailureRate24h != 0.5 {
		t.Fatalf("24h = %d requests, rate %v", p.Requests24h, p.FailureRate24h)
	}
	q := p.Quota
	// 已用 130（含用尽的 b），剩余只算可用的 a：70；最近 24 小时 48 点 → 2/小时；70 ÷ 2 = 35 小时后用完，晚于次日 0 点重置
	if q == nil || q.Unit != "credits" || q.LimitPerKey != 100 || q.Used != 130 || q.Left != 70 || q.BurnPerHour != 2 ||
		!q.LastsUntilReset || q.RunsOutAt != "" || q.NextReset != "2026-11-09T00:00:00Z" {
		t.Fatalf("quota = %+v", q)
	}
	if a := out.Pools[1]; a.Pool != "anthropic-pool" || a.Quota != nil || a.Keys.Available != 1 {
		t.Fatalf("pool without quota = %+v", a)
	}
}

// ADM-30：三种预计结果
func TestPoolQuotaRunsOut(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	day := scheduler.Quota{Unit: "c", Limit: 10, Schedule: config.QuotaSchedule{Period: config.QuotaPeriodDay, Loc: time.UTC}}
	total := scheduler.Quota{Unit: "c", Limit: 10, Schedule: config.QuotaSchedule{Period: config.QuotaPeriodTotal}}
	if q := poolQuota(day, 0, 6, 48, now); q.RunsOutAt != "2026-11-08T15:00:00Z" || q.LastsUntilReset {
		t.Fatalf("runs out before reset = %+v", q)
	}
	if q := poolQuota(day, 0, 6, 0, now); !q.LastsUntilReset || q.RunsOutAt != "" {
		t.Fatalf("no burn with reset = %+v", q)
	}
	if q := poolQuota(total, 0, 6, 0, now); q.LastsUntilReset || q.RunsOutAt != "" || q.NextReset != "" {
		t.Fatalf("no burn without reset = %+v", q)
	}
	if q := poolQuota(total, 0, 240, 24, now); q.RunsOutAt != "2026-11-18T12:00:00Z" {
		t.Fatalf("total period = %+v", q)
	}
}

// ADM-29：用量、费用与 24 小时尝试统计；池过滤与参数校验
func TestKeyStatsView(t *testing.T) {
	env, ids := poolsEnv(t)
	rec := env.req("GET", "/admin/api/keys/stats?pool=openai-pool", nil, env.adminTok)
	var out struct {
		Keys []KeyStatsItem `json:"keys"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Keys) != 3 {
		t.Fatalf("stats: %d %s", rec.Code, rec.Body.String())
	}
	a := out.Keys[0]
	if a.ID != ids["a"] || a.Requests != 2 || a.Usage["credits"] != 24 || a.CostNanoUSD != 240 || a.Attempts24h != 2 ||
		a.Failures24h["rate_limited"] != 1 || a.FailureRate24h != 0.5 || a.LastFailure == nil || a.LastFailure.Kind != "rate_limited" {
		t.Fatalf("key a = %+v", a)
	}
	if c := out.Keys[2]; c.Requests != 0 || c.Usage == nil || c.Failures24h == nil || c.LastFailure != nil {
		t.Fatalf("idle key = %+v", c)
	}
	// 指定更早的时间段：只有 25 小时前那一条
	from := env.now.Add(-26 * time.Hour).Format(time.RFC3339)
	to := env.now.Add(-24 * time.Hour).Format(time.RFC3339)
	rec = env.req("GET", "/admin/api/keys/stats?from="+from+"&to="+to, nil, env.adminTok)
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Keys) != 4 || out.Keys[0].Usage["credits"] != 500 || out.Keys[0].Attempts24h != 2 {
		t.Fatalf("earlier range = %s", rec.Body.String())
	}
	for _, q := range []string{"?pool=nope", "?from=yesterday", "?to=x", "?from=" + to + "&to=" + from,
		"?from=" + env.now.Add(-40*24*time.Hour).Format(time.RFC3339)} {
		if rec := env.req("GET", "/admin/api/keys/stats"+q, nil, env.adminTok); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
}
