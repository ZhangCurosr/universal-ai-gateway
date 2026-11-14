package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// T65：一把 Key 总是 429（被重试换掉）、另一把成功。被重试掉的失败尝试也计入第一把 Key 的失败；
// 用量与计费一致；池视图给出可用数与剩余额度；两个管理面 MCP 工具可用。
func TestE2E_KeyViews(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer kv1-secret-aaaa" {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	g := startRESTGateway(t, func(c *config.Config) {
		c.Pools = append(c.Pools, config.PoolConfig{Name: "kv", Protocol: "rest", BaseURL: upstream.URL, MaxAttempts: 3,
			Quota: &config.QuotaConfig{Unit: "requests", Limit: 100, Period: config.QuotaPeriodTotal}})
		c.Models = append(c.Models, config.ModelConfig{Name: "kv-api", Pool: "kv", Price: map[string]string{"requests": "0.001"}})
		c.Normalize()
	})
	admin := "http://" + g.admin + "/admin/api"
	var cl struct {
		Token string `json:"token"`
	}
	doJSON(t, "POST", admin+"/clients", g.adminToken, map[string]any{"name": "kv", "role": "user", "quota_nano_usd": int64(1e12)}, &cl)
	var ids []int64
	for _, k := range []string{"kv1-secret-aaaa", "kv2-secret-bbbb"} {
		var out struct {
			Key struct {
				ID int64 `json:"id"`
			} `json:"key"`
		}
		doJSON(t, "POST", admin+"/keys", g.adminToken, map[string]any{"pool": "kv", "name": k[:3], "secret": k}, &out)
		ids = append(ids, out.Key.ID)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+g.data+"/rest/kv-api/x", nil)
	req.Header.Set("Authorization", "Bearer "+cl.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // 测试：关闭失败无影响
	if resp.StatusCode != 200 {
		t.Fatalf("call: %d", resp.StatusCode)
	}
	waitFor(t, func() bool { // 计费事件异步刷盘
		var s struct {
			Keys []struct {
				ID       int64 `json:"id"`
				Requests int64 `json:"requests"`
			} `json:"keys"`
		}
		doJSON(t, "GET", admin+"/keys/stats?pool=kv", g.adminToken, nil, &s)
		return len(s.Keys) == 2 && s.Keys[1].Requests == 1
	})

	var stats struct {
		Keys []struct {
			ID             int64                  `json:"id"`
			Requests       int64                  `json:"requests"`
			Usage          map[string]int64       `json:"usage"`
			CostNanoUSD    int64                  `json:"cost_nano_usd"`
			Attempts24h    int64                  `json:"attempts_24h"`
			Failures24h    map[string]int64       `json:"failures_24h"`
			FailureRate24h float64                `json:"failure_rate_24h"`
			LastFailure    *struct{ Kind string } `json:"last_failure"`
		} `json:"keys"`
	}
	doJSON(t, "GET", admin+"/keys/stats?pool=kv", g.adminToken, nil, &stats)
	k1, k2 := stats.Keys[0], stats.Keys[1]
	if k1.ID != ids[0] || k1.Requests != 0 || k1.Attempts24h != 1 || k1.Failures24h["rate_limited"] != 1 || k1.FailureRate24h != 1 || k1.LastFailure == nil || k1.LastFailure.Kind != "rate_limited" {
		t.Fatalf("key 1 = %+v", k1)
	}
	if k2.Requests != 1 || k2.Usage["requests"] != 1 || k2.CostNanoUSD != 1_000_000 || k2.Attempts24h != 1 || k2.FailureRate24h != 0 {
		t.Fatalf("key 2 = %+v", k2)
	}

	var pools struct {
		Pools []struct {
			Pool string `json:"pool"`
			Keys struct {
				Total, Available, Cooling int
			} `json:"keys"`
			Requests24h    int64   `json:"requests_24h"`
			FailureRate24h float64 `json:"failure_rate_24h"`
			Quota          *struct {
				Used, Left  int64
				BurnPerHour float64 `json:"burn_per_hour"`
				RunsOutAt   string  `json:"runs_out_at"`
			} `json:"quota"`
		} `json:"pools"`
	}
	doJSON(t, "GET", admin+"/pools", g.adminToken, nil, &pools)
	var kv = pools.Pools[len(pools.Pools)-1]
	if kv.Pool != "kv" || kv.Keys.Total != 2 || kv.Keys.Available != 2 || kv.Keys.Cooling != 1 || kv.Requests24h != 1 || kv.FailureRate24h != 0.5 {
		t.Fatalf("pool = %+v", kv)
	}
	// 已用 1；剩余 99 + 100；速度 1/24 每小时 → 199 × 24 小时后用完（total 周期没有重置）
	if q := kv.Quota; q == nil || q.Used != 1 || q.Left != 199 || q.BurnPerHour != 0.04 || q.RunsOutAt == "" {
		t.Fatalf("quota = %+v", kv.Quota)
	}
	if res := adminCall(t, g, "gateway_admin_pools", `{}`); res.IsError || !strings.Contains(res.Content[0].Text, `"runs_out_at"`) {
		t.Fatalf("MCP pools = %+v", res)
	}
	if res := adminCall(t, g, "gateway_admin_key_stats", `{"pool":"kv"}`); res.IsError || !strings.Contains(res.Content[0].Text, `"rate_limited":1`) ||
		strings.Contains(res.Content[0].Text, "kv1-secret") {
		t.Fatalf("MCP key stats = %+v", res)
	}
}

// waitFor 每 20 毫秒检查一次 cond，最多等 5 秒。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
