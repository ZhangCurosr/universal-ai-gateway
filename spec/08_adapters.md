# 08 协议适配器（入站 / 出站）与扩展指南

> 本章规定**适配器**（会变、会增加的部分）。**内核**（稳定的部分）见 [13_core.md](./13_core.md)。
> 契约代码：`internal/protocol/`（冻结接口）。参考实现位置：`internal/adapter/anthropic/`、`internal/adapter/openai/`。

## 1. 总览

```
客户端 ──▶ [入站适配器 Ingress]  ──Envelope──▶  [内核 core]  ──Call──▶  [出站适配器 Egress] ──▶ 上游（可多次调用）
                ▲  错误格式 / 目录 / 流错误事件        │                         │
                └──────────────────────────────────────┘◀───────Response────────┘
                     入站协议 ≠ 上游协议时，内核改用 [Translator] 生成 Call、转换 Response（spec/10）
```

「协议」是任意一种 HTTP API 的形状，**不限于大模型**。契约对上游没有以下假设：

| 不假设 | 契约中对应的泛化 |
|:--|:--|
| 只有一个端点 | 入站协议可声明任意多个调用端点，每个是一个操作 `Route.Op`（`search`、`extract`、`task.create`……） |
| 请求体是 JSON | 请求体可以缓冲（`BodyBuffered`）、流式透传（`BodyStream`，大文件上传）或没有（`BodyNone`，GET 查询） |
| 请求里有 `model` | `Envelope.Model` 是「服务名」，由入站适配器决定从哪里来（请求体、请求头、或固定值，见 IN-G03） |
| 按 token 计费 | 用量是任意计量单位的集合 `pricing.Usage`（`input_tokens`、`requests`、`credits`、`pages`……） |
| 一次尝试 = 一次上游调用 | `Egress.Do` 封装一次尝试中的全部上游交互（如异步任务的提交 + 轮询） |
| 响应是 JSON 或 SSE | `Response.Kind`：缓冲体、SSE 事件流、原样字节流（文件下载、NDJSON 等） |
| 请求之间无关联 | 异步任务用 `Response.Pins` + `AffinityHint{Strict}` 把任务 ID 固定到创建它的上游 Key |

| 组件 | 职责 | 接口 | 每个协议一个实现 |
|:--|:--|:--|:--|
| Ingress | 客户端说的协议：路由、解析、错误格式、流错误事件、目录 | `protocol.Ingress` | `internal/adapter/<name>/ingress.go` |
| Egress | 上游说的协议：透传改写、执行尝试、提取用量、过滤与分类事件 | `protocol.Egress` | `internal/adapter/<name>/egress.go` |
| Translator | 跨协议转换 | `protocol.Translator` | `internal/translate/` |
| Registry | 启动时登记全部适配器，供内核与路由查找 | `protocol.Registry` | 只有一个，`cmd/gateway/adapters.go` 装配 |

**硬性约束**：
- 一个协议的入站与出站适配器放在**同一个包**中，包名即协议简称；导出 `Protocol` 常量、操作名常量（`Op*`）与 `Register(*protocol.Registry) error` 函数。
- 适配器包之间互不导入（ARCH-03）；适配器不导入内核、httpapi、admin（ARCH-04）。
- 只有 `cmd/gateway/adapters.go` 导入具体适配器包（ARCH-07，由 archtest 检查）。
- 适配器必须无状态、并发安全；每次尝试的状态（`EventStream`、`Meter`）在 `Do` 中新建。
- 只做入站或只做出站的协议可以只注册一侧。

## 2. 入站适配器（Ingress）

```go
type Ingress interface {
    Protocol() Protocol
    Routes() []Route                       // ≥1 个 RouteCall（Op 唯一），≤1 个 RouteCatalog
    Claims(r *http.Request) bool           // 仅路由冲突时调用
    Parse(r *http.Request, rt Route, body []byte) (*Envelope, error)
    WriteError(w http.ResponseWriter, requestID string, err *apperr.Error)
    StreamError(err *apperr.Error) sse.Event
    WriteCatalog(w http.ResponseWriter, models []ModelInfo)
}

type Route struct {
    Method, Path string   // Path 用 http.ServeMux 模式语法，可含 {id}
    Kind         RouteKind // RouteCall | RouteCatalog
    Op           string    // RouteCall 必填
    Body         BodyMode  // BodyBuffered（默认）| BodyStream | BodyNone
    MaxBodyBytes int64     // 0 = 用全局上限；最终取较小者（CORE-06）
}
```

### 2.1 通用规则（所有协议，一致性套件检查）

| 编号 | 规则 |
|:--|:--|
| IN-G01 | `Envelope.Protocol = Protocol()`、`Envelope.Op = rt.Op`、`Envelope.Header = r.Header` |
| IN-G02 | `rt.Body == BodyBuffered` 时 `Envelope.Body` 为传入的 `body` 原切片（不复制、不修改）；其他模式 `Body` 为 nil，且**不得**读取 `r.Body`（内核在 Parse 之后把它包装为 `Envelope.BodyStream`） |
| IN-G03 | `Envelope.Model` 是服务名。请求里有服务名字段的协议从字段取；没有的协议取适配器常量（如 `tavily-search`），并允许请求头 `X-UAIG-Model` 覆盖。**只有**严格亲和的请求（IN-G07）可以留空：内核按固定记录所在的池确定路由（CORE-11），用于 `GET /v1/responses/{id}` 这类不带服务名的后续操作。其他情况取不到 → `CodeInvalidRequest` |
| IN-G04 | `Envelope.Estimate` 是本请求用量的**保守上界**（用于预留）；能从请求推出的单位才填（如 `max_tokens` → `output_tokens`、`search_depth=advanced` → `credits: 2`），推不出的留空，由模型配置 `reserve` 补齐（spec/06 PRC-03）。**确定不会产生**某单位用量的操作（如查询任务状态不产生 `pages`）把该单位**显式写为 0**，否则 `reserve` 的默认值会被用于预留。单位名必须满足 `pricing.ValidUnit` |
| IN-G05 | 路径参数用 `r.PathValue(name)` 读取；适配器需要传给出站侧的参数（任务 ID、查询串、上传的 Content-Type……）放入 `Envelope.Params`，键名由本包自行约定 |
| IN-G06 | 请求不合法 → `CodeInvalidRequest`，消息说明哪个字段有问题。JSON 协议只读取顶层少数字段，用 `jsonx.Get*`；**禁止**把请求体整体反序列化为结构体 |
| IN-G07 | 查询异步任务的端点：`Affinity = AffinityHint{Source: AffinityPin, Value: 任务 ID, Strict: true}`（spec/07 §7.1） |

### 2.2 路由（各协议）

| 编号 | 协议 | 规则 |
|:--|:--|:--|
| IN-A01 | anthropic | `POST /v1/messages`（Call，Op `messages`）、`GET /v1/models`（Catalog）（已实现） |
| IN-O01 | openai_chat | `POST /v1/chat/completions`（Call，Op `chat.completions`）、`GET /v1/models`（Catalog）（已实现） |
| IN-R01 | openai_responses | 见 §9（已实现） |
| IN-A02 | anthropic | `Claims`：请求带 `anthropic-version` 头时返回 true（已实现） |
| IN-O02 | openai_chat | `Claims` 永远返回 false；OpenAI 注册为兜底协议（`Fallback: true`），冲突路由无人认领时归它（已实现） |

### 2.3 解析（大模型协议的字段）

| 编号 | 规则 |
|:--|:--|
| IN-L01 | 请求体不是 JSON 对象（含解析失败）→ `CodeInvalidRequest`，消息 `request body must be a JSON object` |
| IN-L02 | `model` 必须是非空字符串，否则 `CodeInvalidRequest`，消息 `model: required` |
| IN-L03 | `stream` 缺省为 false；不是布尔值 → `CodeInvalidRequest` |
| IN-L04 | `Estimate[input_tokens] = pricing.EstimateInputTokens(len(body))`；输出上限字段存在且为正整数时 `Estimate[output_tokens]` = 该值；不存在则不填；存在但不是正整数 → `CodeInvalidRequest` |

| 协议 | 输出上限字段 | 亲和线索（§2.4） |
|:--|:--|:--|
| anthropic | `max_tokens` | `metadata.user_id`；否则前缀 = `tools` + `system` + `messages[0]` 的原始字节 |
| openai_chat | `max_completion_tokens`，不存在时 `max_tokens` | `prompt_cache_key`，否则 `user`；否则前缀 = `tools` + `messages[0]` + `messages[1]`（若存在） |

### 2.4 亲和线索（`Envelope.Affinity`）

适配器只负责**提取原始字节**，哈希与格式化由内核完成（spec/07 §7）。

| 编号 | 规则 |
|:--|:--|
| AFH-01 | 有用户/会话标识字段时：`Source = AffinityUser`，`Value` = 该字符串的 UTF-8 字节 |
| AFH-02 | 否则：`Source = AffinityPrefix`，`Value` = 上表中各字段原始 JSON 字节按顺序拼接，字段之间插入一个 `0x00` 分隔；字段不存在时用空字节参与拼接 |
| AFH-03 | 前缀线索只取「第一条（或前两条）消息」，因此同一会话追加新轮次不会改变线索 |
| AFH-04 | 所有相关字段都不存在时返回零值 `AffinityHint{}` |
| AFH-05 | 任务固定线索见 IN-G07；`Strict` 只能与 `AffinityPin` 一起使用 |

### 2.5 错误格式（`WriteError`）

| 协议 | 响应体 |
|:--|:--|
| anthropic | `{"type":"error","error":{"type":"<见下>","message":"..."},"request_id":"req_..."}`（已实现） |
| openai_chat | `{"error":{"message":"...","type":"<见下>","param":null,"code":"<apperr 错误码>"}}`（已实现） |
| 其他协议 | 模仿该上游官方 API 的错误格式（客户端 SDK 才能正确解析）；必须包含消息文本与 `Content-Type` |

错误码到 `error.type` 的映射在各自包的 `errorType` 表中（spec/09 §2.1 汇总）。状态码一律取 `err.Code.HTTPStatus()`，`Retry-After` 由 `apperr.WriteJSON` 统一处理。

### 2.6 流错误事件（`StreamError`）

SSE 流已开始（首字节已写出）后内核用它告知客户端出错，然后结束响应。没有 SSE 端点的协议也要实现（返回包含消息的事件即可，不会被调用）。

| 协议 | 事件 |
|:--|:--|
| anthropic | `Name: "error"`，`Data: {"type":"error","error":{"type":ErrorType(code),"message":"<消息>"}}` |
| openai_chat | `Name: ""`，`Data: {"error":{"message":"<消息>","type":ErrorType(code),"code":"<错误码>"}}`；内核在其后**不**发送 `[DONE]` |

### 2.7 工具（可选接口 `ToolSource`）

入站适配器可以额外实现 `protocol.ToolSource`，把本协议的操作暴露为 MCP / 命令行工具，规则见 spec/14 §1（TS-01 ~ TS-07）。不实现则该协议没有工具，其他一切不受影响。

### 2.8 目录（`WriteCatalog`）

`models` 已由内核按客户端白名单过滤并按 ID 升序排列。没有声明 `RouteCatalog` 的协议实现为写 404。

- anthropic：`{"data":[{"type":"model","id":"...","display_name":"...","created_at":"1970-01-01T00:00:00Z"}],"has_more":false,"first_id":<首个 id 或 null>,"last_id":<末个 id 或 null>}`
- openai_chat：`{"object":"list","data":[{"id":"...","object":"model","created":0,"owned_by":"universal-ai-gateway"}]}`

## 3. 出站适配器（Egress）

```go
type Egress interface {
    Protocol() Protocol
    Units() []string                                                         // 可能报告的计量单位
    Prepare(env *Envelope, upstreamModel string) (*Call, error)              // 同协议透传：每请求一次
    Do(ctx context.Context, up Upstream, call *Call) (*Response, error)      // 一次尝试
}
```

`Do` 的返回约定（内核据此决定重试与转发，spec/13 §4）：

| 情况 | 返回 |
|:--|:--|
| 得到可转发的响应 | `*Response`（`Kind` 决定转发形态；`Meter` 必填） |
| 尚未得到可转发的响应就失败（网络错误、上游非 2xx、轮询超时……） | `*AttemptError{Outcome, Body, Header}`：内核按 `Outcome.Kind.Retryable()` 决定是否换 Key |
| 已产生费用但后续步骤失败，无法交付完整结果 | 同时返回非 nil `*Response` 与 error；`Response.Meter` 必填，只用于结算，不转发 Body/流、不重试。仍使用现有 `Do` 签名（CORE-12） |
| 本地错误（不支持的操作、构造请求失败） | `*apperr.Error` 或其他 error：不重试，不惩罚 Key |
| 流式响应开始后上游出错 | `EventStream.Next` / `Raw.Read` 返回错误，并在 `Meter.Fault` 中报告 Outcome |

### 3.1 通用规则

| 编号 | 规则 |
|:--|:--|
| EG-01 | `Units()` 返回本协议**可能**报告的全部单位；`core.New` 要求模型价目表为每个单位定价（CORE-05）。`Meter.Usage` 不得报告 `Units()` 之外的单位 |
| EG-02 | 只使用 `up.Client` 发送请求，`ctx` 必须传给每个请求（`http.NewRequestWithContext`）；不得自建 `http.Client`、不得 `time.Sleep`（轮询等待用 `select { case <-ctx.Done(): case <-time.After(d): }`） |
| EG-03 | 发送用 `protocol.Send`（自动分类非 2xx、截断错误体、关闭连接）；缓冲读取用 `protocol.ReadBody`（受 `up.MaxBufferedBytes` 限制）；SSE 用 `protocol.NewSSEStream` |
| EG-04 | `Response.Header` 只放白名单头（至少 `Content-Type`；文件下载可加 `Content-Length`、`Content-Disposition`）。**禁止**转发 `Set-Cookie` 与任何限流、账号相关的头（会泄露上游 Key 信息） |
| EG-05 | 一次尝试内的多次上游调用（如提交后轮询）全部用同一个 `up.Secret`；任何一步失败都返回 `*AttemptError`。已产生费用时还必须返回带 Meter 的非 nil Response（CORE-12），避免丢失计量结果 |
| EG-06 | 创建异步任务的操作把任务 ID 放进 `Response.Pins`；查询该任务的请求由内核固定到同一个 Key（spec/07 §7.1） |
| EG-07 | `Call.BodyStream` 非 nil 时直接作为上游请求体（不得读入内存）；内核对这类请求只尝试一次（CORE-07） |
| EG-08 | `Meter.Usage` 的来源：上游明确报告 → `UsageUpstream`；部分报告 + 本地估算 → `UsageEstimated`；什么都没有 → `UsageNone`（内核按**预留用量**结算）。**确定不计费**的响应（如查询任务状态）返回 `(nil, UsageUpstream)`，不要返回 `UsageNone` |

### 3.2 透传改写（`Prepare`，大模型协议）

**原则：除下表列出的字段外，请求体其他字节保持原样**（包括 `cache_control`、未知字段、字段内部空白）。只能用 `jsonx.SetTopLevel` / `SetTopLevelString` 改写**顶层**字段。

| 编号 | 协议 | 改写 |
|:--|:--|:--|
| PT-01 | 全部 | `model` 改为 `upstreamModel` |
| PT-02 | 全部 | 请求体不是 JSON 对象 → `CodeInvalidRequest` |
| PT-03 | openai_chat | `stream == true` 且 `stream_options.include_usage` 不为 `true`：把 `stream_options` 设为原对象并加上 `"include_usage": true`（原对象不存在则为 `{"include_usage":true}`），在 `Call.State` 中记下「已注入」 |
| PT-04 | anthropic | 不做其他改写 |

### 3.3 上游请求（大模型协议）

| 协议 | 上游 URL |
|:--|:--|
| anthropic | `protocol.JoinURL(base_url, "/v1/messages")` |
| openai_chat | `protocol.JoinURL(base_url, "/v1/chat/completions")` |

| 编号 | 协议 | 规则 |
|:--|:--|:--|
| HDR-01 | 全部 | `POST`，`Content-Type: application/json`、`Accept: application/json, text/event-stream`；`base_url` 中已含路径时保留该前缀 |
| HDR-02 | 全部协议（含非大模型） | **只复制白名单头部**，客户端的其他头部一律丢弃，尤其是 `Authorization`、`x-api-key`、`x-goog-api-key`、`Cookie`、`X-Forwarded-*` |
| HDR-03 | anthropic | 鉴权头 `x-api-key: <secret>`；复制 `anthropic-version`（缺省设为 `2023-06-01`）与 `anthropic-beta`（全部值） |
| HDR-04 | openai_chat | 鉴权头 `Authorization: Bearer <secret>`；不复制其他头 |
| HDR-05 | 全部 | `User-Agent` 取适配器的 `UserAgent` 字段，为空时用 `universal-ai-gateway` |
| HDR-06 | 全部 | 共享上游客户端禁止自动跟随任何重定向（包括同来源、跨来源及 HTTPS 降级），使用 `CheckRedirect = http.ErrUseLastResponse`。需要新地址时由管理员修改池的 base_url；不得把鉴权头交给默认重定向规则处理 |

`Do` 的骨架（两个大模型适配器中已实现，只需实现其中调用的未完成函数）：`newRequest` → `protocol.Send` → 响应 `Content-Type` 以 `text/event-stream` 开头时返回 `ResponseEvents`（`NewSSEStream`，每个事件先交给 meter，再按 §3.5 决定是否转发），否则 `ReadBody` 返回 `ResponseBuffered`。

### 3.4 用量提取（`Meter`，大模型协议）

**Anthropic**

| 来源 | 字段 |
|:--|:--|
| 流式事件 `message_start` | `message.usage.{input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens}` |
| 流式事件 `message_delta` | `usage.{output_tokens, input_tokens, cache_creation_input_tokens, cache_read_input_tokens}`（可能只出现一部分） |
| 非流式响应体 | 顶层 `usage.{...}` |

映射：`input_tokens → input_tokens`、`output_tokens → output_tokens`、`cache_read_input_tokens → cache_read_tokens`、`cache_creation_input_tokens → cache_write_tokens`。

| 编号 | 规则 |
|:--|:--|
| USE-01 | 后出现的事件中**存在**的字段覆盖之前的值；不存在的字段保留之前的值（`message_delta` 中的数值是累计值，不是增量） |
| USE-02 | 观察到 `input_tokens` 记为「有输入」；观察到 `message_delta`（或非流式响应体）记为「有最终输出」 |

**OpenAI 兼容**

| 编号 | 规则 |
|:--|:--|
| USE-03 | 用量来自流式中顶层 `usage` 非 null 的 chunk，或非流式响应体的顶层 `usage`。`cached = prompt_tokens_details.cached_tokens`，不存在时取 DeepSeek 的 `prompt_cache_hit_tokens`，都不存在为 0 |
| USE-04 | `input_tokens = max(prompt_tokens - cached, 0)`、`cache_read_tokens = cached`、`output_tokens = completion_tokens` |
| USE-05 | 流式中 `data: [DONE]` 不是 JSON，直接忽略 |
| USE-06 | 观察到 `prompt_tokens` 记为「有用量」 |

**两者共同**

| 编号 | 规则 |
|:--|:--|
| USE-07 | meter 累计已观察到的输出文本字节数（anthropic：`content_block_delta` 的 `delta.text`；openai：`choices[*].delta.content`）。`Usage()`：用量完整 → `UsageUpstream`；只有部分（流中断）→ 缺失的输出用 `pricing.EstimateOutputTokens(文本字节)` 估算，`UsageEstimated`；什么都没有 → `UsageNone`。`Usage()` 已在两个包中实现 |

### 3.5 透传时的事件过滤（大模型协议）

| 编号 | 协议 | 规则 |
|:--|:--|:--|
| FWD-01 | anthropic | 全部转发（包括 `ping`、`error`） |
| FWD-02 | openai_chat | 已注入 usage（PT-03）时，**不转发**「`choices` 为空数组且 `usage` 非 null」的 chunk（客户端没有要求 usage，很多客户端会对空 `choices` 取 `[0]` 而崩溃） |
| FWD-03 | openai_chat | 其他事件（含 `[DONE]`）全部转发 |

### 3.6 流中错误事件分类

| 编号 | 协议 | 规则 |
|:--|:--|:--|
| STR-05 | anthropic | `Name == "error"`：解析 `error.type`，`overloaded_error` → `Overloaded`，`rate_limit_error` → `RateLimited`，其他 → `ServerError`；其余事件 `ok = false`。结果记入 `Meter.Fault` |
| STR-06 | openai_chat | 没有标准错误事件，`Fault` 恒为 false |

### 3.7 通用工具（`internal/protocol/httpx.go`，已实现）

```go
func Send(up Upstream, req *http.Request) (*http.Response, error)   // 非 2xx / 网络错误 → *AttemptError
func ReadBody(up Upstream, resp *http.Response) ([]byte, error)      // 超限 → *AttemptError{LocalError, ErrResponseTooLarge}
func JoinURL(base, path string) string
func CopyHeaders(dst, src http.Header, names ...string)
func NewSSEStream(body io.ReadCloser, inspect func(sse.Event) (forward bool, err error)) EventStream
```

## 4. 跨协议转换器（Translator）

接口见 `internal/protocol/translate.go`，规则见 spec/10：`Request(env, upstreamModel) (*Call, error)` 生成上游协议的 Call，`Response(env, resp) (*Response, error)` 把上游响应转换为入站协议的响应（缓冲体转换 Body；事件流包装为新的 EventStream；字节流通常原样返回）。M1 不注册任何转换器；内核在找不到转换器时返回 400（CORE-03）。

## 5. 一致性测试套件（`internal/adapter/adaptertest`，已提供）

每个适配器包必须有 `conformance_test.go`，调用：

```go
adaptertest.RunIngress(t, Ingress{}, adaptertest.IngressFixture{Valid: ..., Invalid: ...})
adaptertest.RunEgress(t, Egress{}, adaptertest.EgressFixture{Cases: ..., InvalidRequests: ...})
```

入站样例是 `IngressCase`（操作、路径、查询串、请求头、请求体 + 期望的服务名、流式、估计用量、参数、严格亲和），套件通过真实的 `http.ServeMux` 调用 `Parse`，所以 `r.PathValue` 与生产一致。
出站样例是 `EgressCase`（信封 + 脚本化假上游 `http.Handler` + 期望的响应形态、用量、来源、Pins、Fault、客户端字节）。假上游的 base_url 带路径前缀 `/prefix`，交给 Handler 前已去掉。流式请求体用 `NewBodyStream` 提供（每次尝试新建一个）。

| 编号 | 检查 |
|:--|:--|
| CONF-I01 | 至少 1 个调用路由，Op 非空且唯一；至多 1 个目录路由 |
| CONF-I02 | `Valid` 样例覆盖每一个调用操作；`Invalid` 至少 1 个 |
| CONF-I03 | 合法样例解析正确：协议名、Op、服务名（`WantNoModel` 时为空，且空服务名必须伴随严格亲和）、`Stream`、`Estimate`（单位合法且与期望相同）、`Params`、`Body`（缓冲时原样，否则 nil）、`BodyStream` 为 nil、`Header` 非空、严格亲和只与 `AffinityPin` 搭配 |
| CONF-I04 | 非法样例返回 `invalid_request` |
| CONF-I05 | 每种错误码的状态码、`Retry-After`、`Content-Type`、包含消息 |
| CONF-I06 | `StreamError` 的数据包含消息 |
| CONF-I07 | 声明了目录路由时，`WriteCatalog` 对空列表和非空列表都返回 200、`Content-Type` 与全部 ID |
| CONF-E01 | `Units()` 名称合法；样例报告的单位都已声明 |
| CONF-E02 | `Prepare` 拒绝 `InvalidRequests` |
| CONF-E03 | 每个样例：响应形态、`Content-Type`、不转发 `Set-Cookie`、客户端字节、用量与来源、`Fault`、`Pins` 都符合期望；**每个上游请求**都在 base_url 路径之下、不含客户端凭据与 `X-Forwarded-For`、在头或查询串中携带上游凭据 |
| CONF-E04 | 上游返回 400/401/403/429/500/503 时 `Do` 返回 `*AttemptError`，分类与 `ClassifyHTTP` 一致，429 带 `RetryAfter`，400 带错误体 |
| CONF-E05 | 两次尝试的 Meter 互不影响 |
| CONF-E06 | `Prepare` 不修改 `Envelope.Body` |

工具：`adaptertest.ServeFile` / `ServeBody`（假上游）、`AssertTopLevelPreserved`（JSON 协议在 `CheckUpstream` 中检查 PT-01）。

套件本身是架构约束：**不得为了让适配器通过而修改套件**；套件有误时向架构负责人报告。新协议可以在自己的测试中追加协议特有的检查。

## 6. 扩展指南：新增一个协议

1. **建包**：`internal/adapter/<name>/`，包含 `register.go`（包注释、`const Protocol protocol.Protocol = "<name>"`、操作名常量、`Register`）、`ingress.go`、`egress.go`。照抄 `anthropic` 包的结构。
2. **设计**：先写出「端点 → 操作 → 请求体模式 → 计量单位 → 响应形态 → 是否异步」的表格（参照 §8 的范例），追加为本章的一个小节，交架构负责人确认。
3. **实现接口**：按 §2.1、§3.1 的通用规则实现；协议特有规则写进上一步的小节。
4. **一致性测试**：写 `conformance_test.go` 与 `testdata/` 样例，`RunIngress` / `RunEgress` 全部通过。
5. **注册**：在 `cmd/gateway/adapters.go` 的 `adapterRegistrars` 中加一行 `<name>.Register`。
6. **登记分层**：在 `internal/archtest/archtest_test.go` 的 `layers` 中加 `"internal/adapter/<name>": 2`（需要架构负责人批准）。
7. **配置**：用户在 `pools[].protocol` 写 `<name>`，在 `models[].price` 为 `Units()` 的每个单位定价。

**不需要**修改：`internal/core`、`internal/httpapi`、`internal/admin`、`internal/protocol`、`internal/config`、`internal/pricing`、`internal/ledger`、`internal/store`。如果你发现必须修改它们才能完成接入，说明契约不够用——停下，向架构负责人提出契约变更（spec/13 §7），不要绕过。

## 7. 上游错误分类工具（`internal/protocol/classify.go`，已提供）

```go
func ClassifyHTTP(status int, h http.Header, now time.Time) Outcome
func ClassifyNetError(err error, clientCtx context.Context) Outcome
func ParseRetryAfter(h http.Header, now time.Time) time.Duration
```

协议无关的通用规则，`protocol.Send` 已在内部使用。`ParseRetryAfter` 识别 `Retry-After`（秒数或 HTTP-date）、`anthropic-ratelimit-*-reset`（RFC 3339）、`x-ratelimit-reset-*`（Go 时长格式），取最大的正值。上游用 200 + 错误体表示失败的协议（不少国内 API 如此），由适配器在 `Do` 中解析错误体并自行构造 `*AttemptError`（`Outcome` 按语义选择）。

## 8. 非大模型 API 接入

以下两个范例已用临时实现跑通一致性套件，证明契约足以表达它们。真正接入时以厂商当时的官方文档为准核对字段。

### 8.1 搜索 API（以 Tavily 为例，正式规则，已实现）：多端点、无模型字段、按 credits 计费

| 项 | 设计 |
|:--|:--|
| 协议名 | `tavily` |
| 入站路由 | `POST /search`（Op `search`）、`POST /extract`（Op `extract`）、`POST /crawl`（Op `crawl`）；都是 `BodyBuffered` JSON；无目录路由 |
| 服务名 | 固定 `tavily-search` / `tavily-extract` / `tavily-crawl`，可用 `X-UAIG-Model` 覆盖（IN-G03） |
| 估计用量 | `search`：`search_depth == "advanced"` 时 `credits: 2`，否则 `credits: 1`；`extract` / `crawl`：不填，由 `reserve` 决定 |
| 客户端鉴权 | 客户端用 `Authorization: Bearer <网关 Token>`（AUTH-01）。请求体中的 `api_key` 字段必须在 `Prepare` 中删除（`jsonx.SetTopLevel` 不支持删除时，用改写为上游 Key 的方式覆盖），不得转发 |
| 出站 | `POST <base_url>/<op>`，`Authorization: Bearer <secret>`，请求体其他字段原样 |
| 单位 | `Units() = ["credits"]` |
| 用量 | 响应体中有用量字段时 `UsageUpstream`；没有时按请求推算的 credits，`UsageEstimated` |
| 响应 | `ResponseBuffered`，`Content-Type` 原样 |

配置：`price: { credits: "0.008" }`，`reserve: { credits: 2 }`。

### 8.2 文档解析 API（以 MinerU 为例，正式规则，已实现）：文件上传、异步任务、结果下载

上游是「提交任务 → 轮询状态 → 下载结果」的异步 API。适配器可以同时提供两种用法：

| 用法 | 入站路由 | 行为 |
|:--|:--|:--|
| 同步封装 | `POST /v1/parse`（Op `parse`，`BodyStream`，`MaxBodyBytes` = 上游单文件上限） | `Do` 内提交并轮询直到完成，返回结果（`ResponseBuffered` 的 Markdown 或 JSON）。整个过程受池的 `timeouts.response_header_base` 约束，该池应配置为分钟级 |
| 异步透传 | `POST /v1/tasks`（Op `task.create`，`BodyStream` 或 JSON 中给文件 URL） | 返回任务 ID，`Response.Pins = [任务 ID]` |
| | `GET /v1/tasks/{id}`（Op `task.get`，`BodyNone`） | `Affinity = {AffinityPin, id, Strict}`；任务完成时 Meter 报告 `pages` |
| | `GET /v1/tasks/{id}/result`（Op `task.result`，`BodyNone`） | `ResponseRaw`（zip 原样流式转发），`Content-Type`、`Content-Length` 可转发 |

| 项 | 设计 |
|:--|:--|
| 单位 | `Units() = ["pages", "requests"]` |
| 估计用量 | 提交类操作：`pages` 取查询参数或请求中声明的页数上限，没有则不填（由 `reserve` 决定） |
| 计费时点 | 同步封装：完成时按上游报告的实际页数结算（`UsageUpstream`）。异步透传：同一任务会被查询多次，而适配器无状态、无法判断「第一次看到完成」，所以**提交时按估计页数结算**（`UsageEstimated`）；查询与下载的 `Estimate` 写 `{pages: 0, requests: 0}`，Meter 返回 `(nil, UsageUpstream)`，不计费。需要精确按页计费的部署应使用同步封装 |
| 任务固定 | 任务只存在于创建它的上游账号下；`pin_ttl` 应不短于结果在上游的保存时长。网关重启会丢失固定记录（内存），此时查询返回 404 `not_found`，客户端需重新提交 |
| 上传 | 文件请求体直接流式转发给上游（EG-07），不进内存，不重试 |
| 任务提交后重试与结算 | 提交返回 2xx 后即进入付费阶段。后续读取、轮询、下载失败或客户端取消都**不可重试**；同时返回 Meter 与错误（CORE-12），页数已知时按上游实际页数结算，否则按提交估计与预留补齐。任务 ID 已知时保留在内部错误原因中；网关对外只输出安全的协议错误，不转发部分结果 |

配置：`price: { pages: "0.01", requests: "0" }`，`reserve: { pages: 600 }`；池：`pin_ttl: 48h`、`timeouts: { response_header_base: 10m }`。

### 8.3 通用 REST 转发（`rest`，任务卡 T51）

通用 REST 转发适配器（协议名 `rest`，包 `internal/adapter/rest`）用于快速接入无需请求体解析的任意外部 REST API（搜索、天气、通知、翻译等），实现「统一认证、上游换 Key、按次计费、透明转发」。

#### 8.3.1 包与路由

| 编号 | 规则 |
|:--|:--|
| RS-01 | 包 `internal/adapter/rest`；`const Protocol protocol.Protocol = "rest"`；注册优先级与 `tavily` 相同（10），不是 Fallback；`Claims` 恒为 false |
| RS-02 | 路由（都是 `RouteCall`，无目录路由），路径一律为 `/rest/{service}/{path...}`：`GET` → Op `rest.get`，`BodyNone`（`http.ServeMux` 的 `GET` 模式同时匹配 `HEAD`，HEAD 请求也走这条路由，不单独声明）；`DELETE` → `rest.delete`，`BodyNone`；`PUT` → `rest.put`，`BodyBuffered`；`POST` → `rest.post`，`BodyStream`；`PATCH` → `rest.patch`，`BodyStream`。注册表要求 Op 在协议内唯一（REG-02），每个方法只有这一条路由；访问上游根路径用 `/rest/{service}/`（`{path...}` 可以为空）。另有两条可重试的路由，路径为 `/rest-idem/{service}/{path...}`：`POST` → `rest.post_idem`，`BodyBuffered`；`PATCH` → `rest.patch_idem`，`BodyBuffered`（`/rest-idem/` 下的其他方法不声明，落到数据面兜底路由，按 RTE-04 返回 404）。共 7 条路由 |
| RS-03 | `Envelope.Model` = `{service}`，即配置 `models[].name`（`/rest/` 与 `/rest-idem/` 相同，`allowed_models` 对两者同样生效）；`X-UAIG-Model` 不适用（服务名已在路径里）。`{service}` 为空 → `CodeInvalidRequest` |
| RS-04 | `Params`：`method`（`r.Method` 原样，因此 HEAD 以 HEAD 转发）、`path`（`{path...}` 的转义形式，取自 `r.URL.EscapedPath()`，不含 `/rest/{service}` 或 `/rest-idem/{service}` 前缀）、`query`（`r.URL.RawQuery` 原样） |
| RS-05 | 路径安全：`path` 按 `/` 拆段后，任一段在百分号解码后等于 `.` 或 `..`，或含 `\`、NUL → `CodeInvalidRequest`，消息 `path must not contain dot segments` |
| RS-06 | `Estimate = {requests: 1}`；无亲和线索 |

#### 8.3.2 出站

| 编号 | 规则 |
|:--|:--|
| RS-10 | 上游 URL = `protocol.JoinURL(base_url, path)`，有 `query` 时追加 `?` + `query`。`Call.Model`（`upstream_model`）不参与 URL |
| RS-11 | 方法取 `Params["method"]`，只允许 `GET`、`HEAD`、`DELETE`、`PUT`、`POST`、`PATCH`，其他值 → `CodeInvalidRequest`；`PUT`、`rest.post_idem`、`rest.patch_idem` 的请求体用 `Call.Body`，`rest.post` / `rest.patch` 用 `Call.BodyStream`，其他方法无请求体 |
| RS-12 | 请求头白名单（HDR-02）：`Content-Type`、`Accept`、`Accept-Language`、`If-Match`、`If-None-Match`、`If-Modified-Since`、`If-Unmodified-Since`、`Range`。另外，客户端请求头 `X-UAIG-Forward-<Name>: <value>` 以 `<Name>: <value>` 转发，用于上游要求的自定义头（如 `X-Api-Version`）；`<Name>` 是下列之一时丢弃并记一次 Warn：`Authorization`、`Proxy-Authorization`、`Cookie`、`Host`、`Content-Length`、`Transfer-Encoding`、`Connection`、`Upgrade`、`TE`、`Trailer`、`Keep-Alive`、`X-Api-Key`、`X-Goog-Api-Key`、任何 `X-Forwarded-*` 与 `X-UAIG-*` |
| RS-13 | `User-Agent` 同 HDR-05 |
| RS-14 | 上游 Key 按 §8.3.3 放置；Key 放置在头里时，同名的客户端转发头被覆盖；放置在查询参数里时，客户端查询串中的同名参数被删除 |
| RS-15 | 2xx 响应一律 `ResponseRaw`，`Status` = 上游状态码；响应头白名单：`Content-Type`、`Content-Length`、`Content-Disposition`、`Content-Range`、`Accept-Ranges`、`Cache-Control`、`ETag`、`Last-Modified`、`Location`、`Link`（分页）、`Retry-After`、`RateLimit`、`RateLimit-Policy`，以及名字以 `X-RateLimit-` 或 `RateLimit-` 开头的头（剩余额度）。上游是 `text/event-stream` 时同样按字节流转发，不解析 |
| RS-16 | 非 2xx：用 `protocol.Send` 的分类，但上游 403 改写为 `OutcomeClientError`（Status 保持 403），使内核原样转发错误体而不隔离 Key。每次改写都记一条 Warn：`rest upstream returned 403; key not quarantined`，字段为上游主机、去掉查询串的路径、`obs.MaskSecret(Key 明文)`；同一掩码每分钟至多一条（内存表以掩码为键，上限 1024 项，满了整体清空；有锁，无 goroutine）。错误体超过 8 KiB（`MaxErrorBodyBytes`）时被截断，这是已知限制，写进部署文档 |
| RS-17 | 网络错误：交给内核前，把 `*url.Error` 的 `URL` 字段替换为去掉查询串的 URL（无论 Key 放在哪里，一律去掉），避免日志泄露凭据或客户端参数 |
| RS-18 | `Units() = ["requests"]`；Meter 在 2xx 时报告 `{requests: 1}`（`UsageUpstream`），`Fault()` 恒为无 |
| RS-19 | `rest.post_idem` / `rest.patch_idem` 与 `PUT` 一样按 spec/07 §6 重试（429、5xx、超时、网络错误换 Key），同一请求体可能被上游收到多次。只应用于没有副作用的接口，由客户端负责选择；DEPLOYMENT.md 写明。计费仍是每个成功请求 `requests: 1`（失败的尝试不计费） |

#### 8.3.3 Key 的放置方式（`Secret` 的格式）

| 写法 | 上游请求 | 说明 |
|:--|:--|:--|
| `bearer:<token>` | `Authorization: Bearer <token>` | 缺省或通用 Bearer Token |
| `header:<Name>:<value>` | `<Name>: <value>` | `<Name>` 必须是合法的头名（RFC 9110 token），且不能是 RS-12 丢弃列表中除 `Authorization`、`X-Api-Key` 以外的名字 |
| `query:<name>:<value>` | 查询参数 `<name>=<value>`（URL 编码） | `<name>` 只允许 `[A-Za-z0-9_.-]` |
| `basic:<user>:<password>` | `Authorization: Basic base64(user:password)` | 密码中可以含 `:`（只按第一个 `:` 拆分 user） |
| 不以上述前缀开头 | 视为 `bearer:<整个明文>` | 兼容最常见的情况 |

| 编号 | 规则 |
|:--|:--|
| RS-20 | 格式不合法（如 `header:` 后缺少 `:`、头名非法）→ `Do` 返回 `*AttemptError{Outcome: OutcomeAuthFailed}`，错误信息只写「rest upstream key format is invalid」，不含任何部分明文。调度器因此隔离这个 Key，管理员会在 Key 列表里看到它 |
| RS-21 | 管理 API 添加 Key 时不校验格式（管理面不认识协议）；DEPLOYMENT.md 写明格式与示例 |

#### 8.3.4 部署文档安全规范

| 编号 | 规则 |
|:--|:--|
| RS-30 | DEPLOYMENT.md「接入任意 REST API」一节开头用醒目的提示框写明：Key 的全部权限都暴露给被允许访问该服务的客户端，只放最小权限的 Key；读写分离用两个服务名 + `allowed_models` |
| RS-31 | 写明 RS-16 的风险：上游用 403 表示 Key 失效时，Key 不会被自动隔离；看到同一掩码的 Warn 反复出现，就用 `PATCH /admin/api/keys/{id}`、请求体 `{"state":"disabled"}` 停用该 Key（ADM-15） |

#### 8.3.5 接口声明（任务卡 T54）

服务可以在配置中声明接口（`models[].tools`，spec/03 CFG-17）。网关把每个接口作为一个工具交给 MCP 与命令行（spec/14），调用时走与直接请求相同的路由。参数映射：路径中的 `{name}` 从同名参数取值（按单个路径段转义，空串、`.`、`..` 拒绝）并移出参数表；`GET` / `DELETE` 的其余参数放进查询串（标量各一个值、标量数组重复同名参数、`null` 省略、对象或嵌套数组拒绝）；`POST` / `PUT` / `PATCH` 的其余参数原样组成 JSON 请求体（数字保持原文）。`input_schema` 只给 AI 看，网关不按它做完整校验。接口声明**不是**权限边界：被允许访问该服务的客户端仍然可以直接请求 `/rest/{service}/` 下的任意路径（RS-30）。

| 编号 | 规则 |
|:--|:--|
| RS-40 | `rest.Options{Logger *slog.Logger; Tools []ToolDef}`；`RegisterWithOptions(r, opts)`。`ToolDef` 是配置中接口声明的适配器侧表示，另含 `Service`（所属 `models[].name`）。`Register`、`RegisterWithLogger` 保留，等价于没有工具。`Ingress` 实现 `protocol.ToolSource`；零值 `Ingress{}` 的 `Tools()` 返回空 |
| RS-41 | `Tools()`：每个声明生成一个 `protocol.Tool`，`Model` = `Service`，`ReadOnly` 按 CFG-17 的缺省规则，`InputSchema` 为声明的 Schema 编码成的 JSON；按名称升序。工具名与其他适配器冲突时由 `Registry.Seal` 报错，网关拒绝启动（REG-05） |
| RS-42 | `ToolRequest(name, args)` 按 §2.2 映射。Op：`GET` → `rest.get`，`DELETE` → `rest.delete`，`PUT` → `rest.put`，`POST` → `idempotent` ? `rest.post_idem` : `rest.post`，`PATCH` 同理。`PathValues` = `{"service": Service, "path": 去掉开头 / 并填好占位符（已转义）的路径}`。参数不合法 → `CodeInvalidRequest`，消息指明参数名，如 `argument "owner" must be a string or number` |
| RS-43 | `ToolResult`：2xx 时，响应体为空 → 文本 `empty response (HTTP <status>)`；`Content-Type` 为 `application/json` 或 `*+json` 且是合法 JSON → 文本为响应体原文，顶层是对象时同时放进 `Structured`；`text/*` → 文本为响应体（必须是合法 UTF-8，否则同下）；没有 `Content-Type` → 按文本处理（不少 API 不设置它），是合法 JSON 对象时同样放进 `Structured`；其他类型 → `IsError`，文本 `binary response (<Content-Type>, <N> bytes) is not supported by tools`，不包含响应内容。非 2xx → `IsError`，文本 `HTTP <status>: ` + 响应体前 2000 字节（在 UTF-8 字符边界截断）。`ToolResult` 不返回 error |
| RS-44 | 工具调用与直接调用同一路由，鉴权、白名单（TL-03 / TL-06）、计费、选 Key、重试、403 告警全部沿用 T51 规则，本卡不增加特殊路径 |
| RS-45 | DEPLOYMENT.md §9 新增「把接口暴露给 AI」：配置示例（Bivia 的 POST 接口、带占位符的 GET 接口）、参数映射规则、`idempotent` 与 `read_only` 的含义、二进制响应不支持、修改后需重启；并醒目写明 §2.5：接口声明不是权限边界 |

## 9. OpenAI Responses API（`openai_responses`，任务卡 T09C）

`internal/adapter/openairesp`。Codex CLI 等新客户端使用的 OpenAI 接口。与 Chat Completions 的关键区别是**有状态**：`store` 缺省为 true，响应保存在上游账号下，之后的 `previous_response_id` 与按 ID 的操作都只能在同一个上游账号上执行——所以本协议依赖任务固定（spec/07 §7.1）。

### 9.1 入站

| 编号 | 规则 |
|:--|:--|
| IN-R01 | 路由（已实现）：`POST /v1/responses`（`responses.create`，缓冲）、`GET /v1/responses/{id}`（`responses.get`）、`DELETE /v1/responses/{id}`（`responses.delete`）、`POST /v1/responses/{id}/cancel`（`responses.cancel`）、`GET /v1/responses/{id}/input_items`（`responses.input_items`）；后四个 `BodyNone`。不声明目录路由（`GET /v1/models` 由 openai_chat 提供） |
| IN-R02 | `responses.create`：IN-L01 ~ IN-L03；`Estimate[input_tokens] = EstimateInputTokens(len(body))`，`max_output_tokens` 存在且为正整数时 `Estimate[output_tokens]` = 该值，存在但不是正整数 → `CodeInvalidRequest` |
| IN-R03 | `previous_response_id` 是非空字符串时：`Affinity = {AffinityPin, 该 ID, Strict: true}`（续写必须回到保存上一个响应的账号） |
| IN-R04 | 否则亲和线索：`prompt_cache_key` → `AffinityUser`；否则 `safety_identifier`，再否则 `user` → `AffinityUser`；否则前缀 = `instructions` + `tools` + `input` 的第一个元素（`input` 是字符串时取整个字符串）→ `AffinityPrefix`（AFH-02 的拼接规则） |
| IN-R05 | 按 ID 的四个操作：`Model = r.Header.Get("X-UAIG-Model")`（通常为空，见 IN-G03）；`Params = {"id": r.PathValue("id"), "query": r.URL.RawQuery}`；`Estimate` 中三个单位全部显式为 0；`Affinity = {AffinityPin, id, Strict: true}` |
| IN-R06 | 错误格式与 openai_chat 相同（已实现） |
| IN-R07 | `StreamError`：`Name: "error"`，`Data: {"type":"error","code":"<错误码>","message":"<消息>","param":null}` |

### 9.2 出站

| 编号 | 规则 |
|:--|:--|
| PT-R01 | `Prepare`（已实现骨架）：`responses.create` 用 `prepareBody` 改写 `model`；其他操作不改写，直接带上 `Params` |
| PT-R02 | 不注入任何字段（Responses 的流与非流响应都包含 `usage`） |
| PT-R03 | `prepareBody` 同时返回 `store`（`store` 字段为 `false` 时 false，不存在或为 true 时 true） |
| EG-R01 | 各操作的上游方法与路径见 `upstreamEndpoint`（已实现）：查询串原样透传到 `get` 与 `input_items` |
| HDR-R01 | `Authorization: Bearer <secret>`；有请求体时 `Content-Type: application/json`；`Accept: application/json, text/event-stream` |
| HDR-R02 | 白名单只复制 `OpenAI-Beta`（全部值）；其他客户端头一律丢弃（HDR-02） |
| HDR-R03 | `User-Agent` 同 HDR-05 |
| EG-R04 | `responses.create` 且 `store == true`：把响应 ID 放进 `Response.Pins`。非流式取响应体顶层 `id`；流式取 `response.created` 事件的 `response.id`，在流中追加到 `Pins`（CORE-08 在转发结束后读取，已实现于 `Do`） |
| EG-R05 | 其他操作的 Meter 恒为 `(nil, UsageUpstream)`（不计费，已实现） |

### 9.3 用量与流中错误

| 编号 | 规则 |
|:--|:--|
| USE-R01 | 用量来自非流式响应体的顶层 `usage`，或流式 `response.completed` / `response.incomplete` / `response.failed` 事件的 `response.usage`；后出现的覆盖先出现的 |
| USE-R02 | `cached = usage.input_tokens_details.cached_tokens`（不存在为 0）；`input_tokens = max(usage.input_tokens - cached, 0)`、`cache_read_tokens = cached`、`output_tokens = usage.output_tokens`（已包含推理 token） |
| USE-R03 | `response.output_text.delta` 的 `delta` 字节数累计到 `textBytes`（USE-07） |
| USE-R04 | `Usage()`（已实现）：非 create 操作 → `(nil, UsageUpstream)`；有用量 → `UsageUpstream`；只有文本 → 估算输出，`UsageEstimated`；都没有 → `UsageNone` |
| STR-R01 | `response.failed` 事件（`response.error.code`）与 `error` 事件（顶层 `code`）记为 Fault：`rate_limit_exceeded` → `RateLimited`；`server_error`、`server_is_overloaded`、`slow_down` → `Overloaded`；其他 → `ServerError` |

### 9.4 工具 `ask_model`（spec/14 §1）

| 编号 | 规则 |
|:--|:--|
| TL-R01 | `ToolRequest`：参数 `model`（必填，非空字符串）、`input`（必填，非空字符串）、`instructions`（可选字符串）、`max_output_tokens`（可选正整数），不允许其他字段；生成 `responses.create` 请求，请求体为上述字段加 `"store": false`，**不**设置 `stream` |
| TL-R02 | `ToolResult`：2xx → 依次拼接 `output[*]` 中 `type == "message"` 的元素里 `content[*]` 中 `type == "output_text"` 的 `text`；非 2xx → `error.message`，`IsError = true` |

## 10. Gemini API（`gemini`，任务卡 T47）

本章规定 Google AI Studio 的 Gemini API（`generativelanguage.googleapis.com`）同协议透传适配器。

### 10.1 包与常量

| 项 | 规定 |
|:--|:--|
| 包 | `internal/adapter/gemini` |
| 协议名 | `const Protocol protocol.Protocol = "gemini"` |
| 操作名 | `const OpModelsCall = "models.call"` |
| Params 键 | `method`：`generateContent` / `streamGenerateContent` / `countTokens` |

### 10.2 入站（Ingress）

| 编号 | 规则 |
|:--|:--|
| IN-G-01 | 路由：`POST /v1beta/models/{target}`（Call，Op `models.call`，`BodyBuffered`）；`GET /v1beta/models`（Catalog）。*待定 1：不支持 `/v1/...`：SDK 与 Gemini CLI 默认用 `v1beta`，且 `Op` 必须唯一* |
| IN-G-02 | `target` 按**最后一个** `:` 拆分；拆不出来，或模型名为空 → `CodeInvalidRequest`，消息 `path must be /v1beta/models/{model}:{method}` |
| IN-G-03 | 方法只接受 `generateContent`、`streamGenerateContent`、`countTokens`；其他方法 → `CodeInvalidRequest`，消息 `method "<m>" is not supported` |
| IN-G-04 | `streamGenerateContent` 必须带查询参数 `alt=sse`，否则 `CodeInvalidRequest`，消息 `streamGenerateContent requires alt=sse`。*待定 2：不带 `alt=sse` 时返回逐步输出的 JSON 数组无法被流式处理，予以拒绝* |
| IN-G-05 | `Envelope.Model` = 拆出的模型名（不带 `models/` 前缀）；`Stream` = 方法是 `streamGenerateContent`；`Params = {"method": <方法>}` |
| IN-G-06 | 请求体不是 JSON 对象 → `CodeInvalidRequest`（同 IN-L01）。不读取请求体里的 `model`、`stream` |
| IN-G-07 | `Estimate`：`generateContent` / `streamGenerateContent`：`input_tokens = EstimateInputTokens(len(body))`；`generationConfig.maxOutputTokens` 存在且为正整数时 `output_tokens` = 该值，存在但不是正整数 → `CodeInvalidRequest`。`countTokens`：三个单位全部显式为 0（不计费，IN-G04） |
| IN-G-08 | 亲和线索：Gemini 无用户标识字段，用前缀线索：`systemInstruction` + `tools` + `contents[0]`（AFH-02 拼接规则）。`countTokens` 不设亲和 |
| IN-G-09 | 查询串里只有 `alt` 有意义，其他查询参数（含 `key=`）一律不读、不转发。客户端需将网关 Token 放在 `x-goog-api-key` 头或 `Authorization: Bearer` 中 |
| IN-G-10 | `Claims` 恒为 false（路由不与其他协议冲突） |

**错误格式**（`WriteError`）：

```json
{"error":{"code":<HTTP 状态码>,"message":"<消息>","status":"<状态名>"}}
```

| apperr 错误码 | `status` |
|:--|:--|
| `invalid_request`、`request_too_large` | `INVALID_ARGUMENT` |
| `unauthenticated` | `UNAUTHENTICATED` |
| `permission_denied` | `PERMISSION_DENIED` |
| `not_found` | `NOT_FOUND` |
| `conflict` | `ABORTED` |
| `rate_limited`、`insufficient_quota` | `RESOURCE_EXHAUSTED` |
| `not_implemented` | `UNIMPLEMENTED` |
| `timeout` | `DEADLINE_EXCEEDED` |
| `overloaded`、`upstream_unavailable` | `UNAVAILABLE` |
| `internal`、`upstream_error` | `INTERNAL` |

**流错误事件**（`StreamError`）：`Name: ""`，`Data` 为上述错误体 JSON。

**目录**（`WriteCatalog`）：

```json
{"models":[{"name":"models/<id>","displayName":"<id>","supportedGenerationMethods":["generateContent","streamGenerateContent","countTokens"]}]}
```

### 10.3 出站（Egress）

| 编号 | 规则 |
|:--|:--|
| PT-G-01 | `Prepare`：请求体**原样**作为上游请求体，不做改写；`Call.Model = upstreamModel`，`Call.Stream = env.Stream`，`Call.Params = env.Params` |
| PT-G-02 | 请求体不是 JSON 对象 → `CodeInvalidRequest`（PT-02） |
| EG-G-01 | 上游方法：`Params["method"]` 为 `countTokens` 时用 `countTokens`；否则 `Call.Stream` 为 true 用 `streamGenerateContent`，为 false 用 `generateContent` |
| EG-G-02 | 上游 URL：`protocol.JoinURL(base_url, "/v1beta/models/" + url.PathEscape(Call.Model) + ":" + 方法)`；流式时追加 `?alt=sse` |
| HDR-G-01 | `POST`；`Content-Type: application/json`；`Accept: application/json, text/event-stream`；鉴权头 `x-goog-api-key: <secret>`。**不**使用查询参数 `key=` 传输上游 Key |
| HDR-G-02 | 不复制任何客户端头（HDR-02）；`User-Agent` 同 HDR-05 |
| EG-G-03 | 响应 `Content-Type` 以 `text/event-stream` 开头时返回 `ResponseEvents`，否则 `ResponseBuffered` |
| EG-G-04 | 上游 429 错误体包含 `error.details[]` 中 `@type` 以 `RetryInfo` 结尾的项时，提取 `retryDelay`（如 `"30s"`）作为 `AttemptError.Outcome.RetryAfter` |
| EG-G-05 | `Units() = [input_tokens, output_tokens, cache_read_tokens]` |

**用量（`Meter`）**：

| 编号 | 规则 |
|:--|:--|
| USE-G-01 | 来源：非流式响应体顶层 `usageMetadata`；流式时每个 chunk 携带的 `usageMetadata` 为累计值，后到覆盖先到 |
| USE-G-02 | `cached = cachedContentTokenCount`；`input_tokens = max(promptTokenCount − cached, 0) + toolUsePromptTokenCount`；`cache_read_tokens = cached`；`output_tokens = candidatesTokenCount + thoughtsTokenCount`（思考按输出计费） |
| USE-G-03 | `candidates[*].content.parts[*].text` 累计为 `textBytes`（USE-07 估算依据），含 `thought: true` 部分 |
| USE-G-04 | `Usage()`：`countTokens` → `(nil, UsageUpstream)`；有 `promptTokenCount` → `UsageUpstream`；只有文本 → `UsageEstimated`；都没有 → `UsageNone` |

**事件过滤与流中错误**：

| 编号 | 规则 |
|:--|:--|
| FWD-G-01 | 透传时所有 SSE 事件原样转发 |
| STR-G-01 | 带顶层 `error` 对象的 chunk 记为 Fault：`error.code == 429` 或 `status == "RESOURCE_EXHAUSTED"` → `RateLimited`；`503` / `UNAVAILABLE` → `Overloaded`；其他 → `ServerError` |
| STR-G-02 | *待定 3：Gemini SSE 无 `[DONE]` 结束标记，透传路径将连接 EOF 视为正常结束，不强制检查尾帧 `finishReason`* |

