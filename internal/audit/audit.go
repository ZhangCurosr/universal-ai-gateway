// Package audit 实现审计日志的异步流水线（任务卡 T13，AUD-01 ~ AUD-05）。
// 审计记录可以丢弃；计费事件不在本包处理（见 ledger）。
package audit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// RecordWriter 是 Writer 需要的最小存储能力。
type RecordWriter interface {
	InsertAuditRecords(ctx context.Context, recs []store.AuditRecord) error
}

// RecordDeleter 是 Retention 需要的最小存储能力。
type RecordDeleter interface {
	DeleteAuditBefore(ctx context.Context, before time.Time) (int64, error)
}

// Pipeline 是数据面提交审计记录的入口。
type Pipeline struct {
	ch      chan store.AuditRecord
	dropped atomic.Int64
}

// NewPipeline 创建容量为 capacity 的流水线。
func NewPipeline(capacity int) *Pipeline {
	if capacity <= 0 {
		capacity = 8192
	}
	return &Pipeline{ch: make(chan store.AuditRecord, capacity)}
}

// Submit 非阻塞提交；队列满时丢弃并计数（AUD-01）。
func (p *Pipeline) Submit(rec store.AuditRecord) {
	// AUD-01: 非阻塞提交，队列满时丢弃并计数
	select {
	case p.ch <- rec:
	default:
		p.dropped.Add(1)
	}
}

// Dropped 返回累计丢弃数（AUD-05）。
func (p *Pipeline) Dropped() int64 { return p.dropped.Load() }

// WriterConfig 配置批量写入。
type WriterConfig struct {
	BatchSize     int
	FlushInterval time.Duration
}

// Writer 从 Pipeline 读取记录并批量写入存储（AUD-02、AUD-03、AUD-05）。
type Writer struct {
	p   *Pipeline
	st  RecordWriter
	cfg WriterConfig
	log *slog.Logger
}

// NewWriter 创建写入 worker。
func NewWriter(p *Pipeline, st RecordWriter, cfg WriterConfig, log *slog.Logger) *Writer {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 256
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 250 * time.Millisecond
	}
	if log == nil {
		log = obs.Discard()
	}
	return &Writer{p: p, st: st, cfg: cfg, log: log}
}

// Run 阻塞运行直到 ctx 取消，退出前做最后一次写入。
func (w *Writer) Run(ctx context.Context) error {
	if w.p == nil {
		return nil
	}
	batchSize := w.cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 256
	}
	flushInterval := w.cfg.FlushInterval
	if flushInterval <= 0 {
		flushInterval = 250 * time.Millisecond
	}
	logger := w.log
	if logger == nil {
		logger = obs.Discard()
	}

	buf := make([]store.AuditRecord, 0, batchSize)
	var lastDropped int64

	// AUD-05: 累计丢弃数监控，每次 flush 时若自上次以来有新增丢弃，输出一条 Warn
	checkDropped := func() {
		curr := w.p.Dropped()
		if diff := curr - lastDropped; diff > 0 {
			logger.Warn("audit records dropped", "dropped", diff, "total_dropped", curr)
			lastDropped = curr
		}
	}

	// AUD-02: 写入失败丢弃该批并 Warn（审计可以丢，不重试）
	flush := func(flushCtx context.Context) {
		checkDropped()
		if len(buf) == 0 {
			return
		}
		if w.st != nil {
			if err := w.st.InsertAuditRecords(flushCtx, buf); err != nil {
				logger.Warn("failed to insert audit records", "err", err, "count", len(buf))
			}
		}
		buf = buf[:0]
	}

	// 主循环中的写入：不随 ctx 取消而中断（否则正在写的一批会被丢弃），但设上限，
	// 防止 SQLite 卡住时 Run 永远阻塞（AGENTS §3 #6）。
	flushBounded := func() {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		flush(fctx)
	}

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

mainLoop:
	for {
		if ctx.Err() != nil {
			break mainLoop
		}
		select {
		case <-ctx.Done():
			break mainLoop
		case rec, ok := <-w.p.ch:
			if !ok {
				break mainLoop
			}
			buf = append(buf, rec)
			if ctx.Err() != nil {
				break mainLoop
			}
			if len(buf) >= batchSize {
				flushBounded()
				ticker.Reset(flushInterval)
			}
		case <-ticker.C:
			flushBounded()
		}
	}

	// AUD-03: ctx 取消后读空队列做最后一次写入（5 秒超时），然后返回。
	// 最多读取一个队列容量的记录：关停期间生产者仍可能持续 Submit，不设上限 Run 可能永不返回。
	maxDrain := cap(w.p.ch)
drainLoop:
	for drained := 0; drained < maxDrain; drained++ {
		select {
		case rec, ok := <-w.p.ch:
			if !ok {
				break drainLoop
			}
			buf = append(buf, rec)
		default:
			break drainLoop
		}
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flush(drainCtx)

	return nil
}

// Retention 定期删除过期审计记录（AUD-04）。
type Retention struct {
	St            RecordDeleter
	RetentionDays int
	Interval      time.Duration // 默认 1 小时
	Now           func() time.Time
	Log           *slog.Logger
}

// Run 启动时立即清理一次，之后每 Interval 清理一次，直到 ctx 取消。
func (r *Retention) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	interval := r.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	nowFunc := r.Now
	if nowFunc == nil {
		nowFunc = time.Now
	}
	logger := r.Log
	if logger == nil {
		logger = obs.Discard()
	}
	retentionDays := r.RetentionDays
	if retentionDays <= 0 {
		// spec/03 §4 CFG-14：retention_days 在配置校验中保证 >= 1；此处兜底避免误删全部审计日志
		retentionDays = 30
	}

	clean := func() {
		if r.St == nil {
			return
		}
		before := nowFunc().AddDate(0, 0, -retentionDays)
		if _, err := r.St.DeleteAuditBefore(ctx, before); err != nil {
			if ctx.Err() == nil {
				logger.Warn("failed to delete expired audit records", "err", err, "before", before)
			}
		}
	}

	// AUD-04: 启动时立即执行一次
	clean()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			clean()
		}
	}
}
