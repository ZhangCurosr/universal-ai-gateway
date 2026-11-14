package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
)

var alertNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// alertSetup 构造：池 q（额度 10/天，告警：至少 2 把可用、剩余至少 15、72 小时内过期）与池 plain（至少 1 把可用）。
func alertSetup(t *testing.T) (*scheduler.Scheduler, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		Alerts: config.AlertsConfig{CheckInterval: time.Minute, Repeat: time.Hour},
		Pools: []config.PoolConfig{
			{Name: "q", Quota: &config.QuotaConfig{Unit: "credits", Limit: 10, Period: config.QuotaPeriodTotal, Strategy: config.QuotaDrain},
				Alerts: &config.PoolAlerts{MinAvailableKeys: 2, MinQuotaLeft: 15, ExpiringWithin: 72 * time.Hour}},
			{Name: "plain", Alerts: &config.PoolAlerts{MinAvailableKeys: 1}},
			{Name: "quiet"},
		},
	}
	s := scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{Now: func() time.Time { return alertNow }})
	k1 := scheduler.NewKey(1, "q", "a", []byte("s"), 1, 4, scheduler.PersistedReady)
	k2 := scheduler.NewKey(2, "q", "b", []byte("s"), 1, 4, scheduler.PersistedReady)
	k2.SetExpiresAt(alertNow.Add(24 * time.Hour)) // 72 小时内过期
	k3 := scheduler.NewKey(3, "q", "c", []byte("s"), 1, 4, scheduler.PersistedDisabled)
	k4 := scheduler.NewKey(4, "q", "d", []byte("s"), 1, 4, scheduler.PersistedReady)
	k4.SetExpiresAt(alertNow.Add(-time.Hour)) // 已过期：不算可用，也不算即将过期
	s.SetKeys([]*scheduler.Key{k1, k2, k3, k4})
	return s, cfg
}

// AL-01：可用数、剩余额度、即将过期数；冷却中的 Key 算可用
func TestComputePoolStats(t *testing.T) {
	s, cfg := alertSetup(t)
	s.RecordUsage(1, map[string]int64{"credits": 4})
	l, err := s.Acquire(scheduler.AcquireRequest{Pool: "q"})
	if err != nil {
		t.Fatal(err)
	}
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited, RetryAfter: time.Minute}) // 冷却
	st := computePoolStats(cfg, s.KeyStatus(), alertNow)
	q := st["q"]
	if q.available != 2 || q.quotaLeft != 16 || !q.hasQuota || q.expiring != 1 || !q.hasExpiry {
		t.Fatalf("q = %+v", q)
	}
	if p := st["plain"]; p.available != 0 || p.hasQuota || p.hasExpiry {
		t.Fatalf("plain = %+v", p)
	}
	s.RecordUsage(2, map[string]int64{"credits": 10}) // 用尽：不再算可用
	if q := computePoolStats(cfg, s.KeyStatus(), alertNow)["q"]; q.available != 1 || q.quotaLeft != 6 {
		t.Fatalf("after exhausting key 2: %+v", q)
	}
}

type postRecorder struct {
	mu     sync.Mutex
	bodies []string
	fail   int // 前 fail 次返回错误
}

func (p *postRecorder) post(_ context.Context, _ string, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail > 0 {
		p.fail--
		return errors.New("connection refused to https://hooks.example/secret-token-123")
	}
	p.bodies = append(p.bodies, string(body))
	return nil
}

func (p *postRecorder) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.bodies) == 0 {
		return ""
	}
	return p.bodies[len(p.bodies)-1]
}

func newTestEngine(t *testing.T, s *scheduler.Scheduler, cfg **config.Config, rec *postRecorder, logs *bytes.Buffer) (*alertEngine, *time.Time) {
	t.Helper()
	hook := filepath.Join(t.TempDir(), "hook.url")
	if err := os.WriteFile(hook, []byte(" https://hooks.example/secret-token-123 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	(*cfg).Alerts.Webhook = &config.WebhookConfig{URLFile: hook, Format: config.WebhookGeneric}
	now := alertNow
	e := newAlertEngine(s, func() *config.Config { return *cfg }, slog.New(slog.NewJSONHandler(logs, nil)))
	e.now = func() time.Time { return now }
	e.post = rec.post
	e.sleep = func(time.Duration) {}
	return e, &now
}

// AL-02、AL-03、AL-06：新告警推送一次；持续告警按 repeat 重复；级别变化推送；恢复推送；删除规则时直接移除
func TestAlertEngineTransitions(t *testing.T) {
	s, cfg := alertSetup(t)
	rec := &postRecorder{}
	var logs bytes.Buffer
	e, now := newTestEngine(t, s, &cfg, rec, &logs)
	ctx := context.Background()

	e.evaluate(ctx)
	v := e.View()
	kinds := []string{}
	for _, a := range v.Alerts {
		kinds = append(kinds, a.Pool+"/"+a.Kind+"/"+a.Level)
	}
	// q：可用 2（≥ 2，不告警）、剩余 20（≥ 15）、1 把即将过期；plain：0 把可用（严重）
	if strings.Join(kinds, ",") != "plain/low_keys/critical,q/expiring/warning" || v.CheckedAt == "" {
		t.Fatalf("alerts = %v", kinds)
	}
	if len(rec.bodies) != 1 || !strings.Contains(rec.last(), `"status":"firing"`) || !strings.Contains(rec.last(), "pool plain has 0 available keys (minimum 1)") {
		t.Fatalf("first push = %v", rec.bodies)
	}

	e.evaluate(ctx) // 没有变化、未到 repeat：不推送
	if len(rec.bodies) != 1 {
		t.Fatalf("pushed without a change: %d", len(rec.bodies))
	}
	*now = now.Add(time.Hour) // 到 repeat：重复推送两条持续的告警
	e.evaluate(ctx)
	if len(rec.bodies) != 2 || strings.Count(rec.last(), `"status":"firing"`) != 3 { // 两条 + 顶层
		t.Fatalf("repeat push = %s", rec.last())
	}

	// q 用掉 9 点：剩余 11 < 15 → 新增 low_quota；key 1 用尽 → 可用 1 < 2 → 新增 low_keys（warning）
	s.RecordUsage(1, map[string]int64{"credits": 10})
	s.RecordUsage(2, map[string]int64{"credits": 9})
	e.evaluate(ctx)
	if b := rec.last(); !strings.Contains(b, `"kind":"low_quota"`) || !strings.Contains(b, `"value":1`) || strings.Contains(b, "plain") {
		t.Fatalf("new alerts push = %s", b)
	}
	// key 2 也用尽：q 的 low_keys 升级为 critical
	s.RecordUsage(2, map[string]int64{"credits": 1})
	e.evaluate(ctx)
	if b := rec.last(); !strings.Contains(b, `"level":"critical"`) || !strings.Contains(b, "pool q has 0 available keys") {
		t.Fatalf("level change push = %s", b)
	}

	// plain 加一把 Key：恢复（推送 resolved）
	keys := []*scheduler.Key{scheduler.NewKey(9, "plain", "p", []byte("s"), 1, 4, scheduler.PersistedReady)}
	for _, st := range s.KeyStatus() {
		k := scheduler.NewKey(st.ID, st.Pool, st.Name, []byte("s"), 1, 4, st.Persisted)
		k.SetExpiresAt(st.ExpiresAt)
		keys = append(keys, k)
	}
	s.SetKeys(keys)
	e.evaluate(ctx)
	if b := rec.last(); !strings.Contains(b, `"pool":"plain","kind":"low_keys","level":"critical","status":"resolved"`) {
		t.Fatalf("resolve push = %s", b)
	}
	if !strings.Contains(logs.String(), `"msg":"pool alert resolved"`) {
		t.Fatalf("resolve not logged: %s", logs.String())
	}

	// AL-06：删除 q 的告警配置：直接移除，不推送
	n := len(rec.bodies)
	next := *cfg
	next.Pools = append([]config.PoolConfig(nil), cfg.Pools...)
	next.Pools[0].Alerts = nil
	cfg = &next
	e.evaluate(ctx)
	if len(e.View().Alerts) != 0 || len(rec.bodies) != n {
		t.Fatalf("removed rule: alerts %v, pushes %d -> %d", e.View().Alerts, n, len(rec.bodies))
	}

	// MTR-13
	g := e.PoolGauges()
	if len(g) != 3 || g[0].Pool != "q" || !g[0].HasQuota || g[0].HasExpiry || g[2].Pool != "quiet" || len(g[0].FiringAlerts) != 0 {
		t.Fatalf("gauges = %+v", g)
	}
}

// AL-04：失败重试两次后放弃，地址在日志中打码；地址文件错误只记 Warn
func TestAlertWebhookRetryAndMasking(t *testing.T) {
	s, cfg := alertSetup(t)
	rec := &postRecorder{fail: 3}
	var logs bytes.Buffer
	e, _ := newTestEngine(t, s, &cfg, rec, &logs)
	e.evaluate(context.Background())
	if len(rec.bodies) != 0 || !strings.Contains(logs.String(), "alert webhook failed") || strings.Contains(logs.String(), "secret-token-123") {
		t.Fatalf("after 3 failures: bodies %d logs %s", len(rec.bodies), logs.String())
	}
	if len(e.View().Alerts) != 2 {
		t.Fatal("push failure must not change the alert state")
	}

	rec2 := &postRecorder{fail: 2}
	e2, _ := newTestEngine(t, s, &cfg, rec2, &logs)
	e2.evaluate(context.Background())
	if len(rec2.bodies) != 1 {
		t.Fatalf("third attempt must succeed: %d", len(rec2.bodies))
	}

	logs.Reset()
	cfg.Alerts.Webhook.URLFile = filepath.Join(t.TempDir(), "missing.url")
	e3, _ := newTestEngine(t, s, &cfg, &postRecorder{}, &logs)
	e3.cfg = func() *config.Config {
		c := *cfg
		c.Alerts.Webhook = &config.WebhookConfig{URLFile: "missing.url", Format: "generic"}
		return &c
	}
	e3.evaluate(context.Background())
	if !strings.Contains(logs.String(), "alert webhook not sent") {
		t.Fatalf("missing url file not reported: %s", logs.String())
	}
}

// AL-05：五种消息格式
func TestWebhookBody(t *testing.T) {
	out := []notice{
		{a: alert{pool: "q", kind: alertLowKeys, level: alertCritical, value: 0, threshold: 2, since: alertNow, message: "pool q has 0 available keys (minimum 2)"}},
		{a: alert{pool: "p", kind: alertExpiring, level: alertWarning, value: 3, since: alertNow, message: "3 keys in pool p expire within 72h0m0s"}, resolved: true},
	}
	text := "Universal AI Gateway\n[critical] q: pool q has 0 available keys (minimum 2)\n[resolved] p: 3 keys in pool p expire within 72h0m0s"
	for _, tt := range []struct {
		format string
		check  func(map[string]any) bool
	}{
		{config.WebhookSlack, func(m map[string]any) bool { return m["text"] == text }},
		{config.WebhookFeishu, func(m map[string]any) bool {
			return m["msg_type"] == "text" && m["content"].(map[string]any)["text"] == text
		}},
		{config.WebhookWeCom, func(m map[string]any) bool {
			return m["msgtype"] == "text" && m["text"].(map[string]any)["content"] == text
		}},
		{config.WebhookDingTalk, func(m map[string]any) bool { return m["text"].(map[string]any)["content"] == text }},
		{config.WebhookGeneric, func(m map[string]any) bool {
			a := m["alerts"].([]any)
			return m["status"] == "firing" && len(a) == 2 && a[1].(map[string]any)["status"] == "resolved" && a[0].(map[string]any)["since"] == "2026-11-13T12:00:00Z"
		}},
	} {
		var m map[string]any
		if err := json.Unmarshal(webhookBody(tt.format, out), &m); err != nil || !tt.check(m) {
			t.Errorf("%s: %v %v", tt.format, m, err)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(webhookBody(config.WebhookGeneric, out[1:]), &m); err != nil || m["status"] != "resolved" {
		t.Errorf("only resolved = %v", m)
	}
}

// CFG-22：地址文件的检查；postWebhook 的状态码处理
func TestWebhookURLAndPost(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if u, err := readWebhookURL(write("ok", "\n https://h.example/x?k=1 \n")); err != nil || u != "https://h.example/x?k=1" {
		t.Fatalf("ok = %q %v", u, err)
	}
	for _, s := range []string{"", "ftp://h/x", "not a url", "https://"} {
		if _, err := readWebhookURL(write("bad", s)); err == nil || strings.Contains(err.Error(), s+"x") {
			t.Errorf("%q must be rejected without echoing it: %v", s, err)
		}
	}
	if err := checkWebhookFile(&config.Config{Alerts: config.AlertsConfig{Webhook: &config.WebhookConfig{URLFile: filepath.Join(dir, "none")}}}); err == nil {
		t.Error("missing file must fail at startup")
	}
	if err := checkWebhookFile(&config.Config{}); err != nil {
		t.Error(err)
	}

	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		got = buf.Bytes()
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	if err := postWebhook(context.Background(), srv.URL+"/ok", []byte(`{"a":1}`)); err != nil || string(got) != `{"a":1}` {
		t.Fatalf("post = %v %s", err, got)
	}
	if err := postWebhook(context.Background(), srv.URL+"/fail", nil); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("502 = %v", err)
	}
}

func TestWindowText(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Hour: "1h", 36 * time.Hour: "36h", 72 * time.Hour: "3 days", 2160 * time.Hour: "90 days", 90 * time.Minute: "1h"} {
		if got := windowText(d); got != want {
			t.Errorf("windowText(%s) = %q, want %q", d, got, want)
		}
	}
}
