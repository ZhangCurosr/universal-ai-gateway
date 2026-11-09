package metrics

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

var (
	durationBounds = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}
	ttfbBounds     = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30}
)

type requestKey struct {
	model    string
	ingress  string
	upstream string
	outcome  string
	code     string
}

type errorKey struct {
	model     string
	errorCode string
}

type attemptKey struct {
	model    string
	upstream string
}

type histKey struct {
	model  string
	stream string
}

type histSeries struct {
	mu      sync.Mutex
	bounds  []float64
	buckets []uint64
	count   uint64
	sum     float64
}

func newHistSeries(bounds []float64) *histSeries {
	return &histSeries{
		bounds:  bounds,
		buckets: make([]uint64, len(bounds)),
	}
}

func (h *histSeries) observe(val float64) {
	h.mu.Lock()
	h.count++
	h.sum += val
	for i, b := range h.bounds {
		if val <= b {
			h.buckets[i]++
		}
	}
	h.mu.Unlock()
}

type histSnapshot struct {
	bounds  []float64
	buckets []uint64
	count   uint64
	sum     float64
}

func (h *histSeries) snapshot() histSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := make([]uint64, len(h.buckets))
	copy(b, h.buckets)
	return histSnapshot{
		bounds:  h.bounds,
		buckets: b,
		count:   h.count,
		sum:     h.sum,
	}
}

// Collector 汇总网关的 Prometheus 运行指标。并发安全。
type Collector struct {
	mu sync.RWMutex

	requests map[requestKey]*atomic.Uint64
	errors   map[errorKey]*atomic.Uint64
	attempts map[attemptKey]*atomic.Uint64

	duration map[histKey]*histSeries
	ttfb     map[histKey]*histSeries

	// models 为 RestrictModels 设置的已配置模型名集合；为 nil 时不限制（仅供测试与嵌入方使用）。
	models atomic.Pointer[map[string]struct{}]
}

// OtherModel 是不在已配置模型清单中的请求所使用的 model 标签值（spec/15 MTR-05）。
const OtherModel = "other"

// RestrictModels 限定 model 标签只能取 names 中的值，其余（包括客户端请求了不存在的模型、
// 解析失败时的空值）一律记为 OtherModel，保证标签基数有界且不含用户输入（MTR-05）。
// 必须在开始接收请求之前调用；网关装配时传入配置中的全部模型名。
func (c *Collector) RestrictModels(names []string) {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	c.models.Store(&set)
}

func (c *Collector) modelLabel(model string) string {
	set := c.models.Load()
	if set == nil {
		return model
	}
	if _, ok := (*set)[model]; ok {
		return model
	}
	return OtherModel
}

// NewCollector 创建一个新的指标收集器。
func NewCollector() *Collector {
	return &Collector{
		requests: make(map[requestKey]*atomic.Uint64),
		errors:   make(map[errorKey]*atomic.Uint64),
		attempts: make(map[attemptKey]*atomic.Uint64),
		duration: make(map[histKey]*histSeries),
		ttfb:     make(map[histKey]*histSeries),
	}
}

// ObserveRequest 由 audit.Pipeline 观察者同步调用，记录请求维度的计数器与直方图。
func (c *Collector) ObserveRequest(rec store.AuditRecord) {
	model := c.modelLabel(rec.Model)
	outcome := rec.Outcome
	if outcome == "" {
		outcome = "none"
	}
	code := statusCodeClass(rec.Status)

	// 1. uaig_requests_total
	rk := requestKey{
		model:    model,
		ingress:  rec.IngressProtocol,
		upstream: rec.UpstreamProtocol,
		outcome:  outcome,
		code:     code,
	}
	c.incRequest(rk)

	// 2. uaig_request_errors_total (仅在 ErrorCode 非空时计入)
	if rec.ErrorCode != "" {
		ek := errorKey{
			model:     model,
			errorCode: rec.ErrorCode,
		}
		c.incError(ek)
	}

	// 3. uaig_upstream_attempts_total (按 Attempts 累加)
	if rec.Attempts > 0 {
		ak := attemptKey{
			model:    model,
			upstream: rec.UpstreamProtocol,
		}
		c.addAttempts(ak, uint64(rec.Attempts))
	}

	// 4. uaig_request_duration_seconds
	streamStr := "false"
	if rec.Stream {
		streamStr = "true"
	}
	hk := histKey{
		model:  model,
		stream: streamStr,
	}
	durSec := float64(rec.LatencyMillis) / 1000.0
	if durSec < 0 {
		durSec = 0
	}
	c.observeDuration(hk, durSec)

	// 5. uaig_ttfb_seconds (TTFBMillis < 0 时不计入)
	if rec.TTFBMillis >= 0 {
		ttfbSec := float64(rec.TTFBMillis) / 1000.0
		c.observeTTFB(hk, ttfbSec)
	}
}

func statusCodeClass(status int) string {
	if status >= 100 && status < 600 {
		return fmt.Sprintf("%dxx", status/100)
	}
	return "none"
}

func (c *Collector) incRequest(k requestKey) {
	c.mu.RLock()
	cnt, ok := c.requests[k]
	c.mu.RUnlock()
	if ok {
		cnt.Add(1)
		return
	}
	c.mu.Lock()
	cnt, ok = c.requests[k]
	if !ok {
		cnt = new(atomic.Uint64)
		c.requests[k] = cnt
	}
	c.mu.Unlock()
	cnt.Add(1)
}

func (c *Collector) incError(k errorKey) {
	c.mu.RLock()
	cnt, ok := c.errors[k]
	c.mu.RUnlock()
	if ok {
		cnt.Add(1)
		return
	}
	c.mu.Lock()
	cnt, ok = c.errors[k]
	if !ok {
		cnt = new(atomic.Uint64)
		c.errors[k] = cnt
	}
	c.mu.Unlock()
	cnt.Add(1)
}

func (c *Collector) addAttempts(k attemptKey, n uint64) {
	c.mu.RLock()
	cnt, ok := c.attempts[k]
	c.mu.RUnlock()
	if ok {
		cnt.Add(n)
		return
	}
	c.mu.Lock()
	cnt, ok = c.attempts[k]
	if !ok {
		cnt = new(atomic.Uint64)
		c.attempts[k] = cnt
	}
	c.mu.Unlock()
	cnt.Add(n)
}

func (c *Collector) observeDuration(k histKey, val float64) {
	c.mu.RLock()
	h, ok := c.duration[k]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		h, ok = c.duration[k]
		if !ok {
			h = newHistSeries(durationBounds)
			c.duration[k] = h
		}
		c.mu.Unlock()
	}
	h.observe(val)
}

func (c *Collector) observeTTFB(k histKey, val float64) {
	c.mu.RLock()
	h, ok := c.ttfb[k]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		h, ok = c.ttfb[k]
		if !ok {
			h = newHistSeries(ttfbBounds)
			c.ttfb[k] = h
		}
		c.mu.Unlock()
	}
	h.observe(val)
}
