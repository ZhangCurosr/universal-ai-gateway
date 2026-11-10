package translate

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// Options 是转译层的可配置行为（spec/10 §1 第 5、6 条）。零值等价于默认配置。
type Options struct {
	DropUnknownFields bool
	Logger            *slog.Logger // nil 时不记日志
}

// droppedFieldsLogged 记录已打过 Warn 告警的 (from, to, field) 组合（进程级单次）。
var droppedFieldsLogged sync.Map

func logDroppedField(opts Options, from, to, field string) {
	if !opts.DropUnknownFields || opts.Logger == nil {
		return
	}
	key := from + "|" + to + "|" + field
	if _, loaded := droppedFieldsLogged.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	opts.Logger.Warn("translation dropped unknown field",
		slog.String("from", from),
		slog.String("to", to),
		slog.String("field", field),
	)
}

func resetDropLog() {
	droppedFieldsLogged.Range(func(key, _ any) bool {
		droppedFieldsLogged.Delete(key)
		return true
	})
}

// clientSide 是一个协议作为「客户端协议」时提供的组件（spec/10 §9.4 REG-01）。
type clientSide struct {
	op      string                                                                           // 接受的 Envelope.Op
	accepts func(env *protocol.Envelope) error                                               // 可选：Op 之外的检查（如 Params）
	lower   func(env *protocol.Envelope, opts Options, from, to string) (*ir.Request, error) // 请求 → IR
	encoder func(now time.Time, newID idGen) encoder                                         // IR → 客户端响应/流
}

// upstreamSide 是一个协议作为「上游协议」时提供的组件（REG-01）。
type upstreamSide struct {
	op      string                                  // Call.Op
	synth   func(req *ir.Request) ([]byte, error)   // IR → 上游请求体
	params  func(req *ir.Request) map[string]string // 可选：Call.Params
	decoder func() decoder                          // 上游响应/流 → IR
}

// clientSides 与 upstreamSides 是各协议登记的组件；每个协议只登记一次。
var clientSides = map[protocol.Protocol]clientSide{
	protoOpenAIChat: {
		op: opChatCompletions,
		lower: func(env *protocol.Envelope, opts Options, from, to string) (*ir.Request, error) {
			return lowerOpenAIChat(env.Body, opts, from, to)
		},
		encoder: func(now time.Time, newID idGen) encoder { return newChatEncoder(now, newID) },
	},
	protoAnthropic: {
		op: opAnthropicCreate,
		lower: func(env *protocol.Envelope, opts Options, from, to string) (*ir.Request, error) {
			return lowerAnthropic(env.Body, opts, from, to)
		},
		encoder: func(_ time.Time, newID idGen) encoder { return newAnthropicEncoder(newID) },
	},
	protoOpenAIResponses: {
		op: opResponsesCreate,
		lower: func(env *protocol.Envelope, opts Options, from, to string) (*ir.Request, error) {
			return lowerOpenAIResponses(env.Body, opts, from, to)
		},
		encoder: func(now time.Time, newID idGen) encoder { return newResponsesEncoder(now, newID) },
	},
}

var upstreamSides = map[protocol.Protocol]upstreamSide{
	protoAnthropic: {
		op:      opAnthropicCreate,
		synth:   func(req *ir.Request) ([]byte, error) { return synthesizeAnthropic(req, nil) },
		decoder: func() decoder { return anthropicDecoder{} },
	},
	protoOpenAIChat: {
		op:      opChatCompletions,
		synth:   synthesizeOpenAIChat,
		decoder: func() decoder { return chatDecoder{} },
	},
}

// pairs 是开放的转译方向：[客户端协议, 上游协议]（REG-02）。不是笛卡尔积，新增方向在这里加行。
var pairs = [][2]protocol.Protocol{
	{protoOpenAIChat, protoAnthropic},
	{protoOpenAIResponses, protoAnthropic},
	{protoAnthropic, protoOpenAIChat},
	{protoOpenAIResponses, protoOpenAIChat},
}

// legacyTranslators 为 T48 之前就存在的 4 个方向保留导出类型（它们只是 composed 的薄包装），
// 使已有调用方与测试中的类型断言保持不变。新方向直接使用 composed。
var legacyTranslators = map[[2]protocol.Protocol]func(Options) protocol.Translator{
	{protoOpenAIChat, protoAnthropic}:       func(o Options) protocol.Translator { return ChatToAnthropic{Opts: o} },
	{protoOpenAIResponses, protoAnthropic}:  func(o Options) protocol.Translator { return ResponsesToAnthropic{Opts: o} },
	{protoAnthropic, protoOpenAIChat}:       func(o Options) protocol.Translator { return AnthropicToChat{Opts: o} },
	{protoOpenAIResponses, protoOpenAIChat}: func(o Options) protocol.Translator { return ResponsesToChat{Opts: o} },
}

// newTranslator 返回 pair 方向的转换器。
func newTranslator(pair [2]protocol.Protocol, opts Options) protocol.Translator {
	if mk, ok := legacyTranslators[pair]; ok {
		return mk(opts)
	}
	return composed{from: pair[0], to: pair[1], opts: opts}
}

// Registrar 返回按 opts 注册组合表中全部转换器的函数；Register 等价于 Registrar(Options{})。
func Registrar(opts Options) func(*protocol.Registry) error {
	return func(r *protocol.Registry) error {
		for _, pair := range pairs {
			if err := r.RegisterTranslator(newTranslator(pair, opts)); err != nil {
				return fmt.Errorf("register translators: %w", err)
			}
		}
		return nil
	}
}

// Register 注册本包实现的全部跨协议转换器（spec/10 §0），使用默认配置。由 cmd/gateway/adapters.go 调用。
func Register(r *protocol.Registry) error {
	return Registrar(Options{})(r)
}

// composed 由 clientSide 与 upstreamSide 组合成一个转换器（REG-01）：
// Request = 接受检查 → Lower → 设置上游模型 → Synth；Response = 上游 Decoder + 客户端 Encoder。
// 无状态、并发安全。
type composed struct {
	from, to protocol.Protocol
	opts     Options
	// now 与 newID 仅供测试注入；为 nil 时使用 time.Now 与 crypto/rand 生成的 ID（REG-03）。
	now   func() time.Time
	newID func(prefix string) string
}

func (c composed) From() protocol.Protocol { return c.from }
func (c composed) To() protocol.Protocol   { return c.to }

// Request 实现 REG-01 的请求方向；不支持的 Op 返回 400（REG-03）。
func (c composed) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	cs, up := clientSides[c.from], upstreamSides[c.to]
	if env.Op != cs.op {
		return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("operation %s is not supported for model %q", env.Op, env.Model))
	}
	if cs.accepts != nil {
		if err := cs.accepts(env); err != nil {
			return nil, err
		}
	}
	req, err := cs.lower(env, c.opts, string(c.from), string(c.to))
	if err != nil {
		return nil, err
	}
	req.Model = upstreamModel
	body, err := up.synth(req)
	if err != nil {
		return nil, err
	}
	call := &protocol.Call{Op: up.op, Model: upstreamModel, Stream: req.Stream, Body: body, Header: env.Header}
	if up.params != nil {
		call.Params = up.params(req)
	}
	return call, nil
}

// Response 实现 REG-01 的响应方向：上游 Decoder + 客户端 Encoder。model 写客户端请求的服务名。
func (c composed) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	now, newID := clockOr(c.now), idsOr(c.newID)
	dec := upstreamSides[c.to].decoder()
	enc := clientSides[c.from].encoder(now, newID)
	switch resp.Kind {
	case protocol.ResponseBuffered:
		return translateBuffered(dec, enc, env, resp)
	case protocol.ResponseEvents:
		return translateEvents(dec, enc, env, resp)
	default:
		return resp, nil // 字节流没有协议结构，原样返回
	}
}

func translateBuffered(dec decoder, enc encoder, env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	irResp, err := dec.Body(resp.Body)
	if err != nil {
		return nil, err
	}
	body, err := enc.Body(irResp, env)
	if err != nil {
		return nil, err
	}
	return &protocol.Response{
		Kind:   protocol.ResponseBuffered,
		Status: resp.Status,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   body,
		Meter:  resp.Meter,
	}, nil
}

func translateEvents(dec decoder, enc encoder, env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	irSt := dec.Stream(resp.Events)
	outSt := enc.Stream(irSt, env)
	return &protocol.Response{
		Kind:   protocol.ResponseEvents,
		Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "Cache-Control": {"no-cache"}},
		Events: outSt,
		Meter:  resp.Meter,
	}, nil
}

// ChatToAnthropic 让 OpenAI Chat 客户端使用 Anthropic 上游（spec/10 §3 ~ §5）。composed 的薄包装；零值可用。
type ChatToAnthropic struct {
	Opts Options
	// Now 与 NewID 仅供测试注入；为 nil 时使用 time.Now 与 crypto/rand 生成的 ID。
	Now   func() time.Time
	NewID func(prefix string) string
}

func (t ChatToAnthropic) c() composed {
	return composed{from: protoOpenAIChat, to: protoAnthropic, opts: t.Opts, now: t.Now, newID: t.NewID}
}

func (t ChatToAnthropic) From() protocol.Protocol { return t.c().From() }
func (t ChatToAnthropic) To() protocol.Protocol   { return t.c().To() }

// Request：Chat 请求 → IR → Anthropic 请求。
func (t ChatToAnthropic) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	return t.c().Request(env, upstreamModel)
}

// Response：Anthropic 响应/流 → Chat 响应/流。
func (t ChatToAnthropic) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	return t.c().Response(env, resp)
}

// ResponsesToAnthropic 让只说 OpenAI Responses 的客户端（Codex CLI 等）使用 Anthropic 上游（spec/10 §8）。
// composed 的薄包装；零值可用。
type ResponsesToAnthropic struct {
	Opts Options
	// Now 与 NewID 仅供测试注入；为 nil 时使用 time.Now 与 crypto/rand 生成的 ID（RX-05）。
	Now   func() time.Time
	NewID func(prefix string) string
}

func (t ResponsesToAnthropic) c() composed {
	return composed{from: protoOpenAIResponses, to: protoAnthropic, opts: t.Opts, now: t.Now, newID: t.NewID}
}

func (t ResponsesToAnthropic) From() protocol.Protocol { return t.c().From() }
func (t ResponsesToAnthropic) To() protocol.Protocol   { return t.c().To() }

// Request：Responses 请求 → IR → Anthropic 请求。
// RX-04：按 ID 的操作在转译路径上不会到达这里（内核 404），到达时按不支持处理。
func (t ResponsesToAnthropic) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	return t.c().Request(env, upstreamModel)
}

// Response：Anthropic 响应/流 → Responses 响应/流。
func (t ResponsesToAnthropic) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	return t.c().Response(env, resp)
}

// AnthropicToChat 让 Anthropic 客户端（Claude Code 等）使用只提供 OpenAI Chat 的上游（spec/10 §6）。
// composed 的薄包装；零值可用。
type AnthropicToChat struct {
	Opts Options
	// NewID 仅供测试注入；为 nil 时使用 crypto/rand 生成的 ID。
	NewID func(prefix string) string
}

func (t AnthropicToChat) c() composed {
	return composed{from: protoAnthropic, to: protoOpenAIChat, opts: t.Opts, newID: t.NewID}
}

func (t AnthropicToChat) From() protocol.Protocol { return t.c().From() }
func (t AnthropicToChat) To() protocol.Protocol   { return t.c().To() }

// Request：Anthropic 请求 → IR → Chat 请求。
func (t AnthropicToChat) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	return t.c().Request(env, upstreamModel)
}

// Response：Chat 响应/流 → Anthropic 响应/流。
func (t AnthropicToChat) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	return t.c().Response(env, resp)
}

// ResponsesToChat 让 Codex CLI 等只说 Responses 的客户端使用只提供 OpenAI Chat 的上游（spec/10 §8）。
// composed 的薄包装；零值可用。
type ResponsesToChat struct {
	Opts Options
	// Now 与 NewID 仅供测试注入；为 nil 时使用 time.Now 与 crypto/rand 生成的 ID（RX-05）。
	Now   func() time.Time
	NewID func(prefix string) string
}

func (t ResponsesToChat) c() composed {
	return composed{from: protoOpenAIResponses, to: protoOpenAIChat, opts: t.Opts, now: t.Now, newID: t.NewID}
}

func (t ResponsesToChat) From() protocol.Protocol { return t.c().From() }
func (t ResponsesToChat) To() protocol.Protocol   { return t.c().To() }

// Request：Responses 请求 → IR → Chat 请求。
func (t ResponsesToChat) Request(env *protocol.Envelope, upstreamModel string) (*protocol.Call, error) {
	return t.c().Request(env, upstreamModel)
}

// Response：Chat 响应/流 → Responses 响应/流。
func (t ResponsesToChat) Response(env *protocol.Envelope, resp *protocol.Response) (*protocol.Response, error) {
	return t.c().Response(env, resp)
}

func clockOr(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

func idsOr(newID func(string) string) idGen {
	if newID != nil {
		return newID
	}
	return newGatewayID
}
