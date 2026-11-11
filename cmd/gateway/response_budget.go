package main

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// responseBudget 限制所有尝试持有的原始缓冲响应负载；与请求体预算分别计数。
// 预算按实际读取量增长，不为每个请求提前占用整个 64 MiB 上限。
type responseBudget struct {
	limit int64
	used  atomic.Int64
}

func (b *responseBudget) acquire(n int64) bool {
	for {
		used := b.used.Load()
		if n > b.limit-used {
			return false
		}
		if b.used.CompareAndSwap(used, used+n) {
			return true
		}
	}
}

type responseBudgetTransport struct {
	base   http.RoundTripper
	budget *responseBudget
}

func (t *responseBudgetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	resp.Body = &responseBudgetBody{ReadCloser: resp.Body, ctx: r.Context(), budget: t.budget}
	return resp, nil
}

func (t *responseBudgetTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

type responseBudgetBody struct {
	io.ReadCloser
	ctx    context.Context
	budget *responseBudget
}

// ReadBuffered 是 protocol.ReadBody 使用的内部可选能力，不属于冻结的导出接口。
// Body.Close 只归还连接；返回的 []byte 仍被转译器/内核持有，内核通过 retain
// 保留预算到尝试收尾。独立使用客户端时退回 ctx 取消释放；流式 Read 不走此方法。
func (b *responseBudgetBody) ReadBuffered(limit int64, retain func(func())) ([]byte, error) {
	a := &responseAllocation{budget: b.budget}
	if retain != nil {
		retain(a.release)
	} else {
		context.AfterFunc(b.ctx, a.release)
	}
	return io.ReadAll(io.LimitReader(&responseBudgetReader{body: b.ReadCloser, allocation: a}, limit))
}

type responseAllocation struct {
	budget *responseBudget
	mu     sync.Mutex
	used   int64 // guarded by mu
	closed bool  // guarded by mu
}

func (a *responseAllocation) acquire(n int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return context.Canceled
	}
	if !a.budget.acquire(n) {
		return apperr.New(apperr.CodeOverloaded, "buffered response budget exhausted").WithRetryAfter(time.Second)
	}
	a.used += n
	return nil
}

func (a *responseAllocation) giveBack(n int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.used -= n
		a.budget.used.Add(-n)
	}
}

func (a *responseAllocation) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		a.budget.used.Add(-a.used)
	}
}

type responseBudgetReader struct {
	body       io.Reader
	allocation *responseAllocation
}

func (b *responseBudgetReader) Read(p []byte) (int, error) {
	if err := b.allocation.acquire(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := b.body.Read(p)
	b.allocation.giveBack(int64(len(p) - n))
	return n, err
}
