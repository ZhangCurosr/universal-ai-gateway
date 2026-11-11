package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// 本文件实现 spec/13 §4（转发）与 §5 中的流空闲超时。

// maxBufferedResponseBytes 是缓冲读取上游响应体的上限（spec/13 §6）。
const maxBufferedResponseBytes = 64 << 20

// rawChunkBytes 是字节流转发的缓冲大小（spec/13 §4.3）。
const rawChunkBytes = 32 << 10

// trackingWriter 记录是否已写出响应头与状态码（spec/13 §4.5），并透传 Flush。
type trackingWriter struct {
	http.ResponseWriter
	status       int
	writeErr     error // 第一次写客户端失败的错误
	writeTimeout time.Duration
}

func (t *trackingWriter) WriteHeader(code int) {
	if t.status == 0 {
		t.status = code
		if err := t.refreshWriteDeadline(); err != nil {
			return
		}
		t.ResponseWriter.WriteHeader(code)
	}
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	if t.status == 0 {
		t.WriteHeader(http.StatusOK)
	}
	if err := t.refreshWriteDeadline(); err != nil {
		return 0, err
	}
	n, err := t.ResponseWriter.Write(p)
	if err != nil && t.writeErr == nil {
		t.writeErr = err
	}
	return n, err
}

func (t *trackingWriter) Flush() {
	if err := t.FlushError(); err != nil && t.writeErr == nil {
		t.writeErr = err
	}
}

// FlushError 保留底层刷写错误；仅取消上游不能中断被慢客户端阻塞的写操作。
func (t *trackingWriter) FlushError() error {
	if err := t.refreshWriteDeadline(); err != nil {
		return err
	}
	err := http.NewResponseController(t.ResponseWriter).Flush()
	if err != nil && t.writeErr == nil {
		t.writeErr = err
	}
	return err
}

func (t *trackingWriter) refreshWriteDeadline() error {
	if t.writeErr != nil {
		return t.writeErr
	}
	if t.writeTimeout > 0 {
		err := http.NewResponseController(t.ResponseWriter).SetWriteDeadline(time.Now().Add(t.writeTimeout))
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			t.writeErr = err
			return err
		}
	}
	return nil
}

func (t *trackingWriter) clearWriteDeadline() {
	err := http.NewResponseController(t.ResponseWriter).SetWriteDeadline(time.Time{})
	if err != nil && !errors.Is(err, http.ErrNotSupported) && t.writeErr == nil {
		t.writeErr = err
	}
}

// Unwrap 让 http.ResponseController 与 http.MaxBytesReader 能找到底层 ResponseWriter。
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func (t *trackingWriter) wroteHeader() bool { return t.status != 0 }

// hopHeaders 是内核在转发时额外删除的头（防御适配器疏漏，spec/13 §4）。
var hopHeaders = []string{"Set-Cookie", "Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Proxy-Connection", "Trailer", "Content-Length"}

// writeResponseHeader 写 resp.Header（去掉逐跳头）、X-Request-Id 与状态码。
func writeResponseHeader(w *trackingWriter, resp *protocol.Response, requestID string, keepLength bool) {
	h := w.Header()
	for k, vs := range resp.Header {
		h[k] = append([]string(nil), vs...)
	}
	for _, k := range hopHeaders {
		if keepLength && k == "Content-Length" {
			continue
		}
		h.Del(k)
	}
	h.Set("X-Request-Id", requestID)
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

// idleTimer 在 d 内没有 reset 时触发 cancel，并记录是自己触发的（STR-01）。
type idleTimer struct {
	t     *time.Timer
	d     time.Duration
	fired atomic.Bool
}

func newIdleTimer(d time.Duration, cancel context.CancelFunc) *idleTimer {
	it := &idleTimer{d: d}
	it.t = time.AfterFunc(d, func() { it.fired.Store(true); cancel() })
	return it
}

func (it *idleTimer) reset() { it.t.Reset(it.d) }
func (it *idleTimer) stop()  { it.t.Stop() }

// streamResult 是一次转发的结果。
type streamResult struct {
	outcome protocol.Outcome
	errCode apperr.Code // 流中通过 StreamError 告知客户端的错误码；没有为空
	abort   bool        // 字节流中途失败：Handler 返回后必须中止连接（spec/13 STR-02）
}

// forwardBuffered 实现 spec/13 §4.1。
func (x *exchange) forwardBuffered(resp *protocol.Response) streamResult {
	writeResponseHeader(x.w, resp, x.requestID, false)
	if _, err := x.w.Write(resp.Body); err != nil {
		return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}}
	}
	x.w.Flush()
	if x.w.writeErr != nil {
		return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}}
	}
	return streamResult{outcome: faultOr(resp, protocol.Outcome{Kind: protocol.OutcomeSuccess})}
}

// forwardEvents 实现 spec/13 §4.2 第 2 ~ 4 步与 §4.4。first 是已经读到的第一个事件（hasFirst 为 false 表示上游流为空）。
func (x *exchange) forwardEvents(resp *protocol.Response, first sse.Event, hasFirst bool, idle *idleTimer) streamResult {
	writeResponseHeader(x.w, resp, x.requestID, false)
	sw := sse.NewWriter(x.w)
	if hasFirst {
		if err := sw.WriteEvent(first); err != nil || x.w.writeErr != nil {
			return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}} // STR-04
		}
	} else {
		x.w.Flush()
		if x.w.writeErr != nil {
			return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}}
		}
	}
	for hasFirst {
		idle.reset()
		ev, err := resp.Events.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return x.streamFailure(sw, err, idle)
		}
		if err := sw.WriteEvent(ev); err != nil || x.w.writeErr != nil {
			return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}} // STR-04
		}
	}
	return streamResult{outcome: faultOr(resp, protocol.Outcome{Kind: protocol.OutcomeSuccess})}
}

// streamFailure 处理事件流中途的错误（STR-01 ~ STR-03）。
func (x *exchange) streamFailure(sw *sse.Writer, err error, idle *idleTimer) streamResult {
	if x.r.Context().Err() != nil { // STR-03
		return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}}
	}
	if idle.fired.Load() { // STR-01
		e := apperr.New(apperr.CodeTimeout, "upstream stream idle timeout")
		_ = sw.WriteEvent(x.in.StreamError(e)) // 客户端可能已断开，写失败无需处理
		return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeTimeout}, errCode: e.Code}
	}
	o := protocol.Outcome{Kind: protocol.OutcomeNetworkError} // STR-02
	var ae *protocol.AttemptError
	if errors.As(err, &ae) {
		o = ae.Outcome
	}
	e := apperr.New(apperr.CodeUpstreamError, "upstream stream interrupted")
	_ = sw.WriteEvent(x.in.StreamError(e)) // 同上
	return streamResult{outcome: o, errCode: e.Code}
}

// forwardRaw 实现 spec/13 §4.3：32 KiB 缓冲循环读写，每块后 Flush 并重置空闲定时器。
func (x *exchange) forwardRaw(resp *protocol.Response, idle *idleTimer) streamResult {
	writeResponseHeader(x.w, resp, x.requestID, true)
	buf := make([]byte, rawChunkBytes)
	for {
		idle.reset()
		n, err := resp.Raw.Read(buf)
		if n > 0 {
			if _, werr := x.w.Write(buf[:n]); werr != nil {
				return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}} // STR-04
			}
			x.w.Flush()
			if x.w.writeErr != nil {
				return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}}
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return streamResult{outcome: faultOr(resp, protocol.Outcome{Kind: protocol.OutcomeSuccess})}
		}
		switch {
		case x.r.Context().Err() != nil: // STR-03
			return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeCanceled}}
		case idle.fired.Load(): // STR-01：字节流中无法插入错误，只能中止连接
			return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeTimeout}, errCode: apperr.CodeTimeout, abort: true}
		default: // STR-02
			return streamResult{outcome: protocol.Outcome{Kind: protocol.OutcomeNetworkError}, errCode: apperr.CodeUpstreamError, abort: true}
		}
	}
}

// faultOr 返回 Meter 报告的流中错误；没有则返回 o。
func faultOr(resp *protocol.Response, o protocol.Outcome) protocol.Outcome {
	if f, ok := resp.Meter.Fault(); ok {
		return f
	}
	return o
}

// closeResponse 释放响应持有的上游连接（可重复调用）。
func closeResponse(resp *protocol.Response) {
	if resp == nil {
		return
	}
	if resp.Events != nil {
		_ = resp.Events.Close() // 清理调用
	}
	if resp.Raw != nil {
		_ = resp.Raw.Close() // 清理调用
	}
}
