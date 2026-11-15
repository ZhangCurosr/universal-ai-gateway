package main

import (
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// PRC-06 守护测试（T79）：每个出站适配器声明的单位名合法；表示 token 的单位必须以 _tokens 结尾，
// 否则统计页面不会把它算进 token 合计。新增适配器时，它必须出现在 editableProtocols 中，本测试才会覆盖到它。
func TestAdapterUnitsFollowNaming(t *testing.T) {
	for i, register := range adapterRegistrars {
		reg := protocol.NewRegistry()
		if err := register(reg); err != nil {
			t.Fatalf("registrar %d: %v", i, err)
		}
		found := false
		for _, p := range editableProtocols {
			eg, ok := reg.Egress(p)
			if !ok {
				continue
			}
			found = true
			for _, u := range eg.Units() {
				if !pricing.ValidUnit(u) {
					t.Errorf("%s: invalid unit name %q", p, u)
				}
				if strings.Contains(u, "token") && pricing.UnitKind(u) != pricing.KindTokens {
					t.Errorf("%s: unit %q looks like tokens but does not end with %s (PRC-06)", p, u, pricing.TokenSuffix)
				}
			}
		}
		if !found {
			t.Errorf("registrar %d registers no outbound adapter listed in editableProtocols: add its protocol there", i)
		}
	}
}

// usageUnits 列出当前配置中各池协议的单位（ADM-32）。
func TestUsageUnits(t *testing.T) {
	g := newGatewayForUnits(t)
	got := map[string][]string{}
	for _, u := range g.usageUnits() {
		got[u.Name] = append(got[u.Name], u.Protocols...)
	}
	if len(got["input_tokens"]) == 0 || len(got["credits"]) != 1 || got["credits"][0] != "tavily" {
		t.Fatalf("units: %v", got)
	}
	if (&configReloader{}).usageUnits() != nil {
		t.Fatal("no generation should give no units")
	}
}

func newGatewayForUnits(t *testing.T) *configReloader {
	t.Helper()
	cfg, err := config.Parse([]byte("pools:\n  - { name: ds, protocol: openai_chat, base_url: \"https://api.deepseek.com\" }\n  - { name: tv, protocol: tavily, base_url: \"https://api.tavily.com\" }\nmodels:\n  - { name: deepseek-chat, pool: ds, price: { input_tokens: \"1/1M\", output_tokens: \"1/1M\", cache_read_tokens: \"0\" } }\n  - { name: tavily-search, pool: tv, price: { credits: \"0\" } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := buildGeneration(cfg, memoryGenDeps(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	rl := &configReloader{}
	rl.gen.Store(g)
	return rl
}
