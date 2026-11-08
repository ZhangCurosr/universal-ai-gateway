# 08 上游适配器与透传（Providers & Passthrough）

代码位置：
- `internal/provider/provider.go`（接口，已提供）、`internal/provider/classify.go`（错误分类，已提供）
- `internal/provider/anthropic/`（T08A）、`internal/provider/openai/`（T08B）
- `internal/sse/`（SSE 读写，已提供）、`internal/jsonx/`（JSON 局部改写，已提供）
- `internal/proxy/`（转发编排，T10）

## 1. 适配器接口

```go
type Protocol string
const (
    ProtocolAnthropic  Protocol = "anthropic"
    ProtocolOpenAIChat Protocol = "openai_chat"
)

type Adapter interface {
    Protocol() Protocol
    // PrepareBody 对透传请求体做最小改写（§3），返回新的请求体与「是否注入了 usage 选项」。
    PrepareBody(body []byte, upstreamModel string, stream bool) (out []byte, injectedUsage bool, err error)
    // NewRequest 构造发往上游的 HTTP 请求（§4）。
    NewRequest(ctx context.Context, baseURL string, secret []byte, body []byte, inbound http.Header) (*http.Request, error)
    // NewUsageTap 返回一个从响应中提取 usage 的观察器（§5）。
    NewUsageTap() UsageTap
    // ClassifyStreamError 把流中的错误事件映射为 Outcome；不是错误事件返回 ok=false（§6）。
    ClassifyStreamError(ev sse.Event) (o Outcome, ok bool)
    // ShouldForward 决定一个上游流事件是否转发给客户端（§6.2）。
    ShouldForward(ev sse.Event, injectedUsage bool) bool
}

type UsageTap interface {
    ObserveEvent(ev sse.Event) // 流式：每个事件调用一次
    ObserveBody(body []byte)   // 非流式：完整响应体调用一次
    Usage() (pricing.Usage, bool) // ok=true 表示上游至少报告了输入 token 数
}
```

适配器**无状态**（`UsageTap` 除外，每个请求一个新实例），可被多个 goroutine 并发使用。

## 2. 端点映射

| 协议 | 入站路径（客户端 → 网关） | 上游 URL（网关 → 上游） |
|:--|:--|:--|
| `anthropic` | `POST /v1/messages` | `<base_url>/v1/messages` |
| `openai_chat` | `POST /v1/chat/completions` | `<base_url>/v1/chat/completions` |

`base_url` 末尾的 `/` 先去掉再拼接。`base_url` 中已含路径（如 `https://example.com/proxy`）时保留该路径前缀。

## 3. 透传改写（`PrepareBody`）

**原则：除下表列出的字段外，请求体其他字节保持原样**（包括 `cache_control`、未知字段、字段内部的空白与顺序）。
只能用 `jsonx` 改写**顶层**字段：`jsonx.SetTopLevel(body, key, rawValue)`。

| 编号 | 协议 | 改写 |
|:--|:--|:--|
| PT-01 | 全部 | `model` 改为 `upstreamModel` |
| PT-02 | 全部 | 请求体不是 JSON 对象 → 返回 `apperr.CodeInvalidRequest` |
| PT-03 | `openai_chat` | `stream == true` 且 `stream_options.include_usage` 不为 `true`：把 `stream_options` 设为原对象并加上 `"include_usage": true`（原对象不存在则为 `{"include_usage":true}`），返回 `injectedUsage = true` |
| PT-04 | `anthropic` | 不做其他改写 |

`jsonx` 的行为（已实现，供参考）：顶层解码为 `map[string]json.RawMessage`，替换目标键，再编码。顶层键会按字母序重排，但每个值内部的字节保持不变。上游的 Prompt Cache 对 `tools`/`system`/`messages` 的内容敏感，对顶层键顺序不敏感，因此这是安全的。

## 4. 构造上游请求（`NewRequest`）

| 编号 | 协议 | 规则 |
|:--|:--|:--|
| HDR-01 | 全部 | `http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))`，设置 `Content-Type: application/json`、`Accept: application/json, text/event-stream` |
| HDR-02 | 全部 | **只复制白名单头部**，客户端的其他头部一律丢弃，尤其是 `Authorization`、`x-api-key`、`Cookie`、`X-Forwarded-*` |
| HDR-03 | `anthropic` | 鉴权头 `x-api-key: <secret>`；复制 `anthropic-version`（缺省时设为 `2023-06-01`）与 `anthropic-beta`（可有多个值，全部复制） |
| HDR-04 | `openai_chat` | 鉴权头 `Authorization: Bearer <secret>`；不复制其他头 |
| HDR-05 | 全部 | 设置 `User-Agent: universal-ai-gateway/<version>` |

## 5. usage 提取（`UsageTap`）

### 5.1 Anthropic

| 来源 | 字段 |
|:--|:--|
| 流式事件 `message_start` | `message.usage.{input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens}` |
| 流式事件 `message_delta` | `usage.{output_tokens, input_tokens, cache_creation_input_tokens, cache_read_input_tokens}`（字段可能只出现一部分） |
| 非流式响应体 | 顶层 `usage.{...}` |

映射：`input_tokens → InputTokens`、`output_tokens → OutputTokens`、`cache_read_input_tokens → CacheReadTokens`、`cache_creation_input_tokens → CacheWriteTokens`。

| 编号 | 规则 |
|:--|:--|
| USE-01 | 后出现的事件中**存在**的字段覆盖之前的值；不存在的字段保留之前的值（`message_delta` 中的数值是累计值，不是增量） |
| USE-02 | 观察到 `input_tokens` 字段后 `Usage()` 返回 `ok = true` |

### 5.2 OpenAI 兼容

| 来源 | 字段 |
|:--|:--|
| 流式最后一个含 `usage` 的 chunk（`data:` JSON 的顶层 `usage` 非 null） | `usage.{prompt_tokens, completion_tokens, prompt_tokens_details.cached_tokens}` |
| 非流式响应体 | 顶层 `usage.{...}` |
| DeepSeek 扩展 | `usage.prompt_cache_hit_tokens`（当 `prompt_tokens_details.cached_tokens` 不存在时使用） |

| 编号 | 规则 |
|:--|:--|
| USE-03 | `cached = prompt_tokens_details.cached_tokens`，不存在时取 `prompt_cache_hit_tokens`，都不存在为 0 |
| USE-04 | `InputTokens = max(prompt_tokens - cached, 0)`、`CacheReadTokens = cached`、`CacheWriteTokens = 0`、`OutputTokens = completion_tokens` |
| USE-05 | 流式中 `data: [DONE]` 不是 JSON，直接忽略 |
| USE-06 | 观察到 `prompt_tokens` 后 `ok = true` |

## 6. 流式转发（`proxy`，T10）

### 6.1 正常流程

1. 上游 2xx 且 `Content-Type` 以 `text/event-stream` 开头 → 流式；否则按非流式处理。
2. 向客户端写响应头：`Content-Type`（流式为 `text/event-stream; charset=utf-8`，非流式为上游值）、`Cache-Control: no-cache`（仅流式）、`X-Request-Id`。**不转发**上游的 `anthropic-ratelimit-*`、`x-ratelimit-*`、`set-cookie` 等头（会泄露上游 Key 信息）。
3. 用 `sse.NewReader(resp.Body)` 逐个读事件；每个事件：`tap.ObserveEvent(ev)` → `adapter.ClassifyStreamError(ev)` 记录 → 若 `adapter.ShouldForward(ev, injectedUsage)` 则 `sse.Writer.WriteEvent(ev)`（会 Flush）。
4. 读到 EOF 正常结束。Outcome = `Success`（若流中出现过错误事件则为该错误的 Outcome）。

### 6.2 转发过滤（`ShouldForward`）

| 编号 | 协议 | 规则 |
|:--|:--|:--|
| FWD-01 | `anthropic` | 全部转发（包括 `ping`、`error`） |
| FWD-02 | `openai_chat` | `injectedUsage == true` 时，**不转发**「`choices` 为空数组且 `usage` 非 null」的 chunk。因为客户端没有要求 usage，很多客户端代码会对空 `choices` 取 `[0]` 而崩溃 |
| FWD-03 | `openai_chat` | 其他事件（含 `[DONE]`）全部转发 |

### 6.3 流中异常

| 编号 | 情况 | 处理 |
|:--|:--|:--|
| STR-01 | 两个事件之间超过 `stream_idle` | 取消上游请求；向客户端写一个错误事件（§6.4）；Outcome = `Timeout` |
| STR-02 | 读上游时出错（连接断开）且不是客户端取消 | 向客户端写错误事件；Outcome = `NetworkError` |
| STR-03 | 客户端断开（`r.Context()` 取消） | 停止读写，关闭上游；Outcome = `Canceled` |
| STR-04 | 写客户端失败 | 同 STR-03 |
| STR-05 | Anthropic 流中出现 `event: error`：`error.type == "overloaded_error"` → `Overloaded`；`rate_limit_error` → `RateLimited`；其他 → `ServerError` | 照常转发该事件（FWD-01） |

### 6.4 网关生成的错误事件

- `anthropic`：`event: error`，`data: {"type":"error","error":{"type":"api_error","message":"<消息>"}}`
- `openai_chat`：`data: {"error":{"message":"<消息>","type":"upstream_error","code":"upstream_error"}}`，之后**不**发送 `[DONE]`

消息文本固定为 `upstream stream interrupted` 或 `upstream stream idle timeout`，不包含上游原始错误。

### 6.5 非流式

- 读取完整响应体，上限 64 MiB（超过则 502 `upstream_error`）；`tap.ObserveBody(body)`；原样写给客户端。
- 非流式的整体超时为池配置 `timeouts.non_stream_total`（默认 10 分钟）。

## 7. 上游响应头超时

流式请求等待上游响应头的超时：

```
headerTimeout = response_header_base + response_header_per_100k_input_tokens × ceil(估算输入 token / 100000)
```

估算输入 token 同 PRC-03（`ceil(bodyBytes/3)`）。实现：`context.WithCancel` + `time.AfterFunc(headerTimeout, cancel)`，收到响应头后 `Stop()` 该定时器，改用 `stream_idle` 定时器（每收到一个事件 `Reset`）。

## 8. 上游 HTTP 客户端（`cmd/gateway` 创建，全局共享）

```go
&http.Transport{
    Proxy:                 http.ProxyFromEnvironment, // 支持 HTTPS_PROXY，国内访问海外 API 常用
    DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
    ForceAttemptHTTP2:     true,
    MaxIdleConns:          1024,
    MaxIdleConnsPerHost:   256,
    IdleConnTimeout:       90 * time.Second,
    TLSHandshakeTimeout:   10 * time.Second,
    ExpectContinueTimeout: 1 * time.Second,
}
```

`http.Client{Transport: t, Timeout: 0}`——**不设** `Client.Timeout`（它会截断长流），超时全部由 §7 的 context 控制。
`connectTimeout` 取所有池 `timeouts.connect` 的最大值。

## 9. 错误分类（`provider/classify.go`，已提供）

```go
func ClassifyHTTP(status int, h http.Header, now time.Time) Outcome
func ClassifyNetError(err error, clientCtx context.Context) Outcome
func ParseRetryAfter(h http.Header, now time.Time) time.Duration
```

规则见代码注释与 07 §5 表格。`ParseRetryAfter` 依次识别：`Retry-After`（秒数或 HTTP-date）、`anthropic-ratelimit-requests-reset` / `anthropic-ratelimit-tokens-reset`（RFC 3339 时间）、`x-ratelimit-reset-requests` / `x-ratelimit-reset-tokens`（Go 时长格式如 `1s`、`6m0s`），取其中**最大**的正值；都没有返回 0。
