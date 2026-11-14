package config

import (
	"strings"
	"testing"
	"time"
)

// quotaYAML 是带额度的配置；%s 处插入 quota 块（缩进 4 格）。
const quotaYAML = `
server:
  data_dir: "./data"
  master_key_file: "./secrets/master.key"
pools:
  - name: tv
    protocol: tavily
    base_url: "https://api.tavily.com"
    quota:
%s
  - name: rp
    protocol: rest
    base_url: "https://mineru.net"
models:
  - name: search
    pool: tv
    price: { credits: "0.008" }
  - name: mu
    pool: rp
    price: { requests: "0", pages: "0" }
    api:
      endpoints:
        - match: "POST /task"
          usage: { pages: { request_query: pages, default: 1 } }
`

func quotaCfg(q string) string { return strings.Replace(quotaYAML, "%s", q, 1) }

// CFG-21：合法的声明与缺省值
func TestQuotaConfigParses(t *testing.T) {
	cfg, err := Parse([]byte(quotaCfg("      unit: credits\n      limit: 1000\n      period: month\n      reset: { day: 31, time: \"08:30\", tz: \"+08:00\" }")))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	q := cfg.Pools[0].Quota
	if q == nil || q.Limit != 1000 || q.Strategy != QuotaDrain {
		t.Fatalf("quota = %+v", q)
	}
	s, err := q.Schedule()
	if err != nil || s.Period != QuotaPeriodMonth || s.Day != 31 || s.Hour != 8 || s.Minute != 30 {
		t.Fatalf("schedule = %+v %v", s, err)
	}
	if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, s.Loc).Zone(); off != 8*3600 {
		t.Errorf("tz offset = %d", off)
	}
	if cfg.Pools[1].Quota != nil {
		t.Error("pool without quota must stay nil")
	}

	// 第二个池（rest）、周与总量周期、缺省时刻
	y := strings.Replace(quotaYAML, "%s", "      unit: credits\n      limit: 5\n      period: total\n      strategy: spread", 1)
	y = strings.Replace(y, "    base_url: \"https://mineru.net\"\n", "    base_url: \"https://mineru.net\"\n    quota: { unit: pages, limit: 10, period: week, reset: { weekday: Mon } }\n", 1)
	cfg, err = Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if s, _ := cfg.Pools[0].Quota.Schedule(); s.Period != QuotaPeriodTotal || cfg.Pools[0].Quota.Strategy != QuotaSpread {
		t.Errorf("total quota = %+v", cfg.Pools[0].Quota)
	}
	s, err = cfg.Pools[1].Quota.Schedule()
	if err != nil || s.Weekday != time.Monday || s.Hour != 0 || s.Loc != time.UTC {
		t.Errorf("week schedule = %+v %v", s, err)
	}
}

func TestQuotaConfigRejects(t *testing.T) {
	tests := []struct{ name, q, want string }{
		{"unit not metered", "      unit: pages\n      limit: 1\n      period: day", "not priced or metered"},
		{"bad unit name", "      unit: Credits\n      limit: 1\n      period: day", "must match"},
		{"zero limit", "      unit: credits\n      limit: 0\n      period: day", "limit"},
		{"bad period", "      unit: credits\n      limit: 1\n      period: year", "period must be"},
		{"bad strategy", "      unit: credits\n      limit: 1\n      period: day\n      strategy: random", "drain or spread"},
		{"total with reset", "      unit: credits\n      limit: 1\n      period: total\n      reset: { time: \"00:00\" }", "not allowed"},
		{"week without weekday", "      unit: credits\n      limit: 1\n      period: week", "weekday"},
		{"week with day", "      unit: credits\n      limit: 1\n      period: week\n      reset: { weekday: tue, day: 3 }", "only for period month"},
		{"month without day", "      unit: credits\n      limit: 1\n      period: month", "reset.day"},
		{"month day 32", "      unit: credits\n      limit: 1\n      period: month\n      reset: { day: 32 }", "reset.day"},
		{"month with weekday", "      unit: credits\n      limit: 1\n      period: month\n      reset: { day: 1, weekday: mon }", "only for period week"},
		{"day with weekday", "      unit: credits\n      limit: 1\n      period: day\n      reset: { weekday: mon }", "only takes"},
		{"bad time", "      unit: credits\n      limit: 1\n      period: day\n      reset: { time: \"25:00\" }", "HH:MM"},
		{"bad tz", "      unit: credits\n      limit: 1\n      period: day\n      reset: { tz: \"Asia/Shanghai\" }", "time zone"},
		{"unknown field", "      unit: credits\n      limit: 1\n      period: day\n      cap: 3", "cap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(quotaCfg(tt.q)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
