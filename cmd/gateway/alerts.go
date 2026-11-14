package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/admin"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/metrics"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
)

// 池级告警（spec/07 §7.4，AL-01 ~ AL-06，任务卡 T64）：控制面后台定期读取调度器的内存状态，
// 不访问数据库；状态变化时记日志并可选推送到 Webhook。

// 告警种类与级别（AL-02）。
const (
	alertLowKeys  = "low_keys"
	alertLowQuota = "low_quota"
	alertExpiring = "expiring"

	alertWarning  = "warning"
	alertCritical = "critical"
)

// poolStat 是一个池在某一时刻的容量（AL-01）。
type poolStat struct {
	available int
	quotaLeft int64
	hasQuota  bool
	expiring  int
	hasExpiry bool
}

// computePoolStats 实现 AL-01：available（库中 ready、未过期、额度未用尽、熔断器未打开；短暂冷却算可用）、
// quota_left（可用 Key 的剩余额度之和）、expiring（未过期且在 expiring_within 内过期的 ready Key 数）。
func computePoolStats(cfg *config.Config, keys []scheduler.KeyStatus, now time.Time) map[string]*poolStat {
	out := map[string]*poolStat{}
	window := map[string]time.Duration{}
	for _, p := range cfg.Pools {
		st := &poolStat{hasQuota: p.Quota != nil}
		if p.Alerts != nil && p.Alerts.ExpiringWithin > 0 {
			st.hasExpiry = true
			window[p.Name] = p.Alerts.ExpiringWithin
		}
		out[p.Name] = st
	}
	for _, k := range keys {
		st, ok := out[k.Pool]
		if !ok || k.Persisted != scheduler.PersistedReady || k.Expired {
			continue
		}
		if w := window[k.Pool]; w > 0 && !k.ExpiresAt.IsZero() && k.ExpiresAt.Before(now.Add(w)) {
			st.expiring++
		}
		if k.QuotaExhausted || k.Breaker == scheduler.BreakerOpen {
			continue
		}
		st.available++
		if k.QuotaLimit > 0 {
			st.quotaLeft += max(k.QuotaLimit-k.QuotaUsed, 0)
		}
	}
	return out
}

// alert 是一条正在告警或刚恢复的告警。
type alert struct {
	pool, kind, level string
	value, threshold  int64
	since, lastSent   time.Time
	message           string
}

func (a *alert) key() string { return a.pool + "\x00" + a.kind }

// desiredAlerts 实现 AL-02：按配置与当前容量算出应当处于告警状态的条目；configured 是配置了的规则。
func desiredAlerts(cfg *config.Config, stats map[string]*poolStat) (firing map[string]*alert, configured map[string]bool) {
	firing, configured = map[string]*alert{}, map[string]bool{}
	for _, p := range cfg.Pools {
		pa, st := p.Alerts, stats[p.Name]
		if pa == nil || st == nil {
			continue
		}
		add := func(kind, level string, value, threshold int64, msg string) {
			a := &alert{pool: p.Name, kind: kind, level: level, value: value, threshold: threshold, message: msg}
			firing[a.key()] = a
		}
		if pa.MinAvailableKeys > 0 {
			configured[p.Name+"\x00"+alertLowKeys] = true
			if st.available < pa.MinAvailableKeys {
				level := alertWarning
				if st.available == 0 {
					level = alertCritical
				}
				add(alertLowKeys, level, int64(st.available), int64(pa.MinAvailableKeys),
					fmt.Sprintf("pool %s has %d available keys (minimum %d)", p.Name, st.available, pa.MinAvailableKeys))
			}
		}
		if pa.MinQuotaLeft > 0 && p.Quota != nil {
			configured[p.Name+"\x00"+alertLowQuota] = true
			if st.quotaLeft < pa.MinQuotaLeft {
				add(alertLowQuota, alertWarning, st.quotaLeft, pa.MinQuotaLeft,
					fmt.Sprintf("pool %s has %d %s left this period (minimum %d)", p.Name, st.quotaLeft, p.Quota.Unit, pa.MinQuotaLeft))
			}
		}
		if pa.ExpiringWithin > 0 {
			configured[p.Name+"\x00"+alertExpiring] = true
			if st.expiring > 0 {
				add(alertExpiring, alertWarning, int64(st.expiring), 0,
					fmt.Sprintf("%d keys in pool %s expire within %s", st.expiring, p.Name, windowText(pa.ExpiringWithin)))
			}
		}
	}
	return firing, configured
}

// windowText 把过期窗口写成 72h、90 days 这样的文字（配置要求 >= 1h）。
func windowText(d time.Duration) string {
	if d >= 48*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	return fmt.Sprintf("%dh", int64(d.Hours()))
}

// notice 是一次要推送的变化。
type notice struct {
	a        alert
	resolved bool
}

// alertEngine 是 pool_alerts worker 的状态。
type alertEngine struct {
	sched *scheduler.Scheduler
	cfg   func() *config.Config
	log   *slog.Logger
	now   func() time.Time
	post  func(ctx context.Context, url string, body []byte) error // nil = postWebhook
	sleep func(time.Duration)                                      // 重试间隔；nil = time.Sleep

	mu        sync.Mutex
	active    map[string]*alert
	checkedAt time.Time
}

func newAlertEngine(sched *scheduler.Scheduler, cfg func() *config.Config, log *slog.Logger) *alertEngine {
	return &alertEngine{sched: sched, cfg: cfg, log: log, now: time.Now, active: map[string]*alert{}}
}

// Run 是 worker：每个 check_interval 评估一次（间隔按当前配置，热重载后下一轮生效）。
func (e *alertEngine) Run(ctx context.Context) error {
	for {
		e.evaluate(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(e.cfg().Alerts.CheckInterval):
		}
	}
}

// evaluate 执行一次评估（AL-01 ~ AL-03、AL-06）并推送变化。
func (e *alertEngine) evaluate(ctx context.Context) {
	cfg, now := e.cfg(), e.now()
	firing, configured := desiredAlerts(cfg, computePoolStats(cfg, e.sched.KeyStatus(), now))
	var out []notice
	e.mu.Lock()
	for k, a := range e.active {
		switch {
		case !configured[k]: // AL-06：规则或池已删除，直接移除
			delete(e.active, k)
		case firing[k] == nil: // 已恢复
			delete(e.active, k)
			out = append(out, notice{a: *a, resolved: true})
		}
	}
	for k, want := range firing {
		cur, ok := e.active[k]
		switch {
		case !ok:
			want.since, want.lastSent = now, now
			e.active[k] = want
			out = append(out, notice{a: *want})
		case cur.level != want.level:
			cur.level, cur.value, cur.message, cur.lastSent = want.level, want.value, want.message, now
			out = append(out, notice{a: *cur})
		default:
			cur.value, cur.threshold, cur.message = want.value, want.threshold, want.message
			if now.Sub(cur.lastSent) >= cfg.Alerts.Repeat { // AL-03：持续告警时重复推送
				cur.lastSent = now
				out = append(out, notice{a: *cur})
			}
		}
	}
	e.checkedAt = now
	e.mu.Unlock()
	if len(out) == 0 {
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].a.key() < out[j].a.key() })
	for _, n := range out {
		if n.resolved {
			e.log.Info("pool alert resolved", "pool", n.a.pool, "kind", n.a.kind, "message", n.a.message)
		} else {
			e.log.Warn("pool alert", "pool", n.a.pool, "kind", n.a.kind, "level", n.a.level, "message", n.a.message)
		}
	}
	if w := cfg.Alerts.Webhook; w != nil {
		e.push(ctx, w, out)
	}
}

// push 实现 AL-04：一次评估的变化合并为一条消息；失败时间隔 2 秒、4 秒各重试一次，之后放弃（告警状态不受影响）。
func (e *alertEngine) push(ctx context.Context, w *config.WebhookConfig, out []notice) {
	u, err := readWebhookURL(w.URLFile)
	if err != nil {
		e.log.Warn("alert webhook not sent", "error", err)
		return
	}
	body := webhookBody(w.Format, out)
	post, sleep := e.post, e.sleep
	if post == nil {
		post = postWebhook
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	for attempt, wait := 0, 2*time.Second; ; attempt, wait = attempt+1, wait*2 {
		err = post(ctx, u, body)
		if err == nil {
			return
		}
		if attempt == 2 || ctx.Err() != nil {
			e.log.Warn("alert webhook failed", "url", obs.MaskSecret(u), "error", strings.ReplaceAll(err.Error(), u, obs.MaskSecret(u)))
			return
		}
		sleep(wait)
	}
}

// View 实现 ADM-26。
func (e *alertEngine) View() admin.AlertsView {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := admin.AlertsView{Alerts: make([]admin.AlertView, 0, len(e.active))}
	for _, a := range e.active {
		v.Alerts = append(v.Alerts, admin.AlertView{Pool: a.pool, Kind: a.kind, Level: a.level, Value: a.value,
			Threshold: a.threshold, Since: a.since.UTC().Format(time.RFC3339), Message: a.message})
	}
	sort.Slice(v.Alerts, func(i, j int) bool {
		if v.Alerts[i].Pool != v.Alerts[j].Pool {
			return v.Alerts[i].Pool < v.Alerts[j].Pool
		}
		return v.Alerts[i].Kind < v.Alerts[j].Kind
	})
	if !e.checkedAt.IsZero() {
		v.CheckedAt = e.checkedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// PoolGauges 为 /admin/metrics 提供池级容量与正在告警的种类（MTR-13）。
func (e *alertEngine) PoolGauges() []metrics.PoolGauge {
	cfg := e.cfg()
	stats := computePoolStats(cfg, e.sched.KeyStatus(), e.now())
	e.mu.Lock()
	firing := map[string][]string{}
	for _, a := range e.active {
		firing[a.pool] = append(firing[a.pool], a.kind)
	}
	e.mu.Unlock()
	out := make([]metrics.PoolGauge, 0, len(cfg.Pools))
	for _, p := range cfg.Pools {
		st := stats[p.Name]
		kinds := firing[p.Name]
		sort.Strings(kinds)
		out = append(out, metrics.PoolGauge{Pool: p.Name, Available: st.available, HasQuota: st.hasQuota, QuotaLeft: st.quotaLeft,
			HasExpiry: st.hasExpiry, Expiring: st.expiring, FiringAlerts: kinds})
	}
	return out
}

// readWebhookURL 读取并检查 Webhook 地址文件（CFG-22）。错误信息不含文件内容。
func readWebhookURL(path string) (string, error) {
	raw, err := readLimited(path, 4<<10)
	if err != nil {
		return "", fmt.Errorf("alerts.webhook.url_file: %w", err)
	}
	s := strings.TrimSpace(string(raw))
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("alerts.webhook.url_file: the file must contain one http(s) URL")
	}
	return s, nil
}

// checkWebhookFile 在启动时检查地址文件（CFG-22：读不到则启动失败）。
func checkWebhookFile(cfg *config.Config) error {
	if w := cfg.Alerts.Webhook; w != nil {
		if _, err := readWebhookURL(w.URLFile); err != nil {
			return err
		}
	}
	return nil
}

// webhookBody 实现 AL-05。
func webhookBody(format string, out []notice) []byte {
	var text strings.Builder
	text.WriteString("Universal AI Gateway")
	status := "resolved"
	type item struct {
		Pool      string `json:"pool"`
		Kind      string `json:"kind"`
		Level     string `json:"level"`
		Status    string `json:"status"`
		Value     int64  `json:"value"`
		Threshold int64  `json:"threshold"`
		Since     string `json:"since"`
		Message   string `json:"message"`
	}
	items := make([]item, 0, len(out))
	for _, n := range out {
		st, tag := "firing", n.a.level
		if n.resolved {
			st, tag = "resolved", "resolved"
		} else {
			status = "firing"
		}
		fmt.Fprintf(&text, "\n[%s] %s: %s", tag, n.a.pool, n.a.message)
		items = append(items, item{Pool: n.a.pool, Kind: n.a.kind, Level: n.a.level, Status: st, Value: n.a.value,
			Threshold: n.a.threshold, Since: n.a.since.UTC().Format(time.RFC3339), Message: n.a.message})
	}
	var v any
	switch format {
	case config.WebhookSlack:
		v = map[string]any{"text": text.String()}
	case config.WebhookFeishu:
		v = map[string]any{"msg_type": "text", "content": map[string]any{"text": text.String()}}
	case config.WebhookWeCom, config.WebhookDingTalk:
		v = map[string]any{"msgtype": "text", "text": map[string]any{"content": text.String()}}
	default:
		v = map[string]any{"status": status, "alerts": items}
	}
	b, _ := json.Marshal(v) // 只含字符串、整数与切片，必然可以序列化
	return b
}

// webhookClient 只用于告警推送：10 秒超时（AL-04）。
var webhookClient = &http.Client{Timeout: 10 * time.Second}

func postWebhook(ctx context.Context, u string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := webhookClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // 不读响应体，关闭失败无可处理
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
