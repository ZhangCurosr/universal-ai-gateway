// Package audit 实现审计日志的异步流水线（任务卡 T13，AUD-01 ~ AUD-05）。
// 审计记录可以丢弃；计费事件不在本包处理（见 ledger）。
package audit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

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
	// TODO(T13): select { case p.ch <- rec: default: p.dropped.Add(1) }
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
	return &Writer{p: p, st: st, cfg: cfg, log: log}
}

// Run 阻塞运行直到 ctx 取消，退出前做最后一次写入。
func (w *Writer) Run(ctx context.Context) error {
	// TODO(T13)
	<-ctx.Done()
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
	// TODO(T13)
	<-ctx.Done()
	return nil
}
