// Package metrics 实现 Prometheus exposition format 0.0.4 纯文本格式的指标收集与导出（spec/15）。
//
// 本包属于第 2 层，只依赖标准库与 internal/store（AuditRecord 类型），无任何第三方依赖。
package metrics

import (
	"time"
)

// KeyGauge 是单个上游 Key 在抓取时的状态快照。
type KeyGauge struct {
	Pool              string
	ID                int64
	Name              string
	Persisted         string // "ready", "quarantined", "disabled"
	InFlight          int
	CooldownRemaining time.Duration
	BreakerOpen       bool
}

// Gauges 是抓取时动态读取的数据源；任何字段为 nil 时跳过对应指标（spec/15 §2.2）。
type Gauges struct {
	Keys         func() []KeyGauge
	LedgerQueue  func() (length, capacity int)
	InFlight     func() (requests, bodyBytes int64)
	AuditDropped func() int64
	Version      string
	StartedAt    time.Time
	Goroutines   func() int // 为 nil 时使用 runtime.NumGoroutine
}
