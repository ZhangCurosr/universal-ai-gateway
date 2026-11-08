# 13 内核（Core）

> 代码位置：`internal/core`（任务卡 T10）。内核是**协议无关、尽量小、尽量不变**的部分。
> 适配器（会变的部分）见 [08_adapters.md](./08_adapters.md)。

## 1. 设计原则

| 编号 | 原则 |
|:--|:--|
| K-01 | **协议无关**：内核只通过 `protocol` 包的接口与适配器交互，只从 `protocol.Envelope` 了解请求。内核代码中不得出现任何协议名、请求体字段名（`"model"`、`"messages"`、`"max_tokens"` 等）或 `switch protocol` |
| K-02 | **不假设上游是大模型**：内核不得出现 token 专用逻辑。计费只认 `pricing.Usage`（任意单位）与价目表；转发只认 `Response.Kind`；一次尝试内有几次上游调用由适配器决定 |
| K-03 | **不导入适配器**：`internal/core` 不得导入 `internal/adapter/*`（ARCH-07，archtest 强制）。测试文件可以导入真实适配器，也可以定义测试专用协议（`core_test.go` 中的 `jobs`） |
| K-04 | **只做必要的 I/O**：读客户端请求体、调用 `Egress.Do`、写客户端响应；不访问数据库（ARCH-01）、不读写文件 |
| K-05 | **失败封闭**：任何无法确定的情况（未知服务、没有适配器、没有转换器、价目表不完整、任务固定丢失）都返回错误，不做猜测性降级 |
| K-06 | **小**：内核只包含 §2 的流程本身。可以放到适配器、调度器、账本里的逻辑，一律不放进内核 |

## 2. 处理流程（`Core.Handler(in, rt)`）

`httpapi` 为每个入站适配器的每个 `RouteCall` 端点调用一次 `Core.Handler(in, rt)`（spec/09 §1）。每个请求严格按以下顺序执行：

| 步骤 | 编号 | 动作 | 失败时（首字节前，用 `in.WriteError`） |
|:-:|:--|:--|:--|
| 1 | LIFE-03 | 按 `rt.Body` 读取请求体：`BodyBuffered` → `http.MaxBytesReader(w, r.Body, bodyLimit(rt, limits))` + `io.ReadAll`；`BodyStream` / `BodyNone` → 不读 | 超限 413 `request_too_large`；读取失败 400 |
| 2 | LIFE-07a | `env, err := in.Parse(r, rt, body)`；`BodyStream` 时随后设置 `env.BodyStream = http.MaxBytesReader(w, r.Body, bodyLimit(rt, limits))` | 原样返回 Parse 的错误 |
| 3 | LIFE-07b | 先算亲和键（步骤 6 提前），再 `route, err := c.resolveRoute(env, principal, affinityKey)`（已实现）：`env.Model` 不存在 → 400 `invalid_request`；不在白名单 → 403 `permission_denied`；空服务名的严格亲和请求按固定记录确定路由（CORE-11），未命中 → 404 `not_found` | 见左 |
| 4 | CORE-03 | 生成 `Call`（§3） | 400 |
| 5 | LIFE-09 | 预留：`u := reserveUsage(env, route)`，`amount, _ := pricing.Cost(route.Price, u)`，`ledger.Reserve`（LED-01：`amount <= 0` 按 1） | 402 / 503 |
| 6 | LIFE-10 | `AffinityKey(principal.ClientID, env.Header, env.Affinity)` | — |
| 7 | LIFE-11 | 尝试循环（spec/07 §6）：`Acquire`（`Strict = env.Affinity.Strict`）→ `route.Egress.Do(ctx, upstream, call)` | 见 spec/07 §6.1 |
| 8 | CORE-04 | 跨协议时 `resp, err = tr.Response(env, resp)`（Meter 与 Pins 仍取原始响应） | 502 `upstream_error` |
| 9 | LIFE-12/13 | 按 `resp.Kind` 转发（§4） | 事件流首字节后用 `in.StreamError` |
| 10 | CORE-08 | 对 `resp.Pins` 中每个值：`lease.Pin(AffinityKey(clientID, nil, AffinityHint{Source: AffinityPin, Value: pin}), pool.pin_ttl)` | — |
| 11 | LIFE-14~16 | 一个 `defer` 中：`lease.Finish(outcome)`、`Settle` 或 `Release`（§2.1）、`audit.Submit` | — |

| 编号 | 规则 |
|:--|:--|
| CORE-01 | `core.New` 时：每个模型所在池的 `protocol` 必须有已注册的 Egress，否则返回错误，进程拒绝启动（已实现） |
| CORE-02 | 内核所有面向客户端的错误都通过 `in`（本请求的入站适配器）输出；内核自己不拼任何错误 JSON |
| CORE-05 | `core.New` 时：每个模型的 `price` 必须为该池 Egress 的 `Units()` 中**每个**单位定价（免费写 `"0"`），否则返回错误（已实现）。运行时 `pricing.Cost` 返回未定价单位时（适配器违反 EG-01）记 `Error` 日志，该单位按 0 计 |
| CORE-06 | 请求体上限 `bodyLimit(rt, limits)`：缓冲 = `min(rt.MaxBodyBytes, max_request_body_bytes)`，流式 = `min(rt.MaxBodyBytes, max_stream_body_bytes)`，`rt.MaxBodyBytes == 0` 视为不额外限制（已实现） |
| CORE-07 | `call.BodyStream != nil` 时只尝试一次（流只能读一次），无论 `max_attempts` 是多少 |
| CORE-08 | Pins 在转发**结束后**读取，只在 Outcome = Success 时记录（流式适配器可以在转发过程中向 `resp.Pins` 追加，如 Responses 的 `response.created`） |
| CORE-11 | `env.Model` 为空且 `env.Affinity.Strict`：`Scheduler.PinnedPool(affinityKey)` 得到池，路由取该池中名称最小的服务（`poolRoutes`，`New` 时预先计算）；不检查白名单（固定记录按 clientID 隔离，只有创建者能命中）。这类请求的 `Estimate` 应全部为 0，Meter 返回不计费 |
| CORE-09 | `Do` 返回非 `*AttemptError` 的错误：不重试、`lease.Finish(Outcome{Kind: LocalError})`、`Release` 预留；`*apperr.Error` 原样给客户端，其他错误 → 500 `internal`（日志记录原因） |
| CORE-10 | `*AttemptError` 的分类修正：客户端 `r.Context()` 已取消 → `Canceled`；内核的超时定时器已触发 → `Timeout`（覆盖适配器给出的分类，因为适配器分不清是谁取消了 ctx） |

### 2.1 结算（`settledUsage`，已实现）

| `Meter.Usage()` 来源 | 结算用量 | `usage_source` |
|:--|:--|:--|
| `UsageUpstream` | 上游报告的用量（可以为空 = 不计费） | `upstream` |
| `UsageEstimated` | 适配器给出的单位用它的值，其余单位用预留用量补齐 | `estimated` |
| `UsageNone` | 预留用量 | `estimated` |

| 情况 | 账本调用 |
|:--|:--|
| 得到 `*Response`（无论之后转发是否完整） | `Settle`：`cost, _ := pricing.Cost(route.Price, 结算用量)`；`store.UsageEvent{Model, Op: env.Op, Usage: 结算用量, ...}` |
| 所有尝试都在得到 `*Response` 之前失败 | `Release` |
| 在 Reserve 之前失败 | 不需要 |

### 2.2 审计记录（`audit.Submit`）

每个进入 `Core.Handler` 的请求**恰好**提交一条审计记录（在 §2 第 11 步的 `defer` 中，无论成功、失败还是 panic 恢复前）。审计可以丢弃（队列满时 `Submit` 丢弃并计数），但内核不得因审计阻塞。

| 字段 | 取值 |
|:--|:--|
| `RequestID` | `obs.RequestIDFrom(ctx)` |
| `ClientID` | Principal 的 ClientID |
| `KeyID` | 最后一次 `Acquire` 成功的 Key ID；从未选中 Key 为 0 |
| `Model` | 路由的服务名 `route.Model.Name`；路由解析之前失败时取 `env.Model`（可能为空）；`Parse` 之前失败为空串 |
| `Op` | `rt.Op`（不依赖 Parse 是否成功） |
| `IngressProtocol` | `in.Protocol()` |
| `UpstreamProtocol` | `route.Upstream`；路由解析之前失败为空串 |
| `Status` | 写给客户端的 HTTP 状态码（取自 §4.5 的包装 `ResponseWriter`；流已开始后出错仍是已写出的 200）；客户端在写出任何字节前断开记 499 |
| `ErrorCode` | 返回给客户端的 `apperr.Code`（含流中通过 `StreamError` 告知的错误）；成功为空串 |
| `Outcome` | 最后一次尝试的 `OutcomeKind.String()`（转发结束后的最终 Outcome，见 §4）；从未尝试为空串 |
| `Attempts` | 调用 `Egress.Do` 的次数（0 表示在尝试之前就失败了） |
| `Stream` | `env.Stream`；`Parse` 之前失败为 false |
| `TTFBMillis` | 从进入 Handler 到**写出响应头**的毫秒数；未写出响应头为 -1 |
| `LatencyMillis` | 从进入 Handler 到 Handler 返回的毫秒数 |
| `CreatedAt` | 进入 Handler 时的 `Deps.Now()`（UTC） |

审计记录**不得**包含请求体、响应体、请求头、上游 Key 或客户端 Token（spec/02 §5）。

## 3. 生成上游调用（CORE-03、CORE-04）

```
入站协议 env.Protocol == 路由的上游协议 route.Upstream ?
  ├─ 是 → 透传：call, err := route.Egress.Prepare(env, route.Model.UpstreamModel)
  └─ 否 → tr, ok := registry.Translator(env.Protocol, route.Upstream)
            ├─ ok  → 转译：call, err := tr.Request(env, route.Model.UpstreamModel)
            └─ !ok → 400 invalid_request："model %q is served by protocol %q; translation from %q is not supported"
```

| 编号 | 规则 |
|:--|:--|
| CORE-03 | 找不到转换器时返回 400，**不得**尝试「直接透传过去碰碰运气」 |
| CORE-04 | 转译路径：得到 `resp` 后调用 `tr.Response(env, resp)` 再转发。用量、Fault 与 Pins 始终取自**原始** `resp`（`resp.Meter`、`resp.Pins`），不取转换后的值 |

## 4. 转发

先写响应头：`resp.Header` 中的全部头 + `X-Request-Id`；状态码 `resp.Status`（0 视为 200）。内核另外**删除** `Set-Cookie`、`Connection`、`Transfer-Encoding` 等逐跳头（防御适配器疏漏）。

### 4.1 `ResponseBuffered`

写 `resp.Body`。Outcome = `Success`（`Meter.Fault` 为 true 时取该 Outcome）。

### 4.2 `ResponseEvents`

1. **先读第一个事件再写响应头**：`Next()` 在第一个事件之前返回 `*AttemptError` 时仍可重试（RTY-01，此时尚未写出任何字节）。
2. 写响应头（`Content-Type: text/event-stream; charset=utf-8`、`Cache-Control: no-cache` 由适配器给出），然后用 `sse.Writer` 逐个写事件（每个事件后 Flush）。
3. `Next()` 返回 `io.EOF` → 正常结束；Outcome = `Success`，`Meter.Fault` 为 true 时取该 Outcome。
4. 结束后 `Events.Close()`。

### 4.3 `ResponseRaw`

写响应头后，以 32 KiB 缓冲循环 `Read` / `Write` / `Flush`（不用 `io.Copy`，因为每块都要 Flush 并重置空闲定时器）。`io.EOF` → `Success`。结束后 `Raw.Close()`。

### 4.4 流中异常（事件流与字节流）

| 编号 | 情况 | 处理 |
|:--|:--|:--|
| STR-01 | 两次读取之间超过 `stream_idle` | 取消上游 ctx；事件流写 `in.StreamError(apperr.New(CodeTimeout, "upstream stream idle timeout"))`；字节流直接结束响应（无法在字节流中插入错误）；Outcome = `Timeout` |
| STR-02 | 读上游出错且不是客户端取消 | 事件流写 `in.StreamError(apperr.New(CodeUpstreamError, "upstream stream interrupted"))`；字节流直接中止连接（`panic(http.ErrAbortHandler)`，让客户端看到不完整的响应而不是误以为完整）；Outcome = `NetworkError` |
| STR-03 | 客户端断开（`r.Context()` 取消） | 停止读写，关闭上游；Outcome = `Canceled` |
| STR-04 | 写客户端失败 | 同 STR-03 |

流错误事件的消息文本固定为上表两种，**不包含**上游原始错误内容。

### 4.5 「是否已写出首字节」的判断

用包装的 `http.ResponseWriter` 记录 `WriteHeader` 是否已被调用；**只有**未写出时才允许重试（RTY-01）或调用 `in.WriteError`。

## 5. 上游超时

```
headerTimeout = response_header_base + response_header_per_mib × ceil(请求体字节 / 1 MiB)   // headerTimeout()，已实现
```

请求体字节：缓冲时取 `len(body)`，流式时取 `r.ContentLength`（未知为 0）。

- 实现：`ctx, cancel := context.WithCancel(r.Context())` + `time.AfterFunc(headerTimeout, cancel)`，把 `ctx` 传给 `Do`。
- `Do` 返回后 `Stop()` 该定时器：此后缓冲响应已完整；事件流与字节流改用 `stream_idle` 定时器（每读到一个事件或一块数据 `Reset`）。
- 缓冲响应的整个 `Do` 另受 `non_stream_total` 约束（取两者中先到者）。
- 异步任务的「同步封装」在 `Do` 内轮询，因此受 `headerTimeout` 约束——这类池要把 `response_header_base` 配成分钟级（spec/08 §8.2）。

## 6. 上游 HTTP 客户端（`cmd/gateway` 创建，全局共享）

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

`http.Client{Transport: t, Timeout: 0}`——**不设** `Client.Timeout`（会截断长流），超时全部由 §5 控制。`connectTimeout` 取所有池 `timeouts.connect` 的最大值。内核把它作为 `protocol.Upstream.Client` 传给 `Egress.Do`，`MaxBufferedBytes` 固定为 64 MiB。

## 7. 冻结接口与变更流程

以下导出 API 是内核与外界的全部契约，**实现任务中不得修改**：

| 包 | 冻结的导出 API |
|:--|:--|
| `internal/protocol` | 全部（`Envelope`、`AffinityHint`、`Route`、`BodyMode`、`ModelInfo`、`Ingress`、`Egress`、`Call`、`Upstream`、`Doer`、`Response`、`EventStream`、`Meter`、`UsageSource`、`AttemptError`、`Translator`、`ToolSource`、`Tool`、`ToolRequest`、`ToolResult`、`ToolContent`、`Outcome*`、`Registry`（含 `Tools`、`Tool`）、`Classify*`、`httpx.go` 工具） |
| `internal/pricing` | `Usage`、`Price`、`Rate`、单位常量、`ParseRate`、`Cost`、`ReserveUsage`、`Estimate*`、`ValidUnit` |
| `internal/core` | `New`、`Deps`、`Core.Handler`、`Core.Models`、`Core.Route`、`Route`、`AffinityKey`、`SessionHeader` |
| `internal/mcp` | `Server`、`New`、`Deps`、`WriteHTTPError`、`Client`、`Bridge`、`SupportedVersions`、JSON-RPC 类型 |

变更流程：
1. 在交付说明中写清「哪个签名、为什么现有契约做不到、对所有现有适配器的影响」。
2. 架构负责人在 `06_architecture_review_and_revisions.md` 新增一条 ADR 并批准后，才能修改。
3. 修改契约时必须同时更新：本章、spec/08、`adaptertest` 套件、全部现有适配器。

判断标准：**新增协议、新增厂商、新增服务（包括非大模型 API）都不应需要变更契约。** spec/08 §8 的范例是检验标准：搜索 API（多端点、按次计费）与文档解析 API（文件上传、异步任务、结果下载）都已用临时实现跑通一致性套件。只有当一类全新能力（例如 WebSocket 双向流、非 HTTP 传输）出现时才考虑扩展契约，而且优先以「新增可选接口」的方式扩展（适配器可以选择实现，内核用类型断言检测），而不是修改已有接口。
