# T48 Gemini 与其他协议互相转译（M3）

- **类型**：规格 + 实现
- **前置**：T47 的阶段 A 已获批准（本卡要用到它定下的协议名 `gemini`、操作名 `models.call` 和 `Params["method"]`）。T47 的实现最好也已合入，这样才能做端到端测试。
- **需要用户审批**：**是**（§3 规格草案）。
- **当前状态（2026-11-10）**：前置已满足。T47 已合入 main（`d52db25`），它的 3 个待定项按默认方案确认：只支持 `/v1beta`；`streamGenerateContent` 必须带 `alt=sse`；流以 EOF 视为正常结束。规格见 `spec/08` §10，实现在 `internal/adapter/gemini/`。
- **审批门再强调一次**：阶段 A 只提交 spec 修改，提交信息写「待审批」，然后**停下等用户回复**，不要接着写实现。T47 曾在阶段 A 未获批准时直接完成了实现，这次再这样做，整张卡退回。
- **不要与其他改 `internal/translate` 的卡同时进行。**
- **必读**：
  - [`spec/10_ir_translation.md`](../../spec/10_ir_translation.md) 全章，尤其是 §1 原则、§6、§8.1、§9（Decoder / Encoder 分工和流式 IR 的不变量）
  - [`docs/handoff/reports/T43_delivery.md`](reports/T43_delivery.md) §5：新增一个协议要写哪些组件
  - [`internal/ir/types.go`](../../internal/ir/types.go)；`internal/translate/` 中的 `decode*.go`、`encode*.go`、`register.go`、`protocols.go`、`matrix_test.go`
  - T47 的规格（`spec/08` §10）
  - Gemini API 官方文档：`Content`、`Part`、`FunctionDeclaration`、`FunctionCall`、`FunctionResponse`、`GenerationConfig`、`ToolConfig`、`Candidate.finishReason`、思考签名（thought signatures）

## 1. 背景与目标

T47 之后，Gemini 只能和 Gemini 互通。本卡补上和现有三种协议之间的转译，方向与现有转换器对称：

| 转换器 | 入站 → 上游 | 场景 | 组成 |
|:--|:--|:--|:--|
| `GeminiToAnthropic` | gemini → anthropic | Gemini CLI / genai SDK 使用 Claude | Gemini Lower + Anthropic Synth；Anthropic Decoder + Gemini Encoder |
| `GeminiToChat` | gemini → openai_chat | Gemini 客户端使用 DeepSeek 等 | Gemini Lower + Chat Synth；Chat Decoder + Gemini Encoder |
| `AnthropicToGemini` | anthropic → gemini | Claude Code 使用 Gemini | Anthropic Lower + Gemini Synth；Gemini Decoder + Anthropic Encoder |
| `ChatToGemini` | openai_chat → gemini | Chat 客户端使用 Gemini | Chat Lower + Gemini Synth；Gemini Decoder + Chat Encoder |
| `ResponsesToGemini` | openai_responses → gemini | Codex CLI 使用 Gemini | Responses Lower + Gemini Synth；Gemini Decoder + Responses Encoder |

新写 4 个组件：`gemini_lower.go`、`gemini_synth.go`、`decode_gemini.go`、`encode_gemini.go`。其余全部复用。
**不改 `internal/core`、`internal/protocol`，也不改 `internal/ir` 的导出类型**（见 §2）。

本卡另外包含两项维护性改动（来自 2026-11-09 的架构评估），都要在接入 Gemini **之前**完成：

1. **转换器改为表驱动注册**（§2.4）：现在每个「入站 → 上游」组合都手写一个转换器类型，接入 Gemini 后会从 4 个变成 9 个，而且几乎一模一样。先改成「每个协议登记一次组件，由通用类型组合」，再加 Gemini。
2. **计费用量与客户端可见用量的一致性测试**（§2.5）：同一份上游用量现在被解析两遍，出站适配器的 Meter 算账单，转译层的 Decoder 算客户端看到的数字，两者没有测试互相核对。

## 2. 关键设计

### 2.1 工具调用 ID

Gemini 的 `functionCall.id` 是可选的，多数响应里没有；`functionResponse` 按 `name`（有 `id` 时按 `id`）对应到调用。IR 和另外三种协议都要求每个工具调用有 ID。

- **Gemini 上游 → 其他客户端**（Decoder）：`functionCall` 没有 `id` 时由网关签发 `call_` + 24 位小写 base32（`crypto/rand`，复用 `ids.go`）。客户端之后在 `tool_result` 里带回这个 ID。
- **其他客户端 → Gemini 上游**（Synth）：`ToolResultPart.ToolCallID` 要换回函数名。在**同一个请求的历史**里找 ID 相同的 `ToolCallPart`，取它的 `Name`；找不到 → 400。请求总是带完整历史，所以不需要网关保存状态。
- **Gemini 客户端 → 其他上游**（Lower）：`functionCall` 没有 `id` 时生成**确定性**的 ID `gemini_call_<n>`（`n` 是该调用在整个 `contents` 中的序号，从 0 开始）；`functionResponse` 有 `id` 时按 `id` 对应，没有时按 `name` 对应到**紧邻的上一条 model 消息**中第一个还没被对应过的同名调用。对应不上 → 400。

### 2.2 思考签名

Gemini 3 系列在多轮工具调用中要求回传 `thoughtSignature`，缺失时返回 400。从 Anthropic / Chat / Responses 客户端来的历史里不会有 Gemini 的签名（§1 第 3 条：签名只回传给签发它的厂商）。

Google 的文档给出了一个占位签名 `skip_thought_signature_validator`，用于「工具调用不是由该模型生成的」这种情况（例如从别的模型迁移过来的历史），同时说明它会降低模型表现，只应作为最后手段。LiteLLM、Vercel AI SDK 都在跨模型时自动注入它。

*待定 1：Gemini Synth 是否注入占位签名？*

- a. 注入（建议）：每条 model 消息中**第一个** `functionCall` 部分加 `"thoughtSignature": "skip_thought_signature_validator"`。代价是 Gemini 3 在这类历史上表现可能变差。
- b. 不注入：Claude Code / Codex CLI 用 Gemini 3 做多轮工具调用时会直接 400。

选 a 时，执行者要在阶段 A 对照当时的官方文档确认占位值和位置，并用真实上游验证 Gemini 2.5 也接受它（不接受就只对 `gemini-3` 前缀的上游模型注入，并写成规则）。

### 2.3 Gemini 客户端的流式工具调用

Gemini 的流里，一个 `functionCall` 总是**完整地**出现在一个 chunk 里（`args` 是 JSON 对象，没有增量）。而 IR 的工具参数是分片的（`EventToolArgs`）。

- **Decoder**（Gemini 上游）：每个 `functionCall` 输出 `EventToolStart` → 一个 `EventToolArgs`（`args` 的紧凑 JSON 原样输出）→ `EventToolEnd`。
- **Encoder**（Gemini 客户端）：把同一个工具调用的 `EventToolArgs` 拼起来，在 `EventToolEnd`（或下一个 `EventToolStart`、`EventStop`、`EventEnd`）时输出一个只含该 `functionCall` 的 chunk。拼出来的不是 JSON 对象 → 流错误。拼接缓冲上限 1 MiB，超过 → 流错误（禁止无界缓冲）。

### 2.4 转换器表驱动注册（先做，单独提交）

现状：[`register.go`](../../internal/translate/register.go) 里 `ChatToAnthropic`、`ResponsesToAnthropic`、`AnthropicToChat`、`ResponsesToChat` 四个类型各自写一遍「检查 `env.Op` → Lower → 设置模型 → Synth → 组装 `Call`」和「按 `resp.Kind` 选 Decoder + Encoder」。

目标：每个协议**登记一次**自己能提供的组件；转换器由一个通用的组合类型生成。草案（执行者可以调整命名，但结构要等价）：

```go
// 一个协议作为「客户端协议」时提供的组件。
type clientSide struct {
	proto   protocol.Protocol
	accepts func(env *protocol.Envelope) error // 检查 Op / Params，不支持时返回 CodeInvalidRequest
	lower   func(env *protocol.Envelope, opts Options, from, to string) (*ir.Request, error)
	encoder func(now func() time.Time, newID idGen) encoder
}

// 一个协议作为「上游协议」时提供的组件。
type upstreamSide struct {
	proto   protocol.Protocol
	op      string // Call.Op
	synth   func(req *ir.Request, opts Options) ([]byte, error)
	decoder decoder
}

// composed 实现 protocol.Translator：Request = accepts → lower → synth；Response = decoder + encoder。
type composed struct { client clientSide; up upstreamSide; opts Options; now func() time.Time; newID idGen }
```

`Registrar` 按一张**显式的组合表**注册（不是所有组合的笛卡尔积，哪些方向开放仍然由表决定）：

```go
var pairs = [][2]protocol.Protocol{
	{protoOpenAIChat, protoAnthropic}, {protoOpenAIResponses, protoAnthropic},
	{protoAnthropic, protoOpenAIChat}, {protoOpenAIResponses, protoOpenAIChat},
	// T48 新增 5 行
}
```

要求：

- 这是**纯重构**：4 个现有方向的行为逐字节不变；现有测试的**断言一律不改**，现有黄金文件一律不改。
- 现有的 4 个导出类型只在本包的测试里用到。可以保留为包装 `composed` 的薄类型，也可以删除，只把测试里的构造调用改成新的写法（与 T43 的 `body_helpers_test.go` 做法相同）。两种做法都要在交付说明里写明。
- `Now` / `NewID` 的测试注入能力保留。
- 新增 `TestRegistrarPairs`：注册后的转换器集合与 `pairs` 表完全一致；每个组合的 `Request` 对不支持的 `Op` 返回 `CodeInvalidRequest`。
- 写进 spec/10 §9.1（阶段 A 的规格里一起提交审批）：「新增协议时登记组件，并在组合表中加行；不再为每个方向手写转换器类型」。

接入 Gemini 后，`register.go` 只需要多出 Gemini 的两个 side 和组合表里的 5 行。

### 2.5 计费用量与客户端可见用量一致（新增测试）

同一份上游用量现在有两个独立的解析器：

| 用途 | 解析器 | 位置 |
|:--|:--|:--|
| 计费（写进 `usage_events`） | 出站适配器的 `meter` | `internal/adapter/<name>/egress.go` |
| 客户端看到的 `usage` | 转译层的 Decoder | `internal/translate/decode_*.go` |

分层规则不允许两者互相导入，所以不合并代码，改为用测试锁住两者一致：

- 新增 `cmd/gateway/usage_consistency_test.go`（`cmd/gateway` 是唯一可以同时导入具体适配器和 `translate` 的包）。
- 对每个已注册的转换器方向，取该方向**上游协议**的黄金输入（`internal/translate/testdata` 中的上游响应体与流，含带缓存用量的用例），分别：
  1. 交给该上游协议的 Egress（用假上游返回这份数据），读出 `Meter.Usage()`；
  2. 经转换器的 `Response` 转成客户端协议，按**客户端协议**的口径解析出 `usage`，再换算成计量单位（例如 Chat 客户端：`input_tokens = prompt_tokens − cached_tokens`）。
- 断言两者的每个单位都相等。客户端协议本来就不能表达的单位（例如 Chat 客户端没有缓存写入）在断言里明确列出，并加注释说明原因。
- Gemini 方向在 T48 的组件完成后加入同一个测试；以后每新增一个协议，这个测试都要覆盖它的所有方向。

## 3. 规格草案（阶段 A：写进 spec 后提交审批）

> 建议在 `spec/10_ir_translation.md` 末尾新增「§10 Gemini 的转译（T48）」，结构仿照 §6、§8。
> 同时在 `spec/11_tasks.md` 的交接表中登记本卡。标了「待定」的地方请用户回答。
> **所有 Gemini 字段名在阶段 A 对照官方文档核对**，有出入的列在审批请求里。

### 3.1 Gemini 请求 → IR（Lower）

只处理 `Params["method"]` 为 `generateContent` / `streamGenerateContent` 的请求；`countTokens` → 400 `countTokens is not supported for model "<name>"`（*待定 2*，见 §3.6）。
`Stream` 取 `Envelope.Stream`；`Model` 取 `Envelope.Model`（请求体里没有模型名）。

| Gemini 字段 | IR | 说明 |
|:--|:--|:--|
| `contents[]` | `Messages` | 至少 1 条，否则 400 |
| `contents[].role` `user`（缺省也视为 `user`）/ `model` | `RoleUser` / `RoleAssistant` | 旧版的 `function` 角色视为 `user`；其他值 → 400 |
| part `text`（没有 `thought` 或 `thought: false`） | `TextPart` | |
| part `text` 且 `thought: true`（仅 model） | `ThinkingPart{Vendor: gemini, Text, Signature: thoughtSignature}` | 合成到其他厂商时按 §1 第 3 条丢弃 |
| part 上的 `thoughtSignature`（非思考部分） | 丢弃 | 只能回传给 Gemini |
| part `inlineData`，`mimeType` 以 `image/` 开头 | `ImagePart{MediaType, Data}` | base64 非法 → 400 |
| part `inlineData` 其他类型（PDF、音频、视频） | — | 400（`inlineData mimeType "<t>" is not supported for model "<name>"`） |
| part `fileData` | — | 400（Files API 的 URI 在其他厂商不可用） |
| part `functionCall`（仅 model） | `ToolCallPart{ID: §2.1, Name, Arguments: args}` | `args` 缺失 → `{}`；不是对象 → 400 |
| part `functionResponse`（仅 user / function） | `ToolResultPart{ToolCallID: §2.1, Content: [TextPart{response 的紧凑 JSON}]}` | `IsError` 恒为 false（错误信息已在 JSON 里）；带 `parts`（媒体）→ 400 |
| part `executableCode`、`codeExecutionResult`、`videoMetadata` 等 | — | 400（指出 part 类型） |
| `systemInstruction.parts[].text` | `System`（每个一个 TextPart） | 其他 part 类型 → 400 |
| `tools[].functionDeclarations[]` | `Tool{name, description, InputSchema}` | `parametersJsonSchema` 原样作为 `InputSchema`；`parameters`（OpenAPI 风格）按下面的规则转换；都没有 → `{"type":"object","properties":{}}` |
| `tools[]` 中的 `googleSearch`、`codeExecution`、`urlContext`、`googleMaps`、`fileSearch` 等内置工具 | — | 400（`built-in tool "<名>" is not supported for model "<name>"`） |
| `toolConfig.functionCallingConfig.mode` `AUTO` / `ANY` / `NONE` | `auto` / `any` / `none` | `ANY` 且 `allowedFunctionNames` 恰好 1 个 → `tool` + 该名字；多于 1 个 → 400；其他 mode（如 `VALIDATED`）→ 400 |
| `generationConfig.maxOutputTokens` | `MaxTokens` | |
| `generationConfig.temperature`、`topP` | 同名 | |
| `generationConfig.stopSequences` | `StopSequences` | |
| `generationConfig.candidateCount` > 1 | — | 400 |
| `generationConfig.topK` | 丢弃 | 与 §6.1 `top_k` 一致（T42 根据 T41 实测改为丢弃，见 §6.5 第 4 条） |
| `generationConfig.seed`、`presencePenalty`、`frequencyPenalty`、`responseLogprobs`、`logprobs` | — | 400（改变采样语义或输出形状，按 §1 第 2 条不默默丢弃）。*执行者用 T41 的方法检查 Gemini CLI 与 genai SDK 默认会不会发送其中某个字段；会发送的，在审批请求里提出改为丢弃* |
| `generationConfig.responseMimeType` 不是 `text/plain`、`responseSchema`、`responseJsonSchema`、`responseModalities` 含非文本 | — | 400（IR 不能表达结构化输出与多模态输出） |
| `generationConfig.thinkingConfig` | 丢弃 | 与 §6.1 `thinking` 同理 |
| `safetySettings` | 丢弃 | 只对 Gemini 有意义 |
| `cachedContent` | — | 400（引用的是 Gemini 账号下的缓存对象） |
| 其他未知字段 | — | §1 第 5 条（默认 400）。`generationConfig` 下的字段按顶层字段对待 |

`parameters`（OpenAPI 风格 Schema）→ JSON Schema：递归地把 `type` 的值改成小写；`nullable: true` 改为 `"type": [<原类型>, "null"]`；删除 `propertyOrdering`；其余键原样保留。

### 3.2 IR → Gemini 请求（Synth）

输出的请求体**不含模型名**；`Call = {Op: "models.call", Model: upstreamModel, Stream: req.Stream, Body, Header: env.Header}`。T47 的出站适配器按 `Call.Stream` 选择上游方法（EG-G-01）。

| IR | Gemini | 说明 |
|:--|:--|:--|
| `System` | `systemInstruction: {parts: [{text}...]}` | `cache_control` 丢弃 |
| user 消息 | `{"role":"user","parts":[...]}` | `ToolResultPart` 生成的 `functionResponse` 放在该消息其余内容之前 |
| assistant 消息 | `{"role":"model","parts":[...]}` | |
| `TextPart` | `{"text"}` | 空文本跳过 |
| `ImagePart{Data}` | `{"inlineData":{"mimeType","data":<base64>}}` | |
| `ImagePart{URL}` | — | 400（Gemini 不能按任意 URL 取图） |
| `ToolCallPart` | `{"functionCall":{"name","args"}}` | 不发送 `id`；第一个加占位签名（*待定 1*） |
| `ToolResultPart` | `{"functionResponse":{"name":<§2.1 查到的函数名>,"response":{"output":<文本拼接>}}}`；`IsError` 时键名为 `error` | 结果中含 `ImagePart` → 400 |
| `ThinkingPart` | 丢弃 | 来源都不是 Gemini |
| `Tools` | `tools: [{"functionDeclarations": [{"name","description","parametersJsonSchema": InputSchema}]}]` | 用 `parametersJsonSchema` 而不是 `parameters`，免去 Schema 转换。*待定 3：执行者确认所有目标模型都支持该字段；不支持时改为转换成 `parameters`* |
| `ToolChoice` `auto` / `any` / `none` / `tool` | `toolConfig.functionCallingConfig.mode` `AUTO` / `ANY` / `NONE` / `ANY` + `allowedFunctionNames: [Name]` | |
| `MaxTokens` | `generationConfig.maxOutputTokens`（0 时不发送） | |
| `Temperature`、`TopP` | `generationConfig.temperature`、`topP` | |
| `StopSequences` | `generationConfig.stopSequences` | 超过 5 个 → 400 |
| `User` | 丢弃 | Gemini 没有对应字段 |

连续两条相同角色的消息原样输出，不合并。

### 3.3 Gemini 响应 → IR（Decoder）

非流式：

| Gemini | IR |
|:--|:--|
| `responseId` | `ID` |
| `candidates` 恰好 1 个 | 取它；不是 1 个且没有 `promptFeedback.blockReason` → 502 `upstream_error` |
| `candidates` 为空且有 `promptFeedback.blockReason` | `Parts` 为空，`StopRefusal` |
| part `text`（非思考） | `TextPart` |
| part `text` 且 `thought: true` | `ThinkingPart{Vendor: gemini, Text}` |
| part `functionCall` | `ToolCallPart{ID: id 或网关签发, Name, Arguments: args 或 {}}`；`args` 不是对象 → 502 |
| `finishReason` `STOP`（有 functionCall 时）| `StopToolUse` |
| `finishReason` `STOP`（无 functionCall）、缺失、`FINISH_REASON_UNSPECIFIED`、`OTHER`、`LANGUAGE` | `StopEndTurn` |
| `finishReason` `MAX_TOKENS` | `StopMaxTokens` |
| `finishReason` `SAFETY`、`RECITATION`、`BLOCKLIST`、`PROHIBITED_CONTENT`、`SPII`、`IMAGE_SAFETY` | `StopRefusal` |
| `finishReason` `MALFORMED_FUNCTION_CALL`、`UNEXPECTED_TOOL_CALL` | 上游故障：非流式 502 `upstream_error`，流式返回流错误（与 §9.3 对 `insufficient_system_resource` 的处理一致） |
| 其他未列出的 `finishReason` | `StopEndTurn` |
| `usageMetadata` | `Usage`，口径同 USE-G-02：`Input = prompt − cached + toolUsePrompt`，`CacheRead = cached`，`CacheWrite = 0`，`Output = candidates + thoughts` |

流式（每个 SSE 事件的 `data` 是一个完整的 `GenerateContentResponse` 片段）：

| 上游 | IR 事件 |
|:--|:--|
| 第一个 chunk | `EventStart`（`ID = responseId`） |
| 文本 part | `EventText`；思考 part → `EventThinking` |
| `functionCall` part | `EventToolStart` → `EventToolArgs`（`args` 的紧凑 JSON）→ `EventToolEnd`（§2.3） |
| `usageMetadata` | `EventUsage`（累计值，后到覆盖先到） |
| `finishReason` 非空 | `EventStop`（映射同非流式） |
| 上游 EOF，且之前出现过 `finishReason` | `EventEnd` |
| 上游 EOF 但没有出现过 `finishReason` | 流错误（`OutcomeNetworkError`，与 Anthropic Decoder 的「`message_stop` 前结束」一致） |
| 带顶层 `error` 的 chunk | 流错误，分类同 STR-G-01 |

### 3.4 IR → Gemini 响应（Encoder）

非流式：

```json
{
  "candidates": [{"content": {"role": "model", "parts": [...]}, "finishReason": "<见下>", "index": 0}],
  "usageMetadata": {"promptTokenCount": <Input+CacheRead+CacheWrite>, "candidatesTokenCount": <Output>,
                    "totalTokenCount": <两者之和>, "cachedContentTokenCount": <CacheRead>},
  "modelVersion": "<客户端请求的模型名>",
  "responseId": "<IR ID；为空时网关签发>"
}
```

| IR | Gemini |
|:--|:--|
| `TextPart`（含 `IsRefusal`） | `{"text"}` |
| `ThinkingPart` | 丢弃（与 T43 的 B4 一致：只有 Chat 客户端输出思考内容） |
| `ToolCallPart` | `{"functionCall":{"id","name","args"}}`：带上 `id`，客户端回传 `functionResponse.id` 时 Lower 能精确对应 |
| `StopEndTurn`、`StopToolUse`、`StopSequence` | `STOP` |
| `StopMaxTokens` | `MAX_TOKENS` |
| `StopRefusal`，或含 `IsRefusal` 的文本 | `SAFETY` |

流式：每个 IR 文本事件输出一个 chunk（`candidates[0].content.parts = [{"text"}]`，不带 `finishReason`）；工具调用按 §2.3 缓冲后输出；`EventEnd` 时输出最后一个 chunk：`parts` 为空数组、带 `finishReason` 和 `usageMetadata`。**不输出** `[DONE]` 或其他结束标记，Encoder 在最后一个 chunk 之后返回 `io.EOF`。

### 3.5 注册

`protocols.go` 增加 `protoGemini = "gemini"`、`opGeminiCall = "models.call"`；按 §2.4 登记 Gemini 的 `clientSide` 与 `upstreamSide`，在组合表中加入 §1 表中的 5 行。`cmd/gateway/adapters.go` 不需要改（转换器已经由 `translate.Registrar` 统一注册）。

### 3.6 待定项汇总

1. 是否注入占位思考签名（§2.2）。建议注入。
2. `countTokens` 在转译路径上：a. 400（建议，Translator 只能生成上游调用，不能本地作答）；b. 改内核让 Translator 能本地返回响应：需要修改冻结契约，不建议。执行者要在阶段 A 确认 Gemini CLI 在 `countTokens` 失败时能否继续工作，把结论写进审批请求。如果不能，Gemini CLI 就只能用 Gemini 上游，部署文档要写明。
3. `parametersJsonSchema` 还是 `parameters`（§3.2）。建议 `parametersJsonSchema`。

## 4. 实现要求（阶段 B：用户批准后）

- 4 个新组件遵守 §9.1 的分工：Decoder 不读 `env`、不知道客户端协议；Encoder 不知道上游协议。
- Lower 与 Synth 只用于请求方向；`gemini_lower.go` 可以把请求体反序列化为结构体（转译层本来就要解析全部内容），但要按 §1 第 5 条检查未知字段。
- 所有新生成的 ID 用 `ids.go` 的 `crypto/rand` 实现，测试中通过 `NewID` 注入。
- `TestStreamMatrix` 加入 Gemini Decoder 和 Gemini Encoder，组合数从 24 增加到 3 个 Decoder × 4 个 Encoder 的全部组合；为 Gemini Encoder 补上客户端协议的结构约束检查（每个 chunk 都是合法的 `GenerateContentResponse`，只有最后一个带 `finishReason`，`functionCall.args` 是对象）。
- 黄金文件放在 `internal/translate/testdata/{gemini_to_ir, ir_to_gemini, gemini_to_anthropic, gemini_to_chat, anthropic_to_gemini, chat_to_gemini, responses_to_gemini}/`。**现有黄金文件一律不改**。

## 5. 涉及文件

- spec：`spec/10_ir_translation.md`（新增 §10；§9.1 补充表驱动注册的约定）、`spec/11_tasks.md`（登记本卡与状态）
- 新增：`internal/translate/gemini_lower.go`、`gemini_synth.go`、`decode_gemini.go`、`encode_gemini.go` 及各自的测试；`internal/translate/testdata/` 下的上述新目录；`cmd/gateway/usage_consistency_test.go`（§2.5）
- 修改：`internal/translate/protocols.go`、`register.go`、`register_test.go`、`matrix_test.go`、`doc.go`；§2.4 重构时现有测试文件中转换器的**构造调用**（只改调用写法，不改断言）
- 可选：`cmd/gateway/e2e_test.go` 新增独立的 Gemini 转译端到端用例（不改现有用例）

## 6. 测试要求

每个新方向至少覆盖 spec/10 §7 的 10 个用例，另加：

1. **工具 ID**：Gemini 上游不带 `id` 时网关签发，客户端回传后 Synth 能换回函数名；历史中找不到对应调用 → 400。Gemini 客户端不带 `id` 时 Lower 生成确定性 ID，并行两个同名调用按顺序对应；带 `id` 时按 `id` 对应。
2. **占位签名**（待定 1 选 a 时）：每条 model 消息只有第一个 `functionCall` 带签名。
3. **Schema**：`parameters` 中的大写 `type`、`nullable`、`propertyOrdering` 转换正确；`parametersJsonSchema` 原样透传。
4. **停止原因**：§3.3 表中每一类 `finishReason` 至少一个用例，包括 `MALFORMED_FUNCTION_CALL` 的非流式 502 与流式流错误，以及 `promptFeedback.blockReason`。
5. **用量**：`cachedContentTokenCount`、`thoughtsTokenCount`、`toolUsePromptTokenCount` 映射正确；Gemini Encoder 的 `promptTokenCount` 含缓存。
6. **流式**：EOF 前没有 `finishReason` → 流错误；Gemini Encoder 缓冲工具参数，拼出非对象 → 流错误；超过 1 MiB → 流错误。
7. **拒绝与丢弃**：内置工具、`fileData`、非图片 `inlineData`、`candidateCount > 1`、`responseSchema`、`cachedContent`、`seed`、`countTokens` 都返回 400，消息指出字段；`topK`、`thinkingConfig`、`safetySettings` 被丢弃，请求正常转发。
8. **矩阵**：`TestStreamMatrix` 全部组合通过。
9. **表驱动注册**（§2.4）：重构提交本身让 `go run ./tools/check` 全绿，现有测试断言与黄金文件零改动；`TestRegistrarPairs` 通过。
10. **用量一致**（§2.5）：现有 4 个方向和 Gemini 的 5 个方向，计费用量与客户端可见用量逐单位相等。

## 7. 验收标准

- [ ] 阶段 A 的规格已获用户批准，3 个待定项都有答案。
- [ ] §6 的测试全部通过；`go run ./tools/check` 全绿；现有黄金文件逐字节不变。
- [ ] 没有修改 `internal/core`、`internal/protocol`、`internal/ir` 的导出类型；没有新增依赖。
- [ ] §2.4 的重构是一个独立提交，`git diff` 中现有测试文件只有构造调用的改动。
- [ ] §2.5 的一致性测试覆盖全部 9 个方向；如果发现现有方向本来就不一致，**停下报告**，不要自行修改计费或转译逻辑。
- [ ] 真实客户端验证（每项没做就写明原因）：Claude Code → Gemini 上游跑通一次带工具调用的多轮对话；Gemini CLI 或 genai SDK → Anthropic 或 Chat 上游跑通一次流式对话。交付说明里**不贴出任何 Key 或 Token**。
- [ ] 改完后在 Linux 上按 [T46](T46_race_verify_linux.md) 的流程复跑 `-race`（可以由审查方安排）。

## 8. 建议提交拆分

1. 阶段 A：`docs(spec): T48 新增 spec/10 §10 Gemini 转译（待审批）`
2. `refactor(translate): T48 转换器改为表驱动注册`（§2.4，纯重构）
3. `test(gateway): T48 计费用量与转译用量一致性测试`（§2.5，先覆盖现有 4 个方向）
4. `feat(translate): T48 Gemini 请求 Lower 与 Synth`
5. `feat(translate): T48 Gemini Decoder 与 Encoder`
6. `feat(translate): T48 注册 Gemini 转换器，扩展流式矩阵与用量一致性测试`
