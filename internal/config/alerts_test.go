package config

import (
	"strings"
	"testing"
	"time"
)

const alertsYAML = `
server:
  data_dir: "./data"
  master_key_file: "./secrets/master.key"
alerts:
%s
pools:
  - name: tv
    protocol: tavily
    base_url: "https://api.tavily.com"
    quota: { unit: credits, limit: 100, period: day }
    alerts:
%s
  - name: plain
    protocol: tavily
    base_url: "https://api.tavily.com"
models:
  - name: search
    pool: tv
    price: { credits: "0" }
  - name: search2
    pool: plain
    price: { credits: "0" }
`

func alertsCfg(global, pool string) string {
	return strings.Replace(strings.Replace(alertsYAML, "%s", global, 1), "%s", pool, 1)
}

// CFG-22：缺省值与合法声明
func TestAlertsConfigParses(t *testing.T) {
	cfg, err := Parse([]byte(alertsCfg("  webhook: { url_file: ./secrets/hook.url }", "      min_available_keys: 3\n      min_quota_left: 50\n      expiring_within: 72h")))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Alerts
	if a.CheckInterval != time.Minute || a.Repeat != 24*time.Hour || a.Webhook.Format != WebhookGeneric {
		t.Fatalf("alerts = %+v %+v", a, a.Webhook)
	}
	pa := cfg.Pools[0].Alerts
	if pa.MinAvailableKeys != 3 || pa.MinQuotaLeft != 50 || pa.ExpiringWithin != 72*time.Hour || cfg.Pools[1].Alerts != nil {
		t.Fatalf("pool alerts = %+v", pa)
	}
	// 不写 alerts 段：缺省值，不告警
	cfg, err = Parse([]byte(strings.Replace(alertsCfg("  repeat: 1h", "      min_available_keys: 1"), "alerts:\n  repeat: 1h\n", "", 1)))
	if err != nil || cfg.Alerts.Webhook != nil || cfg.Alerts.CheckInterval != time.Minute {
		t.Fatalf("no alerts section: %+v %v", cfg.Alerts, err)
	}
}

func TestAlertsConfigRejects(t *testing.T) {
	ok := "      min_available_keys: 1"
	tests := []struct{ name, global, pool, want string }{
		{"short interval", "  check_interval: 5s", ok, "check_interval"},
		{"short repeat", "  repeat: 30s", ok, "alerts.repeat"},
		{"webhook without file", "  webhook: { format: slack }", ok, "url_file"},
		{"bad format", "  webhook: { url_file: f, format: teams }", ok, "alerts.webhook.format"},
		{"empty pool alerts", "  repeat: 1h", "      expiring_within: 0s", "at least one"},
		{"negative keys", "  repeat: 1h", "      min_available_keys: -1", "min_available_keys"},
		{"short expiry window", "  repeat: 1h", "      expiring_within: 30m", "expiring_within"},
		{"unknown field", "  repeat: 1h", "      min_keys: 1", "min_keys"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(alertsCfg(tt.global, tt.pool)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
	// min_quota_left 需要池声明 quota
	y := strings.Replace(alertsCfg("  repeat: 1h", ok), "  - name: plain\n    protocol: tavily\n    base_url: \"https://api.tavily.com\"\n", "  - name: plain\n    protocol: tavily\n    base_url: \"https://api.tavily.com\"\n    alerts: { min_quota_left: 5 }\n", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "needs pools[1].quota") {
		t.Fatalf("min_quota_left without quota: %v", err)
	}
}
