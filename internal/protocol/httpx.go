package protocol

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// 本文件是出站适配器的通用工具（已实现，spec/08 §3.6）。适配器应优先使用它们，而不是自己处理这些细节。

// MaxErrorBodyBytes 是读取上游错误响应体的上限（RTY-04）。
const MaxErrorBodyBytes = 8 << 10

// ErrResponseTooLarge 表示上游响应体超过 Upstream.MaxBufferedBytes。
var ErrResponseTooLarge = errors.New("protocol: upstream response body too large")

// Send 用 up.Client 发送 req。
//   - 网络错误 → *AttemptError（ClassifyNetError；内核会按客户端是否断开、自身定时器是否触发改写分类）；
//   - 非 2xx → 读取至多 MaxErrorBodyBytes 的错误体后关闭连接，返回 *AttemptError（ClassifyHTTP）；
//   - 2xx → 返回响应，调用方负责关闭 Body。
func Send(up Upstream, req *http.Request) (*http.Response, error) {
	resp, err := up.Client.Do(req)
	if err != nil {
		return nil, &AttemptError{Outcome: ClassifyNetError(err, nil), Err: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxErrorBodyBytes)) // 尽量读完以复用连接
	_ = resp.Body.Close()                                                    // 清理调用，错误无意义
	h := http.Header{}
	CopyHeaders(h, resp.Header, "Content-Type")
	return nil, &AttemptError{Outcome: ClassifyHTTP(resp.StatusCode, resp.Header, time.Now()), Body: body, Header: h}
}

// ReadBody 读取完整响应体（至多 up.MaxBufferedBytes）并关闭。超限返回 *AttemptError{OutcomeLocalError, ErrResponseTooLarge}。
func ReadBody(up Upstream, resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	limit := up.MaxBufferedBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	var body []byte
	var err error
	// OVL-06：装配层可以给响应体附加共享缓冲预算。普通 Doer 与测试替身
	// 仍使用原有读取方式，既有导出接口不变；原样流式读取不申请缓冲预算。
	if br, ok := resp.Body.(interface {
		ReadBuffered(int64, func(func())) ([]byte, error)
	}); ok {
		var retain func(func())
		if owner, ok := up.Client.(interface{ RetainResponseBuffer(func()) }); ok {
			retain = owner.RetainResponseBuffer
		}
		body, err = br.ReadBuffered(limit+1, retain)
	} else {
		body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	}
	if err != nil {
		if apperr.CodeOf(err) == apperr.CodeOverloaded {
			return nil, err // 共享预算耗尽是本机准入失败，不惩罚或重试 Key。
		}
		return nil, &AttemptError{Outcome: ClassifyNetError(err, nil), Err: err}
	}
	if int64(len(body)) > limit {
		return nil, &AttemptError{Outcome: Outcome{Kind: OutcomeLocalError, Status: resp.StatusCode}, Err: ErrResponseTooLarge}
	}
	return body, nil
}

// JoinURL 把 base 与 path 拼成 URL，保证中间恰好一个 "/"（HDR-01）。path 可以带查询串。
func JoinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// CopyHeaders 把 src 中名为 names 的头（全部值）复制到 dst。用于白名单转发（HDR-02、FWD-04）。
func CopyHeaders(dst, src http.Header, names ...string) {
	for _, n := range names {
		for _, v := range src.Values(n) {
			dst.Add(n, v)
		}
	}
}

// NewSSEStream 把 SSE 响应体包装为 EventStream。每个上游事件先交给 inspect：
// forward=false 的事件不转发；err != nil 时 Next 返回该错误（可以是 *AttemptError，见 EventStream.Next）。
// inspect 通常同时更新 Meter（用量、流中错误）。
func NewSSEStream(body io.ReadCloser, inspect func(ev sse.Event) (forward bool, err error)) EventStream {
	return &sseStream{body: body, r: sse.NewReader(body), inspect: inspect}
}

type sseStream struct {
	body    io.ReadCloser
	r       *sse.Reader
	inspect func(sse.Event) (bool, error)
	once    sync.Once
}

func (s *sseStream) Next() (sse.Event, error) {
	for {
		ev, err := s.r.Next()
		if err != nil {
			return sse.Event{}, err
		}
		forward, err := s.inspect(ev)
		if err != nil {
			return sse.Event{}, err
		}
		if forward {
			return ev, nil
		}
	}
}

func (s *sseStream) Close() error {
	var err error
	s.once.Do(func() { err = s.body.Close() })
	return err
}
