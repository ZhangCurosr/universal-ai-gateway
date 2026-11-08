package ledger

import (
	"context"
	"log/slog"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// EventWriter 是 Flusher 需要的最小存储能力。
type EventWriter interface {
	InsertUsageEvents(ctx context.Context, events []store.UsageEvent) error
}

// FlusherConfig 配置刷盘 worker。
type FlusherConfig struct {
	BatchSize       int           // 默认 256
	Interval        time.Duration // 默认 250ms
	QueueCapacity   int           // 与 MemoryConfig.EventQueueCapacity 相同，用于计算待写缓冲上限（FLU-02）
	UnflushedPath   string        // ctx 取消后最终写入失败时的落地文件（FLU-04）
	InitialBackoff  time.Duration // 默认 100ms
	MaxBackoff      time.Duration // 默认 5s
	ErrorLogPeriod  time.Duration // 默认 1 分钟（FLU-03）
	ShutdownTimeout time.Duration // 默认 5s（FLU-04）
}

// Flusher 把计费事件批量写入存储（spec/06 §4）。
type Flusher struct {
	st     EventWriter
	events <-chan store.UsageEvent
	cfg    FlusherConfig
	log    *slog.Logger
	now    func() time.Time
}

// NewFlusher 创建刷盘 worker。
func NewFlusher(st EventWriter, events <-chan store.UsageEvent, cfg FlusherConfig, log *slog.Logger) *Flusher {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 256
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 250 * time.Millisecond
	}
	if cfg.QueueCapacity <= 0 {
		cfg.QueueCapacity = 4096
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 100 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Second
	}
	if cfg.ErrorLogPeriod <= 0 {
		cfg.ErrorLogPeriod = time.Minute
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	return &Flusher{st: st, events: events, cfg: cfg, log: log, now: time.Now}
}

// Run 阻塞运行直到 ctx 取消；退出前完成最终刷盘（FLU-01 ~ FLU-04）。
func (f *Flusher) Run(ctx context.Context) error {
	// TODO(T06)
	<-ctx.Done()
	return nil
}

// RecoverUnflushed 把上次关闭时落地的事件文件写回数据库并删除文件（FLU-05）。文件不存在时返回 nil。
func RecoverUnflushed(ctx context.Context, st EventWriter, path string) error {
	// TODO(T06)
	return apperr.NotImplemented("T06")
}
