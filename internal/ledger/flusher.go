package ledger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

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
	if log == nil {
		log = slog.Default()
	}
	return &Flusher{st: st, events: events, cfg: cfg, log: log, now: time.Now}
}

// Run 阻塞运行直到 ctx 取消；退出前完成最终刷盘（FLU-01 ~ FLU-04）。
func (f *Flusher) Run(ctx context.Context) error {
	batchSize := f.cfg.BatchSize
	flushInterval := f.cfg.Interval
	maxBuf := f.cfg.QueueCapacity * 4
	initBackoff := f.cfg.InitialBackoff
	maxBackoff := f.cfg.MaxBackoff
	errLogPeriod := f.cfg.ErrorLogPeriod

	buf := make([]store.UsageEvent, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var (
		retrying     bool
		backoff      = initBackoff
		retryTimer   *time.Timer
		retryCh      <-chan time.Time
		firstFailure time.Time
		lastErrorLog time.Time
	)

	// flush 将 buf 写入存储并处理重试/退避状态（FLU-01, FLU-02, FLU-03）。
	flush := func() bool {
		if len(buf) == 0 {
			return true
		}
		err := f.st.InsertUsageEvents(ctx, buf)
		if err == nil {
			buf = buf[:0]
			if retrying {
				retrying = false
				backoff = initBackoff
				firstFailure = time.Time{}
				lastErrorLog = time.Time{}
				if retryTimer != nil {
					retryTimer.Stop()
					retryTimer = nil
					retryCh = nil
				}
			}
			ticker.Reset(flushInterval)
			return true
		}

		if ctx.Err() != nil {
			return false
		}

		now := f.now()
		if !retrying {
			retrying = true
			firstFailure = now
			lastErrorLog = time.Time{}
			backoff = initBackoff
		}

		// FLU-03: 连续失败超过 1 分钟：每分钟一条 Error 日志
		continuous := now.Sub(firstFailure)
		if continuous >= errLogPeriod {
			if lastErrorLog.IsZero() || now.Sub(lastErrorLog) >= errLogPeriod {
				f.log.Error("flush usage events failed continuously",
					"err", err, "events", len(buf), "duration", continuous)
				lastErrorLog = now
			} else {
				f.log.Warn("flush usage events failed, will retry",
					"err", err, "events", len(buf), "backoff", backoff)
			}
		} else {
			// FLU-02: 写入失败保留整批并 Warn 日志
			f.log.Warn("flush usage events failed, will retry",
				"err", err, "events", len(buf), "backoff", backoff)
		}

		if retryTimer != nil {
			retryTimer.Stop()
		}
		retryTimer = time.NewTimer(backoff)
		retryCh = retryTimer.C

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		return false
	}

mainLoop:
	for {
		if ctx.Err() != nil {
			break mainLoop
		}

		// FLU-02: 缓冲达到上限停止从 channel 接收，靠 LED-04 背压
		var eventsCh <-chan store.UsageEvent
		if len(buf) < maxBuf {
			eventsCh = f.events
		}

		select {
		case <-ctx.Done():
			break mainLoop

		case ev, ok := <-eventsCh:
			if !ok {
				eventsCh = nil
				f.events = nil
				continue
			}
			buf = append(buf, ev)
			// FLU-01: 达到 BatchSize 批量写入
			if !retrying && len(buf) >= batchSize {
				flush()
			}

		case <-ticker.C:
			// FLU-01: Interval 到期批量写入
			if !retrying && len(buf) > 0 {
				flush()
			}

		case <-retryCh:
			retryCh = nil
			retryTimer = nil
			flush()
		}
	}

	if retryTimer != nil {
		retryTimer.Stop()
	}

	// FLU-04: ctx 取消后读空 channel 剩余事件（非阻塞读）
	if f.events != nil {
	drainLoop:
		for {
			select {
			case ev, ok := <-f.events:
				if !ok {
					break drainLoop
				}
				buf = append(buf, ev)
			default:
				break drainLoop
			}
		}
	}

	if len(buf) == 0 {
		return nil
	}

	// FLU-04: 使用独立的 5 秒超时 context 尝试最终写入
	shutdownTimeout := f.cfg.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 5 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var lastErr error
	backoff = initBackoff

shutdownLoop:
	for {
		err := f.st.InsertUsageEvents(shutdownCtx, buf)
		if err == nil {
			return nil
		}
		lastErr = err

		if shutdownCtx.Err() != nil {
			break shutdownLoop
		}

		f.log.Warn("final flush usage events failed, will retry",
			"err", err, "events", len(buf), "backoff", backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		case <-shutdownCtx.Done():
			timer.Stop()
			break shutdownLoop
		}
	}

	// FLU-04: 最终写入仍失败以 JSON Lines 追加写到 UnflushedPath 文件
	if f.cfg.UnflushedPath != "" {
		if err := writeJSONLines(f.cfg.UnflushedPath, buf); err != nil {
			f.log.Error("failed to write unflushed events to file",
				"path", f.cfg.UnflushedPath, "events", len(buf), "err", err)
		} else {
			f.log.Error("flush usage events failed on shutdown, written to file",
				"path", f.cfg.UnflushedPath, "events", len(buf), "err", lastErr)
		}
	} else {
		f.log.Error("flush usage events failed on shutdown, no unflushed path configured",
			"events", len(buf), "err", lastErr)
	}
	return nil
}

func writeJSONLines(path string, events []store.UsageEvent) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create unflushed dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open unflushed file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	var buf bytes.Buffer
	for i := range events {
		b, err := json.Marshal(events[i])
		if err != nil {
			return fmt.Errorf("marshal unflushed event: %w", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write unflushed events: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync unflushed file: %w", err)
	}
	closed = true
	if err := f.Close(); err != nil {
		return fmt.Errorf("close unflushed file: %w", err)
	}
	return nil
}

// RecoverUnflushed 把上次关闭时落地的事件文件写回数据库并删除文件（FLU-05）。文件不存在时返回 nil。
func RecoverUnflushed(ctx context.Context, st EventWriter, path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open unflushed file: %w", err)
	}

	var events []store.UsageEvent
	scanner := bufio.NewScanner(f)
	const maxLineSize = 10 * 1024 * 1024
	scanBuf := make([]byte, 64*1024)
	scanner.Buffer(scanBuf, maxLineSize)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev store.UsageEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			_ = f.Close()
			return fmt.Errorf("unmarshal unflushed event: %w", err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		_ = f.Close()
		return fmt.Errorf("scan unflushed file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close unflushed file: %w", err)
	}

	if len(events) > 0 {
		if err := st.InsertUsageEvents(ctx, events); err != nil {
			return fmt.Errorf("insert recovered usage events: %w", err)
		}
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove recovered file: %w", err)
	}
	return nil
}
