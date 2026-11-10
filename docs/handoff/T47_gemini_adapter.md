# T47 Gemini 协议适配器（同协议透传，M3）

- **类型**：规格 + 实现
- **前置**：无（只依赖已冻结的契约与一致性套件）
- **需要用户审批**：**是**。spec/01 规定 M3 中除指标端点外的规格在 M2 完成后补充。本卡的阶段 A 就是补这部分规格，批准后才能写代码。
- **后续**：[T48](T48_gemini_translate.md)（Gemini 与其他协议互相转译）依赖本卡的协议名、操作名与 Params 约定。
- **必读**：
  - [`spec/08_adapters.md`](../../spec/08_adapters.md) 全章，尤其是 §2.1、§3.1 的通用规则和 §6 扩展指南
  - [`spec/05_security_auth.md`](../../spec/05_security_auth.md) AUTH-01 ~ AUTH-04（`x-goog-api-key` 已经是客户端凭据的来源之一；**不读取查询参数**）
  - [`internal/protocol/`](../../internal/protocol/) 全部文件（冻结契约）
  - [`internal/adapter/openai/`](../../internal/adapter/openai/)：结构照抄这个包（入站 + 出站、Meter、一致性测试）
  - [`internal/adapter/adaptertest/`](../../internal/adapter/adaptertest/)：一致性套件
  - [`cmd/gateway/adapters.go`](../../cmd/gateway/adapters.go)、[`internal/archtest/archtest_test.go`](../../internal/archtest/archtest_test.go)
  - Gemini API 官方文档：`generateContent`、`streamGenerateContent`、`countTokens`、`models.list`、错误格式、用量字段（`usageMetadata`）。**草案里的字段名都要在阶段 A 对照当时的官方文档核对一遍**，有出入的列在审批请求里

## 1. 背景与目标

让说 Gemini API（Google AI Studio 的 `generativelanguage.googleapis.com`）的客户端通过网关使用 Gemini 上游：换 Key、计费、限流、调度、审计和其他协议一样。
典型客户端：`google-genai` SDK（Python / JS）、Gemini CLI（用 API Key 模式，并把 base URL 指向网关）。

本卡只做**同协议透传**（入站 gemini → 上游 gemini）。跨协议转译是 T48。

**明确不做**：

- Vertex AI（`aiplatform.googleapis.com`）：鉴权是 OAuth / 服务账号，URL 结构也不同，以后需要再单独开卡。
- `embedContent`、`batchEmbedContents`、文件上传（Files API）、`cachedContents` 管理、Live API（WebSocket）、Batch API。
- 按提示词长度分档的价格（例如 Gemini 2.5 Pro 超过 200k 输入 token 时单价更高）：计量单位无法表达，部署文档里说明按哪一档配置。

## 2. 关键设计：模型名和方法都在路径里

Gemini 的调用端点是 `POST /v1beta/models/{model}:{method}`，例如 `/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse`。

- `http.ServeMux` 的通配段必须占满整个路径段，`{model}:generateContent` 这种写法不合法。所以只能声明**一个**路由 `POST /v1beta/models/{target}`，由 `Parse` 按**最后一个** `:` 把 `target` 拆成模型名和方法名。
- 同一个路由只能有一个 `Op`（IN-G01 要求 `Envelope.Op = rt.Op`，一致性套件会检查）。因此三种方法共用操作名 `models.call`，具体方法放在 `Envelope.Params["method"]`。
- 模型名来自路径，请求体里没有 `model` 字段，所以透传时**请求体完全不改写**，上游模型名只写进上游 URL。

## 3. 规格草案（阶段 A：写进 spec 后提交审批）

> 建议在 `spec/08_adapters.md` 末尾新增「§10 Gemini API（`gemini`，任务卡 T47）」，结构仿照 §9。
> 同时修改 spec/01 §2 的 M3 小节（写明 Gemini 的范围和不做的部分），并在 `spec/11_tasks.md` 的交接表中登记本卡。标了「待定」的地方请用户回答。

### 3.1 包与常量

| 项 | 规定 |
|:--|:--|
| 包 | `internal/adapter/gemini` |
| 协议名 | `const Protocol protocol.Protocol = "gemini"` |
| 操作名 | `const OpModelsCall = "models.call"` |
| Params 键 | `method`：`generateContent` / `streamGenerateContent` / `countTokens` |

### 3.2 入站

| 编号 | 规则 |
|:--|:--|
| IN-G-01 | 路由：`POST /v1beta/models/{target}`（Call，Op `models.call`，`BodyBuffered`）；`GET /v1beta/models`（Catalog）。*待定 1：要不要同时支持 `/v1/...`？建议不支持：SDK 与 Gemini CLI 默认用 `v1beta`，而且 `Op` 必须唯一，支持两个版本就要多一个操作名* |
| IN-G-02 | `target` 按**最后一个** `:` 拆分；拆不出来，或模型名为空 → `CodeInvalidRequest`，消息 `path must be /v1beta/models/{model}:{method}` |
| IN-G-03 | 方法只接受 `generateContent`、`streamGenerateContent`、`countTokens`；其他方法 → `CodeInvalidRequest`，消息 `method "<m>" is not supported` |
| IN-G-04 | `streamGenerateContent` 必须带查询参数 `alt=sse`，否则 `CodeInvalidRequest`，消息 `streamGenerateContent requires alt=sse`。*待定 2：不带 `alt=sse` 时上游返回的是逐步输出的 JSON 数组，不是 SSE，内核的流式路径无法处理。建议拒绝；官方 SDK 与 Gemini CLI 都带 `alt=sse`* |
| IN-G-05 | `Envelope.Model` = 拆出的模型名（不带 `models/` 前缀）；`Stream` = 方法是 `streamGenerateContent`；`Params = {"method": <方法>}` |
| IN-G-06 | 请求体不是 JSON 对象 → `CodeInvalidRequest`（同 IN-L01）。不读取请求体里的 `model`、`stream` |
| IN-G-07 | `Estimate`：`generateContent` / `streamGenerateContent`：`input_tokens = EstimateInputTokens(len(body))`；`generationConfig.maxOutputTokens` 存在且为正整数时 `output_tokens` = 该值，存在但不是正整数 → `CodeInvalidRequest`。`countTokens`：三个单位全部显式为 0（不计费，IN-G04） |
| IN-G-08 | 亲和线索：Gemini 没有用户标识字段，用前缀线索：`systemInstruction` + `tools` + `contents[0]`（AFH-02 的拼接规则）。`countTokens` 不设亲和 |
| IN-G-09 | 查询串里**只有** `alt` 有意义，其他查询参数（包括 `key=`）一律不读、不转发。客户端用 `?key=<网关 Token>` 鉴权会得到 401（AUTH-03），部署文档要说明「请把网关 Token 放在 `x-goog-api-key` 头里」 |
| IN-G-10 | `Claims` 恒为 false（路由不和其他协议冲突） |

**错误格式**（`WriteError`，模仿 Google API 的错误体）：

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

**流错误事件**（`StreamError`）：`Name: ""`，`Data` 与上面的错误体相同。Gemini 的 SSE 没有专门的错误事件名，SDK 会把带 `error` 的 chunk 当错误抛出。*执行者在阶段 A 用官方 SDK 源码确认这一点。*

**目录**（`WriteCatalog`）：

```json
{"models":[{"name":"models/<id>","displayName":"<id>","supportedGenerationMethods":["generateContent","streamGenerateContent","countTokens"]}]}
```

不输出 `nextPageToken`。`GET /v1beta/models/{model}`（单个模型的信息）不提供：ServeMux 会按方法不匹配返回 405，部署文档说明。

### 3.3 出站

| 编号 | 规则 |
|:--|:--|
| PT-G-01 | `Prepare`：请求体**原样**作为上游请求体，不做任何改写（请求体里没有模型名）；`Call.Model = upstreamModel`，`Call.Stream = env.Stream`，`Call.Params = env.Params` |
| PT-G-02 | 请求体不是 JSON 对象 → `CodeInvalidRequest`（PT-02） |
| EG-G-01 | 上游方法：`Params["method"]` 为 `countTokens` 时用 `countTokens`；否则 `Call.Stream` 为 true 用 `streamGenerateContent`，为 false 用 `generateContent`。这样 T48 的转换器只要设置 `Call.Stream`，不必知道方法名 |
| EG-G-02 | 上游 URL：`protocol.JoinURL(base_url, "/v1beta/models/" + url.PathEscape(Call.Model) + ":" + 方法)`；流式时追加 `?alt=sse`。不追加其他查询参数 |
| HDR-G-01 | `POST`；`Content-Type: application/json`；`Accept: application/json, text/event-stream`；鉴权头 `x-goog-api-key: <secret>`。**不**用查询参数 `key=` 传上游 Key（会出现在代理与访问日志里） |
| HDR-G-02 | 不复制任何客户端头（HDR-02）；`User-Agent` 同 HDR-05 |
| EG-G-03 | 响应 `Content-Type` 以 `text/event-stream` 开头时返回 `ResponseEvents`，否则 `ResponseBuffered`（同 §3.3 的骨架） |
| EG-G-04 | 上游 429 的错误体里有 `error.details[]` 中 `@type` 以 `RetryInfo` 结尾的项时，把其中的 `retryDelay`（如 `"30s"`，按 Go 时长解析）作为 `AttemptError.Outcome.RetryAfter`，取它与 `ClassifyHTTP` 结果中较大的值。解析失败就忽略，不报错 |
| EG-G-05 | `Units() = [input_tokens, output_tokens, cache_read_tokens]` |

**用量**（`Meter`）：

| 编号 | 规则 |
|:--|:--|
| USE-G-01 | 来源：非流式响应体顶层 `usageMetadata`；流式时每个 chunk 都可能带 `usageMetadata`，是累计值，后出现的覆盖先出现的 |
| USE-G-02 | `cached = cachedContentTokenCount`（不存在为 0）；`input_tokens = max(promptTokenCount − cached, 0) + toolUsePromptTokenCount`；`cache_read_tokens = cached`；`output_tokens = candidatesTokenCount + thoughtsTokenCount`（思考 token 按输出计费）。不存在的字段按 0 |
| USE-G-03 | `candidates[*].content.parts[*].text` 的字节数累计为 `textBytes`（USE-07 的估算依据），`thought: true` 的部分也计入 |
| USE-G-04 | `Usage()`：`countTokens` → `(nil, UsageUpstream)`（不计费）；观察到 `promptTokenCount` → `UsageUpstream`；只有文本 → 估算输出，`UsageEstimated`；都没有 → `UsageNone` |

**事件过滤与流中错误**：

| 编号 | 规则 |
|:--|:--|
| FWD-G-01 | 透传时全部事件原样转发 |
| STR-G-01 | 带顶层 `error` 对象的 chunk 记为 Fault：`error.code == 429` 或 `status == "RESOURCE_EXHAUSTED"` → `RateLimited`；`503` / `UNAVAILABLE` → `Overloaded`；其他 → `ServerError` |

*待定 3：Gemini 的 SSE 没有结束标记（没有 `[DONE]`），上游正常结束就是连接关闭。透传时内核把 EOF 当作正常结束，这和现在的 openai_chat 一样，不需要特殊处理。建议**不**在透传路径上检查「最后一个 chunk 是否带 `finishReason`」，因为同协议透传应当尽量不解释内容；T48 的 Decoder 会做这个检查。*

### 3.4 分层与注册

- 在 `internal/archtest/archtest_test.go` 的 `layers` 中加 `"internal/adapter/gemini": 2`。这是登记新包，不是修改规则，但要在审批请求里写明（spec/08 §6 第 6 步）。
- 在 `cmd/gateway/adapters.go` 的 `adapterRegistrars` 中加 `gemini.Register`。

## 4. 实现要求（阶段 B：用户批准后）

- `register.go`、`ingress.go`、`egress.go`、`meter.go`（可以合并进 `egress.go`），结构照抄 `internal/adapter/openai`。
- 只读顶层和少数嵌套字段，用 `jsonx.Get*`；**禁止**把请求体整体反序列化为结构体（IN-G06）。响应侧的 Meter 可以解析 `usageMetadata` 子对象。
- 读取上游错误体时用 `AttemptError.Body`（已经截断），不要自己再读一遍响应体。
- `gateway.example.yaml` 加一个注释掉的 Gemini 池示例：`protocol: gemini`、`base_url: https://generativelanguage.googleapis.com`，模型价目为三个单位定价。
- `docs/DEPLOYMENT.md`（或用户部署指南）加一节：Gemini 客户端怎么指向网关（SDK 的 `base_url` / `http_options`，Gemini CLI 的环境变量），Token 放 `x-goog-api-key` 头，不支持 `?key=`，以及分档价格的配置建议。

## 5. 涉及文件

- spec：`spec/08_adapters.md`（新增 §10）、`spec/01_scope_and_architecture.md`（M3 小节）、`spec/11_tasks.md`（登记本卡与状态）
- 新增：`internal/adapter/gemini/*.go`、`internal/adapter/gemini/testdata/**`
- 修改：`cmd/gateway/adapters.go`（加一行）、`internal/archtest/archtest_test.go`（只在 `layers` 中登记）、`gateway.example.yaml`、`docs/DEPLOYMENT.md`
- 可选：`cmd/gateway/e2e_test.go` 新增一个独立的 Gemini 端到端用例（不改现有用例）

## 6. 测试要求

1. **一致性套件**：`conformance_test.go` 调用 `RunIngress` 和 `RunEgress`，全部通过。入站样例覆盖三种方法；非法样例至少包含：路径没有 `:`、不支持的方法、`streamGenerateContent` 不带 `alt=sse`、请求体不是对象、`maxOutputTokens` 为 0 或字符串。
2. **路径拆分**：模型名里带 `.`、`-`（如 `gemini-2.5-flash-lite`）；`target` 里有多个 `:` 时按最后一个拆。
3. **出站 URL 与头**：非流式、流式（带 `?alt=sse`）、`countTokens` 三种上游 URL 都正确；上游请求带 `x-goog-api-key`，不带客户端的 `x-goog-api-key` 和查询参数 `key`；`base_url` 带路径前缀时保留前缀。
4. **用量**：USE-G-02 的每一项（含 `cachedContentTokenCount`、`thoughtsTokenCount`、`toolUsePromptTokenCount`）；流式累计值后到覆盖先到；流中断时 `UsageEstimated`；`countTokens` 不计费。
5. **重试时间**：429 错误体带 `RetryInfo.retryDelay` 时 `RetryAfter` 正确；错误体格式不对时不报错。
6. **流中错误**：带 `error` 的 chunk 被记为 Fault，分类正确。
7. **凭据**：测试日志和错误消息中不出现上游 Key 明文。

## 7. 验收标准

- [ ] 阶段 A 的规格已获用户批准，3 个待定项都有答案。
- [ ] §6 的测试全部通过；`go run ./tools/check` 全绿。
- [ ] 没有新增 `go.mod` 依赖；没有修改 `internal/core`、`internal/protocol`、`internal/adapter/adaptertest`；archtest 只是登记了新包。
- [ ] 用真实客户端验证（至少一种）：`google-genai` SDK 或 Gemini CLI 指向本地网关，非流式和流式各跑通一次。交付说明里贴出命令和结果（**不要贴出任何 Key 或 Token**）。没有真实 Gemini Key 时，用假上游跑通，并在交付说明里写明「未做真实上游验证」。
- [ ] 部署文档与配置示例已更新。

## 8. 建议提交拆分

1. 阶段 A：`docs(spec): T47 新增 spec/08 §10 Gemini 适配器（待审批）`
2. `feat(gemini): T47 Gemini 入站与出站适配器`
3. `feat(gateway): T47 注册 Gemini 适配器`
4. `docs(deploy): T47 Gemini 客户端接入说明`
