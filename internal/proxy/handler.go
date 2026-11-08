// Package proxy 编排数据面的一次模型请求（spec/01 §6 LIFE-07 ~ LIFE-16，任务卡 T10）。
//
// 进入本包之前，httpapi 已完成：RequestID、读取请求头、鉴权（Principal 在 context 中）、RPM 限流。
// 本包禁止访问数据库（ARCH-01）。
package proxy

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/audit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/provider"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
)

// Route 是一个客户端模型名解析后的路由信息。
type Route struct {
	Model    config.ModelConfig
	Pool     config.PoolConfig
	Protocol provider.Protocol
	Price    pricing.Price
}

// Deps 是 Handler 的依赖。全部字段必填（Now 除外）。
type Deps struct {
	Config     *config.Config
	Ledger     ledger.Ledger
	Scheduler  *scheduler.Scheduler
	Adapters   map[provider.Protocol]provider.Adapter
	Audit      *audit.Pipeline
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time // 为 nil 时用 time.Now
}

// Handler 处理模型请求。并发安全。
type Handler struct {
	d      Deps
	routes map[string]Route // 客户端模型名 → 路由，构造后只读
}

// New 创建 Handler，并预先解析全部模型路由。
func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	h := &Handler{d: d, routes: map[string]Route{}}
	for _, m := range d.Config.Models {
		p, ok := d.Config.Pool(m.Pool)
		if !ok {
			continue // config.Validate 已保证存在
		}
		h.routes[m.Name] = Route{
			Model:    m,
			Pool:     *p,
			Protocol: provider.Protocol(p.Protocol),
			Price:    pricing.FromUSDPerMTok(m.Price.Input, m.Price.Output, m.Price.CacheRead, m.Price.CacheWrite),
		}
	}
	return h
}

// Route 返回模型路由（供 httpapi 的 /v1/models 使用）。
func (h *Handler) Route(model string) (Route, bool) {
	r, ok := h.routes[model]
	return r, ok
}

// ForProtocol 返回处理指定入站协议的 http.Handler。
// 调用前请求体尚未读取；Principal 与请求 ID 已在 context 中。
func (h *Handler) ForProtocol(ingress provider.Protocol) http.Handler {
	format := apperr.FormatOpenAI
	if ingress == provider.ProtocolAnthropic {
		format = apperr.FormatAnthropic
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO(T10): 按 LIFE-03、LIFE-07 ~ LIFE-16 实现；收尾动作全部放在一个 defer 中。
		apperr.WriteHTTP(w, format, obs.RequestIDFrom(r.Context()), apperr.NotImplemented("T10"))
	})
}
