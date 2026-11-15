package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// ADM-32、ADM-33（T79）：分组、时区、token 合计、单位目录（含只在数据中出现的新单位）、参数校验、CSV。
func TestUsageStats(t *testing.T) {
	env := newTestEnvWith(t, func(d *Deps) {
		d.Units = func() []UnitSource {
			return []UnitSource{{Name: "input_tokens", Protocols: []string{"openai_chat"}}, {Name: "input_tokens", Protocols: []string{"anthropic"}},
				{Name: "output_tokens", Protocols: []string{"openai_chat"}}, {Name: "credits", Protocols: []string{"tavily"}}}
		}
	})
	now := *env.now
	ev := func(id, model string, at time.Duration, usage map[string]int64, cost int64, est bool) store.UsageEvent {
		src := store.UsageSourceUpstream
		if est {
			src = store.UsageSourceEstimated
		}
		return store.UsageEvent{RequestID: id, ClientID: env.adminID, KeyID: 1, Model: model, Op: "call", Usage: usage, CostNanoUSD: cost, UsageSource: src, CreatedAt: now.Add(at)}
	}
	if err := env.store.InsertUsageEvents(context.Background(), []store.UsageEvent{
		ev("a", "gpt", -3*time.Hour, map[string]int64{"input_tokens": 100, "output_tokens": 10}, 1_500_000_000, false),
		ev("b", "gpt", -2*time.Hour, map[string]int64{"input_tokens": 50, "output_tokens": 5, "audio_input_tokens": 7}, 250, true),
		ev("c", "search", -1*time.Hour, map[string]int64{"credits": 2}, 0, false),
	}); err != nil {
		t.Fatal(err)
	}

	type resp struct {
		Items  []UsageStatsItem `json:"items"`
		Units  []UnitInfo       `json:"units"`
		Totals UsageStatsTotals `json:"totals"`
	}
	get := func(qs string) resp {
		t.Helper()
		rec := env.req("GET", "/admin/api/usage/stats"+qs, nil, env.adminTok)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", qs, rec.Code, rec.Body.String())
		}
		var r resp
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	r := get("?group=model")
	if len(r.Items) != 2 || r.Items[0].Model != "gpt" || r.Items[0].Tokens != 172 || r.Items[0].EstimatedRequests != 1 || r.Items[1].Tokens != 0 {
		t.Fatalf("by model: %+v", r.Items)
	}
	if r.Totals.Requests != 3 || r.Totals.Tokens != 172 || r.Totals.Usage["credits"] != 2 || r.Totals.CostNanoUSD != 1_500_000_250 {
		t.Fatalf("totals: %+v", r.Totals)
	}
	var names []string
	for _, u := range r.Units {
		names = append(names, u.Name+":"+u.Kind+":"+strings.Join(u.Protocols, "+"))
	}
	if got := strings.Join(names, ","); got != "input_tokens:tokens:anthropic+openai_chat,output_tokens:tokens:openai_chat,audio_input_tokens:tokens:,credits:other:tavily" {
		t.Fatalf("units: %s", got)
	}

	// 按小时、+08:00；按客户端带名称
	r = get("?group=time&interval=hour&tz=%2B08:00")
	if len(r.Items) != 3 || r.Items[0].Time != now.Add(-3*time.Hour).In(time.FixedZone("", 8*3600)).Format(time.RFC3339) {
		t.Fatalf("hourly: %+v", r.Items)
	}
	r = get("?group=client")
	if len(r.Items) != 1 || r.Items[0].ClientName == "" || r.Items[0].ClientID != env.adminID {
		t.Fatalf("by client: %+v", r.Items)
	}

	// 参数校验
	for _, qs := range []string{"?group=protocol", "?tz=Asia/Shanghai", "?from=x", "?format=xml", "?client_id=-1",
		"?group=time&interval=hour&from=2026-10-01T00:00:00Z", "?from=2026-01-01T00:00:00Z"} {
		if rec := env.req("GET", "/admin/api/usage/stats"+qs, nil, env.adminTok); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", qs, rec.Code, rec.Body.String())
		}
	}

	// CSV：BOM、表头列顺序、成本按美元
	rec := env.req("GET", "/admin/api/usage/stats?group=model&format=csv", nil, env.adminTok)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(body, "\xEF\xBB\xBF") || !strings.Contains(rec.Header().Get("Content-Disposition"), "usage.csv") {
		t.Fatalf("csv: %d %q", rec.Code, body)
	}
	lines := strings.Split(strings.TrimSpace(strings.TrimPrefix(body, "\xEF\xBB\xBF")), "\n")
	if lines[0] != "model,requests,estimated_requests,cost_usd,tokens,input_tokens,output_tokens,audio_input_tokens,credits" ||
		strings.TrimSpace(lines[1]) != "gpt,2,1,1.50000025,172,150,15,7,0" {
		t.Fatalf("csv lines: %q", lines)
	}
	// 没有登录 → 401
	if rec := env.req("GET", "/admin/api/usage/stats", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
}

func TestFormatUSD(t *testing.T) {
	for nano, want := range map[int64]string{0: "0", 1: "0.000000001", 1_500_000_000: "1.5", 2_000_000_000: "2", -250: "-0.00000025"} {
		if got := formatUSD(nano); got != want {
			t.Errorf("formatUSD(%d) = %q, want %q", nano, got, want)
		}
	}
}
