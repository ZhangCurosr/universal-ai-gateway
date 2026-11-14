package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
)

// ADM-25：额度字段与 exhausted 状态；没有额度的池不输出额度字段
func TestKeyViewQuota(t *testing.T) {
	env := newTestEnv(t)
	pools := append([]config.PoolConfig(nil), env.cfg.Pools...)
	pools[0].Quota = &config.QuotaConfig{Unit: "credits", Limit: 3, Period: config.QuotaPeriodDay, Strategy: config.QuotaDrain}
	env.scheduler.SetPools(scheduler.PoolsFromConfig(pools))
	ids := map[string]int64{}
	for _, k := range []struct{ pool, name, secret string }{
		{"openai-pool", "full", "sk-quota-full-000001"}, {"openai-pool", "some", "sk-quota-some-000002"}, {"anthropic-pool", "free", "sk-quota-free-000003"},
	} {
		rec := env.req("POST", "/admin/api/keys", CreateKeyRequest{Pool: k.pool, Name: k.name, Secret: k.secret}, env.adminTok)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", k.name, rec.Code, rec.Body.String())
		}
		var out struct {
			Key KeyView `json:"key"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out) // 解析失败时下面的断言会报告
		ids[k.name] = out.Key.ID
	}
	env.scheduler.RecordUsage(ids["full"], map[string]int64{"credits": 3})
	env.scheduler.RecordUsage(ids["some"], map[string]int64{"credits": 1})
	full, some, free := findKeyView(t, env, ids["full"]), findKeyView(t, env, ids["some"]), findKeyView(t, env, ids["free"])
	if full.State != KeyStateExhausted || full.StoredState != "ready" || full.QuotaUsed != 3 || full.QuotaLimit != 3 || full.QuotaResetAt != "2026-11-09T00:00:00Z" {
		t.Fatalf("exhausted key = %+v", full)
	}
	if some.State != "ready" || some.StoredState != "" || some.QuotaUsed != 1 || some.QuotaLimit != 3 {
		t.Fatalf("partly used key = %+v", some)
	}
	if free.QuotaLimit != 0 || free.QuotaUsed != 0 || free.QuotaResetAt != "" {
		t.Fatalf("key without quota = %+v", free)
	}
	// 停用优先于额度用尽
	rec := env.req("PATCH", "/admin/api/keys/"+jsonID(ids["full"]), `{"state":"disabled"}`, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if v := findKeyView(t, env, ids["full"]); v.State != "disabled" || v.StoredState != "" {
		t.Fatalf("disabled exhausted key = %+v", v)
	}
}

func jsonID(id int64) string {
	b, _ := json.Marshal(id) // 整数必然可以序列化
	return string(b)
}
