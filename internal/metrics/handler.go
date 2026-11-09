package metrics

import (
	"net/http"
)

// ContentType 是 Prometheus exposition format 0.0.4 的 MIME 类型。
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler 返回导出当前指标的 HTTP 处理程序（spec/15 §1）。
func (c *Collector) Handler(g Gauges) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", ContentType)
		w.WriteHeader(http.StatusOK)
		_ = c.WriteMetrics(w, g)
	})
}
