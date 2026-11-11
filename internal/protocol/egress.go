package protocol

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// Egress 是出站适配器：负责「上游说的协议」的一切细节（spec/08 §3）。
// 实现必须无状态、并发安全（Response 中的 EventStream、Meter 除外，每次尝试一个新实例）。
type Egress interface {
	Protocol() Protocol

	// Units 返回本协议可能报告的全部计量单位。core.New 据此校验模型价目表是否完整（CORE-05）。
	Units() []string

	// Prepare 为同协议透传构造上游调用（每个请求调用一次，在全部尝试之前）。
	// 至少要把服务名改为 upstreamModel（如果本协议的请求里有这个概念）；请求体不合法返回 CodeInvalidRequest。
	Prepare(env *Envelope, upstreamModel string) (*Call, error)

	// Do 执行一次尝试：用 up 中的凭据完成全部上游 HTTP 交互，返回可以转发给客户端的响应。
	// 一次尝试可以包含多次上游调用（例如异步任务的提交 + 轮询），全部通过 up.Client 发送并受 ctx 约束。
	//
	// 在得到可转发的响应之前失败时返回 *AttemptError（内核据此决定是否换 Key 重试）；
	// 返回 *Response 之后的错误通过 EventStream.Next / Raw.Read 的错误与 Meter.Fault 报告。
	// 已产生费用但无法返回完整结果时，可同时返回非 nil Response 与 error：Response.Meter
	// 只用于结算，内核不转发其 Body、不重试（CORE-12）。现有返回签名不变。
	Do(ctx context.Context, up Upstream, call *Call) (*Response, error)
}

// Call 是一次上游调用的描述，由 Egress.Prepare（透传）或 Translator.Request（转译）生成。
// 内核把它原样传给 Egress.Do，不读取除 BodyStream 以外的字段。
type Call struct {
	Op         string            // 上游操作名（出站适配器据此选择上游端点）
	Model      string            // 上游服务名（config models[].upstream_model）
	Stream     bool              // 是否请求上游流式响应
	Body       []byte            // 上游请求体；可以为 nil
	BodyStream io.Reader         // 未缓冲的请求体（只读一次）；非 nil 时内核只尝试一次（CORE-07）
	Params     map[string]string // 参数（通常来自 Envelope.Params）
	Header     http.Header       // 入站请求头（只读）；只能按白名单复制到上游（HDR-02）
	State      any               // 适配器私有状态，内核不读取
}

// Doer 是发送 HTTP 请求的最小接口；*http.Client 实现它。
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Upstream 是内核为一次尝试提供给出站适配器的上游信息。
type Upstream struct {
	BaseURL string // 池的 base_url
	Secret  []byte // 本次选中 Key 的明文凭据；不得写入日志、错误或响应
	Client  Doer   // 共享的上游 HTTP 客户端（spec/13 §6）；适配器不得自建客户端
	// MaxBufferedBytes 是缓冲读取上游响应体的上限；超过时返回 ErrResponseTooLarge。
	MaxBufferedBytes int64
}

// ResponseKind 是响应的转发形态（spec/13 §4）。
type ResponseKind int

const (
	ResponseBuffered ResponseKind = iota // 完整响应体：Response.Body
	ResponseEvents                       // SSE 事件流：Response.Events，内核逐个写为 SSE
	ResponseRaw                          // 原样字节流：Response.Raw，内核边读边写（文件下载、NDJSON 等）
)

// Response 是上游已接受请求的结果；与 error 同时返回时只提供结算信息（CORE-12）。
type Response struct {
	Kind   ResponseKind
	Status int         // 写给客户端的状态码；0 = 200
	Header http.Header // 允许写给客户端的响应头（由适配器按白名单生成，至少含 Content-Type）
	Body   []byte      // ResponseBuffered
	Events EventStream // ResponseEvents
	Raw    io.ReadCloser
	// Meter 报告本次尝试的用量与流中错误；必填。
	Meter Meter
	// Pins 是要固定到本次所用 Key 的线索值（如新建任务的 ID），供之后带 AffinityPin + Strict 的请求使用。
	Pins []string
}

// EventStream 是上游事件流（已按本协议过滤掉不应转发的事件）。非并发安全。
type EventStream interface {
	// Next 返回下一个要转发的事件。正常结束返回 io.EOF。
	// 在第一个事件之前返回 *AttemptError 时，内核仍可换 Key 重试（尚未写出任何字节）。
	Next() (sse.Event, error)
	// Close 释放上游连接。可重复调用。
	Close() error
}

// UsageSource 是用量的来源。
type UsageSource int

const (
	UsageNone      UsageSource = iota // 没有任何用量信息：内核按预留用量结算（spec/06 §3.3）
	UsageUpstream                     // 上游报告的用量
	UsageEstimated                    // 适配器根据部分信息估算的用量
)

// Meter 报告一次尝试的结果信息。内核在转发结束后调用（单 goroutine）。
type Meter interface {
	// Usage 返回用量。适配器可以返回部分上游用量加估算的组合，此时来源为 UsageEstimated。
	Usage() (pricing.Usage, UsageSource)
	// Fault 返回响应过程中出现的上游错误（例如流中的 overloaded 事件）；没有返回 ok=false。
	Fault() (o Outcome, ok bool)
}

// AttemptError 表示一次尝试在得到可转发响应之前失败。
type AttemptError struct {
	Outcome Outcome
	// Body / Header 是上游错误响应（截断到 MaxErrorBodyBytes），仅在同协议透传且结果为
	// OutcomeClientError 时由内核原样转发给客户端（spec/07 §6.1）。可以为空。
	Body   []byte
	Header http.Header
	Err    error // 底层原因（仅用于日志，不得包含凭据）
}

func (e *AttemptError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("upstream attempt failed: %s (status %d): %v", e.Outcome.Kind, e.Outcome.Status, e.Err)
	}
	return fmt.Sprintf("upstream attempt failed: %s (status %d)", e.Outcome.Kind, e.Outcome.Status)
}

func (e *AttemptError) Unwrap() error { return e.Err }

// OutcomeKind 是一次上游尝试的结果分类（spec/07 §5）。
type OutcomeKind int

const (
	OutcomeSuccess          OutcomeKind = iota // 2xx 且正常结束
	OutcomeClientError                         // 400/404/413/422 等：客户端请求问题，不惩罚 Key
	OutcomeCanceled                            // 客户端断开
	OutcomeLocalError                          // 本机资源错误（端口耗尽、响应过大等），不惩罚 Key
	OutcomeRateLimited                         // 429
	OutcomeAuthFailed                          // 401
	OutcomePermissionDenied                    // 403
	OutcomeOverloaded                          // 529 / 503：厂商全局过载
	OutcomeServerError                         // 500/502/504 及其他 5xx
	OutcomeTimeout                             // 等待响应头或流空闲超时
	OutcomeNetworkError                        // 连接失败、连接中断
)

var outcomeNames = [...]string{
	"success", "client_error", "canceled", "local_error", "rate_limited", "auth_failed",
	"permission_denied", "overloaded", "server_error", "timeout", "network_error",
}

func (k OutcomeKind) String() string {
	if int(k) >= 0 && int(k) < len(outcomeNames) {
		return outcomeNames[k]
	}
	return "unknown"
}

// Retryable 报告该结果是否允许在首字节之前换 Key 重试（spec/07 §5）。
func (k OutcomeKind) Retryable() bool {
	switch k {
	case OutcomeRateLimited, OutcomeAuthFailed, OutcomePermissionDenied,
		OutcomeOverloaded, OutcomeServerError, OutcomeTimeout, OutcomeNetworkError:
		return true
	default:
		return false
	}
}

// Outcome 是一次上游尝试的结果。
type Outcome struct {
	Kind       OutcomeKind
	Status     int           // 上游 HTTP 状态码；没有响应时为 0
	RetryAfter time.Duration // 上游建议的等待时长；没有为 0
}
