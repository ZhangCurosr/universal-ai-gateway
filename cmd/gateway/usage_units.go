package main

import (
	"github.com/ZhangCurosr/universal-ai-gateway/internal/admin"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// usageUnits 给统计页面列出当前配置中各池协议报告的计量单位（ADM-32，T79）。
// rest 的单位来自当前配置的接口声明（与价目单位一致）。新接入的适配器只要在 Units() 中声明，就会自动出现。
func (r *configReloader) usageUnits() []admin.UnitSource {
	g := r.gen.Load()
	if g == nil {
		return nil
	}
	seen := map[protocol.Protocol]bool{}
	var out []admin.UnitSource
	for _, p := range g.cfg.Pools {
		proto := protocol.Protocol(p.Protocol)
		if seen[proto] {
			continue
		}
		seen[proto] = true
		eg, ok := g.registry.Egress(proto)
		if !ok {
			continue
		}
		for _, u := range eg.Units() {
			out = append(out, admin.UnitSource{Name: u, Protocols: []string{string(proto)}})
		}
	}
	return out
}
