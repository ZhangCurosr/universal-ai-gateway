# 10 跨协议转译（M2：IR & Translation）

> **M1 不实现本章。** M1 完成（任务 T01–T14 全部 `DONE`）后才开始 T20 起的任务。
> 代码位置：`internal/ir`（类型已提供）、`internal/translate`（T20–T27）。

## 0. 接入方式

转换器实现 `protocol.Translator`（`internal/protocol/translate.go`）：`Request(env, upstreamModel) (*protocol.Call, error)` 生成上游协议的 Call（`Op` 取上游协议的操作名常量，`Body` 为转换后的请求体）；`Response(env, resp) (*protocol.Response, error)` 转换上游响应：`ResponseBuffered` 转换 `Body`，`ResponseEvents` 返回包装了 `resp.Events` 的新 `EventStream`（逐个转换事件，上游 `io.EOF` 时先输出收尾事件再返回 `io.EOF`）。实现放在 `internal/translate/`，每个方向一个类型（如 `OpenAIToAnthropic`），由 `translate.Register(r *protocol.Registry) error` 统一注册，并在 `cmd/gateway/adapters.go` 中调用。**内核的转译路径在 M1 的 T10 中已经实现并用假转换器测试过（CORE-04），M2 只需要编写转换器本身，不修改内核。**

`internal/translate` 不得导入具体适配器包；需要的协议常量直接写字符串（如 `protocol.Protocol("anthropic")`），并在包内集中定义。

## 1. 原则

1. 转译只用于「入站协议 ≠ 上游协议」。同协议永远走透传（08）。
2. 转译是**有损**的：无法表达的特性必须**明确拒绝**（400 `invalid_request`，消息说明哪个特性不支持）或**明确丢弃**（下表标注「丢弃」），**禁止**默默编造等价物。
3. 思考签名、加密推理内容**只能回传给签发它的厂商**；跨厂商一律丢弃（详见 06_architecture_review ADR-03）。
4. 入站解析（Lower）与出站合成（Synthesize）分开写，每个方向独立测试：`openai → IR`、`IR → anthropic`、`anthropic → IR`、`IR → openai`。
5. **未知字段策略**：请求中出现本章字段表未列出的字段，按配置 `translation.unknown_fields`（spec/03）处理：
   - `reject`（默认）：400 `invalid_request`，消息为 `<字段路径> is not supported when translating to another protocol`；
   - `drop`：丢弃该字段继续处理，并按第 6 条记日志。

   「字段表未列出」指：顶层字段、消息（或 Responses 输入项）级字段。内容块级未知键保持始终丢弃。
   字段表中**标为 400 的字段**始终拒绝，不受此配置影响；**标为丢弃的字段**始终丢弃，不记日志。
   值为 JSON `null` 的字段视为未提供，不触发此规则。
6. `drop` 模式下，每个进程对每个（转换方向、字段路径）组合只记**一次** `Warn` 日志：`msg="translation dropped unknown field"`，属性 `from`、`to`、`field`（字段路径，如 `messages[].foo`，数组下标统一写成 `[]`）。
   日志**不得**包含字段的值。

## 2. IR 类型（`internal/ir/types.go`，已提供）

```go
type Request struct {
    Model         string
    System        []TextPart
    Messages      []Message
    Tools         []Tool
    ToolChoice    *ToolChoice
    MaxTokens     int64            // 0 = 未指定
    Temperature   *float64
    TopP          *float64
    StopSequences []string
    Stream        bool
    User          string
}
type Message struct { Role Role; Parts []Part }        // Role: user | assistant
type Part interface{ isPart() }                         // TextPart / ImagePart / ToolCallPart / ToolResultPart / ThinkingPart
type TextPart struct { Text string; CacheControl *CacheControl; IsRefusal bool } // IsRefusal 仅响应方向：上游拒绝说明（§9.3）
type ImagePart struct { MediaType string; Data []byte; URL string }
type ToolCallPart struct { ID, Name string; Arguments json.RawMessage }
type ToolResultPart struct { ToolCallID string; Content []Part; IsError bool }
type ThinkingPart struct { Vendor ir.Vendor; Text string; Signature string; Redacted bool; RedactedData string }
type Tool struct { Name, Description string; InputSchema json.RawMessage; CacheControl *CacheControl }
type ToolChoice struct { Mode string; Name string }    // auto | any | none | tool
type CacheControl struct { Type string; TTL string }
type Response struct { ID, Model string; Parts []Part; StopReason StopReason; Usage TokenUsage }
type TokenUsage struct { Input, CacheRead, CacheWrite, Output int64 }
type StreamEvent struct { Kind StreamEventKind; ID, Text string; IsRefusal bool; ToolIndex int; ToolID, ToolName string; Usage TokenUsage; Stop StopReason }
```

## 3. OpenAI Chat → IR（T20）

| OpenAI 字段 | IR | 说明 |
|:--|:--|:--|
| `messages[role=system]`、`messages[role=developer]` | `System`（按出现顺序拼接为多个 TextPart） | 只能出现在 user/assistant 消息之前，否则 400 |
| `messages[role=user].content` 字符串 | `Message{user, [TextPart]}` | |
| `content` 数组 `type=text` | `TextPart` | |
| `content` 数组 `type=image_url` | `ImagePart`：`data:` URL 解码为 `Data`+`MediaType`；http(s) URL 放 `URL` | |
| `messages[role=assistant].content` | `TextPart` | |
| `messages[role=assistant].tool_calls[]` | `ToolCallPart{ID, function.name, function.arguments}` | `arguments` 必须是合法 JSON 字符串，否则 400 |
| `messages[role=tool]` | `ToolResultPart{tool_call_id, [TextPart(content)]}`，放入一条 `user` 消息 | 连续的多条 tool 消息合并到同一条 user 消息 |
| `messages[].name` | — | 丢弃（IR 与 Anthropic 无对应字段） |
| `tools[type=function]` | `Tool{name, description, parameters}` | 非 function 类型 → 400 |
| `tool_choice` `"auto"`/`"none"`/`"required"`/`{function:{name}}` | `auto` / `none` / `any` / `tool+Name` | |
| `max_completion_tokens`，否则 `max_tokens` | `MaxTokens` | |
| `temperature`、`top_p`、`stop`（字符串或数组）、`stream`、`user` | 对应字段 | |
| `n > 1` | — | 400（不支持） |
| `response_format`、`logprobs`、`audio`、`modalities` | — | 400（不支持） |
| `reasoning_content`（历史 assistant 消息） | — | 丢弃 |
| `presence_penalty`、`frequency_penalty`、`seed`、`reasoning_effort` | — | 丢弃 |

## 4. IR → Anthropic Messages（T21）

| IR | Anthropic | 说明 |
|:--|:--|:--|
| `System` | 顶层 `system` 数组（`{"type":"text","text":...}`，保留 `cache_control`） | |
| `Messages` | `messages`；相邻同角色消息合并（Anthropic 要求 user/assistant 交替） | 第一条必须是 user，否则在最前插入 `{"role":"user","content":[{"type":"text","text":"(continue)"}]}` 并记 Debug 日志 |
| `TextPart` | `{"type":"text","text":...}` | |
| `ImagePart` | `{"type":"image","source":{"type":"base64",...}}` 或 `{"type":"url","url":...}` | |
| `ToolCallPart` | `{"type":"tool_use","id","name","input":<arguments 解析后的对象>}` | |
| `ToolResultPart` | `{"type":"tool_result","tool_use_id","content":[...],"is_error"}` | |
| `ThinkingPart{Vendor: anthropic}` | `{"type":"thinking","thinking","signature"}` / `redacted_thinking` | 其他 Vendor → 丢弃 |
| `Tools` | `tools[{name, description, input_schema}]` | |
| `ToolChoice` | `{"type":"auto"|"any"|"tool","name"}`；`none` → 不发送 `tools` 与 `tool_choice` | |
| `MaxTokens == 0` | `max_tokens = translate.DefaultMaxTokens`（8192） | Anthropic 必填 |
| `StopSequences` | `stop_sequences` | |
| `User` | `metadata.user_id` | |

## 5. Anthropic 响应 → OpenAI 响应（T22）

### 5.1 非流式

| Anthropic | OpenAI |
|:--|:--|
| `id` | `id`（前缀改为 `chatcmpl-`） |
| `content[type=text]` 拼接 | `choices[0].message.content` |
| `content[type=tool_use]` | `choices[0].message.tool_calls[]`（`arguments` = `input` 的 JSON 字符串） |
| `content[type=thinking]` | `choices[0].message.reasoning_content`（仅展示，客户端回传时会被 §3 丢弃） |
| `stop_reason`: `end_turn`/`stop_sequence` → `stop`；`max_tokens` → `length`；`tool_use` → `tool_calls`；`refusal` → `content_filter`；其他未列出值与缺失 → `stop` | `choices[0].finish_reason` |
| `usage` | `usage.prompt_tokens = input + cache_read + cache_write`、`completion_tokens = output`、`prompt_tokens_details.cached_tokens = cache_read` |
| — | `object = "chat.completion"`、`created = 当前 Unix 秒`、`model = 客户端请求的模型名` |

### 5.2 流式（状态机）

| Anthropic 事件 | 输出 OpenAI chunk |
|:--|:--|
| `message_start` | 一个 chunk：`delta: {"role":"assistant","content":""}` |
| `content_block_start` type=text | 无输出 |
| `content_block_delta` `text_delta` | `delta: {"content": text}` |
| `content_block_start` type=tool_use | `delta.tool_calls[{index: i, id, type:"function", function:{name, arguments:""}}]`，`i` 为本响应内第几个工具调用（从 0 开始） |
| `content_block_delta` `input_json_delta` | `delta.tool_calls[{index: i, function:{arguments: partial_json}}]` |
| `content_block_delta` `thinking_delta` | `delta: {"reasoning_content": thinking}` |
| `content_block_delta` `signature_delta`、`content_block_stop`、`ping` | 无输出 |
| `message_delta` | `delta: {}`、`finish_reason` 按 §5.1 映射 |
| `message_stop` | 若客户端请求了 `stream_options.include_usage`：先发 usage chunk（`choices: []`）；然后 `data: [DONE]` |
| `error` | 返回入站适配器 `StreamError` 生成的事件（spec/08 §2.6）并结束 |

所有 chunk 的 `id` 相同、`object = "chat.completion.chunk"`、`created` 相同、`model` 为客户端请求的模型名。

## 6. 反方向（T23、T24）

目标场景：Claude Code 等只说 Anthropic 的客户端使用 DeepSeek、Qwen 等只提供 OpenAI Chat 的上游。
转换器 `anthropic → openai_chat` = Anthropic 请求 → IR（§6.1）+ IR → Chat（§6.2）；Chat 响应/流 → Anthropic（§6.3）。
§6.2 同时被 `openai_responses → openai_chat`（T27）复用。

### 6.1 Anthropic Messages → IR（T23）

| Anthropic 字段 | IR | 说明 |
|:--|:--|:--|
| `model` | `Model` | 必填 |
| `max_tokens` | `MaxTokens` | 必填，≤ 0 → 400 |
| `system` 字符串 | `System`（一个 TextPart） | |
| `system` 数组 `type=text` | `System`（按顺序，保留 `cache_control`） | 其他类型 → 400 |
| `messages[role=user\|assistant].content` 字符串 | 一个 TextPart | 除 `system`（下一行）外的其他 role → 400 |
| `messages[role=system]`（Claude Code 2.1 起会发送；T75，TC-01） | 出现在第一条 user / assistant 消息之前：追加到 `System`；之后：作为 user 文本留在原位置（上一条 IR 消息是 user 时追加到其末尾，否则新建 user 消息；紧随其后的 user 消息并入同一条，不产生连续两条 user 消息） | 内容只允许文本，否则 400；文本原样保留，不加标记 |
| content 块 `text` | `TextPart`（保留 `cache_control`） | `citations` 丢弃 |
| content 块 `image`，`source.type=base64` | `ImagePart{MediaType, Data}` | base64 非法 → 400 |
| content 块 `image`，`source.type=url` | `ImagePart{URL}` | 只允许 http(s) |
| content 块 `tool_use`（仅 assistant） | `ToolCallPart{id, name, input}` | `input` 必须是 JSON 对象 |
| content 块 `tool_result`（仅 user） | `ToolResultPart{tool_use_id, content, is_error}` | `content` 为字符串或 `text`/`image` 块数组；其他块 → 400 |
| content 块 `thinking` / `redacted_thinking`（仅 assistant） | `ThinkingPart{Vendor: anthropic, ...}` | 合成到其他厂商时按 §1 第 3 条丢弃 |
| content 块 `document`、`search_result`、`server_tool_use`、`*_tool_result`、`container_upload` 等 | — | 400（消息指出块类型） |
| `tools[]` 无 `type` 或 `type=custom` | `Tool{name, description, input_schema, cache_control}` | |
| `tools[]` 其他 `type`（`web_search_*`、`bash_*`、`text_editor_*`、`computer_*`、`code_execution_*` 等服务端/内置工具） | — | 400（`built-in tool "<type>" is not supported for model "<name>"`） |
| `tool_choice` `auto` / `any` / `tool+name` / `none` | 同名 Mode | |
| `tool_choice.disable_parallel_tool_use: true` | — | 400（IR 无法表达；与 T20 拒绝 `parallel_tool_calls=false` 一致） |
| `stop_sequences`、`temperature`、`top_p`、`stream` | 对应字段 | |
| `metadata.user_id` | `User` | `metadata` 其他字段丢弃 |
| `top_k` | — | 丢弃（Chat 无等价参数） |
| `thinking`（`enabled` / `adaptive` / `disabled`） | 丢弃 | 与 §8.2 `reasoning.effort` 同理，不做猜测性映射 |
| `service_tier`、`context_management` | 丢弃 | |
| `mcp_servers`、`container` | — | 400 |
| 其他未知顶层字段 | — | 见 §1 第 5 条未知字段策略（默认 400） |

### 6.2 IR → OpenAI Chat（T23，T27 复用）

| IR | Chat | 说明 |
|:--|:--|:--|
| `System` | 每个 TextPart 一条 `{"role":"system","content":<字符串>}`，放在最前 | `cache_control` 丢弃 |
| user 消息中的 `ToolResultPart` | 每个一条 `{"role":"tool","tool_call_id","content":<文本拼接>}`，按出现顺序放在该 user 消息其余内容之前 | 结果中含 `ImagePart` → 400；`is_error` 丢弃（错误文本已在 content 中） |
| user 消息其余内容只有一个 TextPart | `{"role":"user","content":<字符串>}` | 多数兼容上游只稳定支持字符串 content |
| user 消息其余内容为多个块或含图片 | `content` 数组：`{"type":"text"}` / `{"type":"image_url","image_url":{"url"}}` | `Data` 编码为 `data:<media_type>;base64,...` |
| assistant 消息的 TextPart | `content`（多个直接拼接） | 没有文本时 `content: null` |
| assistant 消息的 `ToolCallPart` | `tool_calls[{id, type:"function", function:{name, arguments:<JSON 字符串>}}]` | |
| `ThinkingPart` | 丢弃 | 任何厂商都不回传给 Chat 上游 |
| `Tools` | `tools[{type:"function", function:{name, description, parameters}}]` | `cache_control` 丢弃 |
| `ToolChoice` `auto` / `any` / `none` / `tool` | `"auto"` / `"required"` / `"none"` / `{"type":"function","function":{"name"}}` | |
| `MaxTokens` | `max_tokens`（0 时不发送） | 兼容上游普遍只认 `max_tokens`；见下方待定项 |
| `Temperature`、`TopP` | 同名 | |
| `StopSequences` | `stop` 数组 | 超过 4 个 → 400 |
| `Stream` | `stream`；为 true 时同时发送 `stream_options: {"include_usage": true}` | 由合成器显式写入，PT-03 不再注入，因此 FWD-02 不会过滤 usage chunk，§6.3 能拿到用量 |
| `User` | `user` | |

### 6.3 Chat 响应 → Anthropic 响应（T24）

非流式：

| Chat | Anthropic |
|:--|:--|
| `id` | `id`：`chatcmpl-` 前缀改为 `msg_`；缺失时网关签发 `msg_` + 24 位 base32 |
| — | `type: "message"`、`role: "assistant"`、`model` = 客户端请求的模型名、`stop_sequence: null` |
| `choices[0].message.content` 非空 | 一个 `text` 块 |
| `choices[0].message.refusal` 非空 | 一个 `text` 块（拒绝文本），`stop_reason: "refusal"` |
| `choices[0].message.tool_calls[]` | 每个一个 `tool_use` 块（`input` = `arguments` 解析后的对象；无法解析为 JSON 对象 → 502 `upstream_error`） |
| `choices[0].message.reasoning_content` | 丢弃（没有签名，回传给 Anthropic 上游会被拒绝） |
| `finish_reason`：`stop` → `end_turn`；`length` → `max_tokens`；`tool_calls` / `function_call` → `tool_use`；`content_filter` → `refusal`；`insufficient_system_resource`（DeepSeek：推理因资源不足被中断，输出不完整）视为上游故障（非流式 502 `upstream_error`，流式返回流错误）；其他未列出的值与缺失值 → `end_turn` | `stop_reason` |
| `usage` | `input_tokens = prompt_tokens − cached_tokens`、`cache_read_input_tokens = prompt_tokens_details.cached_tokens`、`cache_creation_input_tokens = 0`、`output_tokens = completion_tokens` |
| `choices` 不是恰好 1 个 | 502 `upstream_error` |

流式（状态机；输出的 content block `index` 从 0 递增）：

| Chat chunk | 输出 Anthropic 事件 |
|:--|:--|
| 第一个 chunk | `message_start`（`content: []`、`stop_reason: null`、`usage: {input_tokens: 0, output_tokens: 0}`；真实用量在 `message_delta` 中给出） |
| `delta.content` 非空 | 没有打开的 text 块时：先关闭打开的块，再 `content_block_start`（text）；然后 `content_block_delta`（`text_delta`） |
| `delta.tool_calls[]` 中出现新的 `index` | 关闭打开的块，`content_block_start`（`tool_use`，`id`、`name`、`input: {}`） |
| `delta.tool_calls[].function.arguments` 非空 | `content_block_delta`（`input_json_delta`）；参数属于已关闭的工具调用（交错输出）→ 流错误 |
| `delta.reasoning_content` | 丢弃 |
| `finish_reason` 非空 | 关闭打开的块（`content_block_stop`），记下 stop_reason（映射同非流式） |
| `choices: []` 且 `usage` 非 null | 记下用量 |
| `[DONE]` | `message_delta`（`delta: {stop_reason, stop_sequence: null}`，`usage` 按非流式映射）→ `message_stop` |
| `{"error":...}` chunk、上游流在 `[DONE]` 前结束、`[DONE]` 前没有 `finish_reason`、`finish_reason` 为 `insufficient_system_resource` | 返回入站适配器 `StreamError`（`event: error`）并结束 |

### 6.4 必测用例

§7 的 10 个用例全部适用，另加：服务端工具 → 400；`top_k` 被丢弃；`thinking` 被丢弃；`tool_result` 带 `is_error`；
`stop_sequences` 超过 4 个 → 400；流式 usage chunk 映射到 `message_delta.usage`；`content_filter` → `refusal`。

### 6.5 待定项（起草时的取舍，审批时请确认）

1. `max_tokens` 而非 `max_completion_tokens`：前者兼容 DeepSeek / Qwen 等，后者是 OpenAI 官方 o 系列推理模型的要求。若需要直连 OpenAI 官方推理模型，应改为可按池配置。
2. `reasoning_content` → Anthropic 一律丢弃：保守做法；代价是 Claude Code 看不到 DeepSeek 的思考过程。
3. `thinking` 参数丢弃而不是 400：否则 Claude Code（默认开启 thinking）无法使用 Chat 上游。
4. `disable_parallel_tool_use` 选择 400 而不是丢弃：按 §1 第 2 条「改变调用语义的不默默丢弃」。`top_k` 起草时也是 400，T42 根据 T41 实测报告（客户端配置了 `top_k` 时，400 会中断会话）改为丢弃，见 §6.1。

## 7. 必测用例（每个方向至少）

1. 纯文本单轮；2. 多轮对话；3. system 提示；4. 单个工具调用及其结果回填；5. 并行两个工具调用；
6. 图片输入（base64）；7. 流式文本；8. 流式工具调用（参数分多个 delta）；9. 不支持的字段 → 400；
10. 思考块跨厂商被丢弃。测试数据放在 `internal/translate/testdata/<用例名>/{input.json, expected.json}`，用黄金文件比较。

## 8. OpenAI Responses 的转译（T25 ~ T27）

目标场景：Codex CLI 等只说 Responses 的客户端使用 Claude、DeepSeek 等非 Responses 上游。只做**入站 Responses → 其他上游**这一个方向（反方向——用 Chat / Anthropic 客户端调用只提供 Responses 的上游——目前没有需求，不做）。

| 转换器 | 组成 | 任务卡 |
|:--|:--|:--|
| `openai_responses` → `anthropic` | Responses 请求 → IR（T25）+ IR → Anthropic（T21，复用）；Anthropic 响应/流 → Responses（T26） | T25、T26 |
| `openai_responses` → `openai_chat` | Responses 请求 → IR（T25，复用）+ IR → Chat（T23，复用）；Chat 响应/流 → Responses（T27） | T27 |

### 8.1 有状态特性（转译路径的硬规则）

被转换到的上游不保存响应，因此 Responses 的状态特性在转译路径上**一律不支持**：

| 编号 | 规则 |
|:--|:--|
| RX-01 | 请求带 `previous_response_id` 或 `conversation` → 400 `invalid_request`（`previous_response_id is not supported for model "<name>"; send the full input instead`）。Codex CLI 默认发送完整输入，不受影响 |
| RX-02 | `store` 视为 false：不产生 `Response.Pins`；响应中的 `store` 字段写 `false` |
| RX-03 | `background: true` → 400 |
| RX-04 | 转译路径上不支持按 ID 的操作（get / delete / cancel / input_items）：这些请求没有服务名，只能由固定记录路由，而转译响应从不产生固定记录，因此内核自然返回 404（CORE-11），无需特殊处理 |
| RX-05 | 响应 ID 由网关生成：`resp_` + 24 位小写 base32（`crypto/rand`）；输出项 ID 为 `msg_` / `fc_` + 同样格式 |

### 8.2 请求 → IR（T25）

| Responses 字段 | IR | 说明 |
|:--|:--|:--|
| `instructions` | `System`（一个 TextPart） | |
| `input` 为字符串 | 一条 user 消息，一个 TextPart | |
| `input[]` 中 `{"type":"message","role":"system"\|"developer"}` | 追加到 `System` | 只允许出现在其他消息之前，否则 400 |
| `input[]` 中 `{"type":"message","role":"user"\|"assistant"}` | `Message`；`content` 为字符串或 `input_text` / `output_text` / `input_image` 部件 | `input_image.image_url` 为 data URL 时解码为 `Data`，否则填 `URL`；`input_file` → 400 |
| `input[]` 中 `{"type":"function_call","call_id","name","arguments"}` | 追加到前一条 assistant 消息的 `ToolCallPart{ID: call_id}`；前面不是 assistant 消息则新建一条 | |
| `input[]` 中 `{"type":"function_call_output","call_id","output"}` | user 消息中的 `ToolResultPart{ToolCallID: call_id}` | 连续的多个合并为一条 user 消息 |
| `input[]` 中 `{"type":"reasoning","encrypted_content"}` | `ThinkingPart{Vendor: openai, Encrypted}` | 合成时按 §1 第 3 条跨厂商丢弃 |
| `tools[]` 中 `{"type":"function","name","description","parameters"}` | `Tool` | `strict` 丢弃 |
| `tools[]` 中非 function 内置工具（`namespace`、`web_search` 等）未在 `tool_choice` 中显式指定时 | — | 忽略/丢弃（避免阻断客户端整体请求） |
| `tools[]` 中其他类型（或在 `tool_choice` 中被显式指定调用时） | — | 400（`built-in tool "<type>" is not supported for model "<name>"`） |
| `tool_choice` | `ToolChoice`：`auto` → auto，`required` → any，`none` → none，`{"type":"function","name"}` → tool | |
| `max_output_tokens` | `MaxTokens` | |
| `temperature`、`top_p` | 同名 | |
| `reasoning.effort` | 丢弃 | 目标上游的思考预算语义不同，不做猜测性映射 |
| `text.format` 为 `json_schema` / `json_object` | — | 400（无法保证等价） |
| `parallel_tool_calls`、`truncation`、`metadata`、`client_metadata`、`prompt_cache_key`、`safety_identifier`、`user`、`include`、`service_tier` | 丢弃（`safety_identifier` 或 `user` 写入 `IR.User`） | |
| `stream` | `Stream` | 合成 Chat 请求时按 PT-03 加 `stream_options.include_usage` |

### 8.3 响应 → Responses（T26：自 Anthropic，T27：自 Chat）

非流式：生成一个 `{"id","object":"response","created_at","status":"completed","model":<客户端请求的服务名>,"output":[...],"usage":{...}}`：

- 文本 → 一个 `{"type":"message","id","role":"assistant","status":"completed","content":[{"type":"output_text","text","annotations":[]}]}`；
- 工具调用 → 每个一个 `{"type":"function_call","id","call_id":<上游工具调用 ID>,"name","arguments","status":"completed"}`；
- 思考内容丢弃（不输出 `reasoning` 项）；
- 停止原因：正常结束 / 工具调用 → `status: "completed"`；达到输出上限 → `status: "incomplete"`、`incomplete_details: {"reason":"max_output_tokens"}`；`refusal` → `status: "incomplete"`、`incomplete_details: {"reason":"content_filter"}`；其他未列出值（含 `pause_turn`）→ `status: "completed"`；
- `usage`：`input_tokens = input + cache_read + cache_write`、`input_tokens_details.cached_tokens = cache_read`、`output_tokens = output`、`total_tokens` 为两者之和。

流式事件序列（每个事件的 `sequence_number` 从 0 递增，`event:` 名与 `data.type` 相同）：

| 时机 | 输出事件 |
|:--|:--|
| 上游流开始 | `response.created`、`response.in_progress`（`response` 为 `status: "in_progress"`、`output: []` 的骨架） |
| 第一段文本 | `response.output_item.added`（message 项，**总是带 `content: []`**：Codex 等客户端缺少它时会忽略该项；T75，TC-02）→ `response.content_part.added`（output_text，`text: ""`） |
| 每段文本增量 | `response.output_text.delta`（`item_id`、`output_index`、`content_index: 0`、`delta`） |
| 文本结束（遇到工具调用或流结束） | `response.output_text.done`（完整 text）→ `response.content_part.done` → `response.output_item.done` |
| 工具调用开始 | `response.output_item.added`（function_call 项，`arguments: ""`） |
| 工具参数增量 | `response.function_call_arguments.delta` |
| 工具调用结束 | `response.function_call_arguments.done`（完整 arguments）→ `response.output_item.done` |
| 上游正常结束 | `response.completed`（完整 response，含 `usage`）；达到输出上限或 `refusal` 时改为 `response.incomplete` |
| 上游流中错误 | 入站适配器的 `StreamError`（IN-R07）后结束 |

思考增量（Anthropic `thinking_delta`、Chat `reasoning_content`）不输出。`output_index` 按项出现顺序从 0 编号。

自 Chat 上游（T27）时的来源映射：文本 = `delta.content` / `message.content`，Chat 上游的 `refusal` 文本亦作为 `output_text` 输出（非流式与流式均拼接为文本，不生成 `refusal` 内容部件）；工具调用 = `tool_calls[]`（按 `index` 区分，交错输出 → 流错误）；
`finish_reason` 先按 §6.3 映射为 IR 停止原因，再按上一条写状态：`length` → `incomplete` + `max_output_tokens`；`content_filter` → `incomplete` + `content_filter`；`insufficient_system_resource` → 上游故障（非流式 502，流式流错误）；其余 → `completed`（见 §9.3）；`usage.input_tokens = prompt_tokens`（已含缓存）、
`input_tokens_details.cached_tokens = prompt_tokens_details.cached_tokens`、`output_tokens = completion_tokens`；
流式在收到 `[DONE]` 时输出 `response.completed`（用量取之前的 usage chunk；§6.2 保证合成请求带 `include_usage`）。

### 8.4 必测用例

§7 的 10 个用例全部适用，另加：`previous_response_id` → 400；内置工具 → 400；`input` 字符串与数组两种形式；`function_call_output` 回填后的多轮；达到输出上限 → `incomplete`。

## 9. 响应的 IR 与流式 IR（T43）

### 9.1 架构分工：Decoder 与 Encoder

响应转译统一采用两阶段管道架构，将原有 $N \times M$ 的双向响应绑定转换器解耦为 $N + M$ 架构：

```
上游响应/流 ──Decoder（每个上游协议一个）──▶ IR 事件 / ir.Response ──Encoder（每个客户端协议一个）──▶ 客户端响应/流
```

1. **Decoder（未导出）**：只做「上游协议 → 规范化事件 / ir.Response」，不读取 `protocol.Envelope`，不知道目标客户端协议。
2. **Encoder（未导出）**：只做「规范化事件 / ir.Response → 客户端协议」，不知道来源上游协议。所有因客户端而异的选择（如 ID 签发策略、思考内容是否过滤、Responses 的消息与工具项切分、Chat 的 usage chunk 输出条件）均由 Encoder 决定。

### 9.2 流式 IR 规范与不变量

1. **Token 用量口径**：`TokenUsage.Input` 统一按规范化不含缓存的输入 token 数记录（Anthropic 口径）。含缓存的 Chat 上游 Decoder 用 `prompt_tokens - cached` 计算；计费与计量仍以入站 Meter 为准，IR 中的 Usage 仅用于客户端展现。
2. **文本规范**：Decoder 不产生空的文本增量事件（`EventText`），也不在 `ir.Response` 中产生空的 `TextPart`。上游返回空文本时统一不输出。
3. **停止原因映射**：无法识别的上游停止原因统一映射为 `StopEndTurn`。
4. **工具调用生命周期**：Decoder 产出的 `EventToolStart`、`EventToolArgs`、`EventToolEnd` 带规范化的 `ToolIndex`（从 0 开始自增），Encoder 可按需进行流式项状态管理。
5. **流异常处理**：上游流异常（error 事件、提前截断、无停止原因断流等）由 Decoder 统一包装为 `*protocol.AttemptError`，其 Outcome 分类与原转译层完全一致。

### 9.3 经 IR 统一后的语义（T43 验收）

响应改走 Decoder → IR → Encoder 之后，同一个 IR 值对所有客户端协议含义相同，以下几条因此统一：

1. **Chat 上游的停止原因**：只映射一次（§6.3 的 `finish_reason` 表）。因此发给 Responses 客户端时，`content_filter` → `incomplete` + `content_filter`，`insufficient_system_resource` → 上游故障，与发给 Anthropic 客户端的语义一致（T44 G3/G4）。
2. **拒绝说明（B9）**：Chat 上游的 `refusal` 文本在 IR 中是 `TextPart{IsRefusal: true}`（流式为 `EventText` + `IsRefusal`）。Anthropic Encoder 把它输出为 text，并把 `stop_reason` 写为 `refusal`（非流式与流式一致）；Responses Encoder 把它与正文拼接为 `output_text`，不因此改变 `status`。
3. **工具参数**：`ToolCallPart.Arguments` 与 `EventToolArgs` 的 JSON 原样透传给客户端，Encoder 不得解析后重新编码（会改变键序、损失大整数精度）；为空时输出 `{}`。

### 9.4 转换器注册：组件登记 + 组合表（T48）

| 编号 | 规则 |
|:--|:--|
| REG-01 | 每个协议在 `internal/translate` 中**登记一次**自己的组件：作为客户端协议时提供「接受检查（Op / Params）、Lower、Encoder」，作为上游协议时提供「上游 `Call.Op`、Synth、Decoder」。转换器由一个通用的组合类型生成：`Request` = 接受检查 → Lower → 设置上游模型 → Synth；`Response` = 该上游的 Decoder + 该客户端的 Encoder |
| REG-02 | 开放哪些方向由一张**显式的组合表**（`[客户端协议, 上游协议]` 列表）决定，不是所有组合的笛卡尔积。新增协议 = 登记组件 + 在组合表中加行，不再为每个方向手写转换器类型 |
| REG-03 | 组合类型对客户端协议不支持的 `Op` 返回 400 `invalid_request`；`Now` / `NewID` 的测试注入能力保留 |
| REG-04 | 计费用量（出站适配器的 Meter）与客户端可见用量（Decoder → Encoder 输出的 `usage`）必须一致，由 `cmd/gateway/usage_consistency_test.go` 对组合表中**每个方向**逐计量单位核对。客户端协议本身无法表达的单位（如 Chat 客户端的缓存写入）在测试中显式列出 |

## 10. Gemini 的转译（T48）

本章规定 Gemini（`gemini`，spec/08 §10）与 `anthropic`、`openai_chat`、`openai_responses` 之间的转译。开放的方向（§9.4 组合表新增 5 行）：

| 客户端（入站） | 上游 | 典型场景 |
|:--|:--|:--|
| `gemini` | `anthropic` | Gemini CLI / genai SDK 使用 Claude |
| `gemini` | `openai_chat` | Gemini 客户端使用 DeepSeek 等 Chat 兼容上游 |
| `anthropic` | `gemini` | Claude Code 使用 Gemini |
| `openai_chat` | `gemini` | Chat 客户端使用 Gemini |
| `openai_responses` | `gemini` | Codex CLI 使用 Gemini |

`gemini → openai_responses` 不开放（与现有矩阵一致：Responses 只作为客户端协议）。

### 10.1 工具调用 ID

Gemini 的 `functionCall.id` 可选，多数响应不带；IR 与另外三种协议要求每个工具调用都有 ID。

| 编号 | 规则 |
|:--|:--|
| GID-01 | **Decoder**（Gemini 上游）：`functionCall` 不带 `id` 时，网关签发 `call_` + 24 位小写 base32（`crypto/rand`，同 RX-05 的实现）；带 `id` 时原样使用 |
| GID-02 | **Synth**（IR → Gemini 上游）：`ToolResultPart.ToolCallID` 换回函数名：在**同一请求的历史**中找 ID 相同的 `ToolCallPart`，取其 `Name`；找不到 → 400 `tool result "<id>" has no matching tool call`。不需要网关保存状态 |
| GID-03 | **Lower**（Gemini 客户端）：`functionCall` 不带 `id` 时生成确定性 ID `gemini_call_<n>`（`n` 为该调用在整个 `contents` 中的序号，从 0 开始） |
| GID-04 | **Lower**：`functionResponse` 带 `id` 时按 `id` 对应；不带时按 `name` 对应到**紧邻的上一条 model 消息**中第一个尚未被对应的同名调用。对应不上 → 400 `functionResponse "<name>" has no matching functionCall` |

### 10.2 思考签名

| 编号 | 规则 |
|:--|:--|
| GSIG-01 | 来自 Gemini 客户端的 `thoughtSignature` 只能回传给 Gemini；本章的方向都是跨厂商，因此 Lower 读到的签名只进入 `ThinkingPart.Signature`，Synth 到其他厂商时丢弃（§1 第 3 条） |
| GSIG-02 | Synth（IR → Gemini）在每条 model 消息的**第一个** `functionCall` part 上加 `"thoughtSignature": "skip_thought_signature_validator"`（Google 文档给出的占位值，用于历史不是由该模型生成的情况）。上游模型名以 `gemini-1.` 或 `gemini-2.` 开头时**不**注入（旧模型不要求签名）；其余一律注入（§10.8 第 1 条） |

### 10.3 Gemini 请求 → IR（Lower）

| 编号 | 规则 |
|:--|:--|
| GL-01 | 只接受 `Params["method"]` 为 `generateContent` / `streamGenerateContent`。`countTokens` → 400 `countTokens is not supported for model "<name>"`（§10.8 第 2 条） |
| GL-02 | `Stream` 取 `Envelope.Stream`；`Model` 取 `Envelope.Model`（Gemini 请求体中没有模型名） |

| Gemini 字段 | IR | 说明 |
|:--|:--|:--|
| `contents[]` | `Messages` | 至少 1 条，否则 400 |
| `contents[].role` `user`（缺省也视为 `user`）/ `model` | `RoleUser` / `RoleAssistant` | 旧版 `function` 角色视为 `user`；其他值 → 400 |
| part `text`（无 `thought` 或 `thought: false`） | `TextPart` | |
| part `text` 且 `thought: true`（仅 model） | `ThinkingPart{Vendor: gemini, Text, Signature: thoughtSignature}` | 合成到其他厂商时丢弃（§1 第 3 条） |
| 非思考 part 上的 `thoughtSignature` | 丢弃 | 只能回传给 Gemini |
| part `inlineData`，`mimeType` 以 `image/` 开头 | `ImagePart{MediaType, Data}` | base64 非法 → 400 |
| part `inlineData` 其他类型（PDF、音频、视频） | — | 400 `inlineData mimeType "<t>" is not supported for model "<name>"` |
| part `fileData` | — | 400（Files API 的 URI 在其他厂商不可用） |
| part `functionCall`（仅 model） | `ToolCallPart{ID: GID-03, Name, Arguments: args}` | `args` 缺失 → `{}`；不是对象 → 400 |
| part `functionResponse`（仅 user / function） | `ToolResultPart{ToolCallID: GID-04, Content: [TextPart{response 的紧凑 JSON}]}` | `IsError` 恒为 false（错误信息已在 JSON 中）；带 `parts`（多模态结果）→ 400 |
| part `executableCode`、`codeExecutionResult`、`videoMetadata` 等其他 part | — | 400（消息指出 part 类型） |
| `systemInstruction.parts[].text` | `System`（每个一个 TextPart） | 其他 part 类型 → 400 |
| `tools[].functionDeclarations[]` | `Tool{Name, Description, InputSchema}` | `parametersJsonSchema` 原样作为 `InputSchema`；只有 `parameters`（OpenAPI 风格）时按 GL-03 转换；都没有 → `{"type":"object","properties":{}}` |
| `tools[]` 中的内置工具（`googleSearch`、`googleSearchRetrieval`、`codeExecution`、`urlContext`、`googleMaps`、`fileSearch`、`computerUse` 等） | — | 400 `built-in tool "<名>" is not supported for model "<name>"` |
| `toolConfig.functionCallingConfig.mode` `AUTO` / `ANY` / `NONE` | `auto` / `any` / `none` | `ANY` 且 `allowedFunctionNames` 恰好 1 个 → `tool` + 该名字；多于 1 个 → 400；其他 mode（如 `VALIDATED`）→ 400 |
| `generationConfig.maxOutputTokens` | `MaxTokens` | |
| `generationConfig.temperature`、`topP` | 同名 | |
| `generationConfig.stopSequences` | `StopSequences` | |
| `generationConfig.candidateCount` > 1 | — | 400 |
| `generationConfig.topK` | 丢弃 | 与 §6.1 `top_k` 一致（§6.5 第 4 条） |
| `generationConfig.seed`、`presencePenalty`、`frequencyPenalty`、`responseLogprobs`、`logprobs` | — | 400（改变采样语义或输出形状，§1 第 2 条） |
| `generationConfig.responseMimeType` 不是 `text/plain`；`responseSchema`、`responseJsonSchema`、`responseFormat`；`responseModalities` 含 `TEXT` 以外的值；`speechConfig`、`imageConfig` | — | 400（IR 不能表达结构化输出与多模态输出） |
| `generationConfig.thinkingConfig`、`mediaResolution` | 丢弃 | 与 §6.1 `thinking` 同理 |
| `safetySettings` | 丢弃 | 只对 Gemini 有意义 |
| `cachedContent` | — | 400（引用 Gemini 账号下的缓存对象） |
| 其他未知字段 | — | §1 第 5 条。`generationConfig` 下的字段按顶层字段对待 |

| 编号 | 规则 |
|:--|:--|
| GL-03 | `parameters`（OpenAPI 风格 Schema）→ JSON Schema：递归地把 `type` 的值改成小写；`nullable: true` 改为 `"type": [<原类型>, "null"]` 并删除 `nullable`；删除 `propertyOrdering`；其余键原样保留 |

### 10.4 IR → Gemini 请求（Synth）

输出的请求体**不含模型名**；`Call = {Op: "models.call", Model: <上游模型>, Stream: req.Stream, Params: {"method": Stream ? "streamGenerateContent" : "generateContent"}, Body}`。出站适配器按 EG-G-01 选择上游方法，流式时追加 `alt=sse`。

| IR | Gemini | 说明 |
|:--|:--|:--|
| `System` | `systemInstruction: {"parts": [{"text"}...]}` | `cache_control` 丢弃 |
| user 消息 | `{"role":"user","parts":[...]}` | `ToolResultPart` 生成的 `functionResponse` 放在该消息其余 part 之前 |
| assistant 消息 | `{"role":"model","parts":[...]}` | |
| `TextPart` | `{"text"}` | 空文本跳过 |
| `ImagePart{Data}` | `{"inlineData":{"mimeType","data":<base64>}}` | |
| `ImagePart{URL}` | — | 400（Gemini 不能按任意 URL 取图） |
| `ToolCallPart` | `{"functionCall":{"name","args"}}` | 不发送 `id`；GSIG-02 |
| `ToolResultPart` | `{"functionResponse":{"name":<GID-02 查到的函数名>,"response":{"output":<文本拼接>}}}`；`IsError` 时键名为 `error` | 结果含 `ImagePart` → 400 |
| `ThinkingPart` | 丢弃 | 来源都不是 Gemini（§1 第 3 条） |
| `Tools` | `tools: [{"functionDeclarations": [{"name","description","parametersJsonSchema": InputSchema}]}]` | 用 `parametersJsonSchema` 而不是 `parameters`，InputSchema 原样透传，不做转换（§10.8 第 3 条） |
| `ToolChoice` `auto` / `any` / `none` / `tool` | `toolConfig.functionCallingConfig.mode` `AUTO` / `ANY` / `NONE` / `ANY` + `allowedFunctionNames: [Name]` | |
| `MaxTokens` | `generationConfig.maxOutputTokens` | 0 时不发送 |
| `Temperature`、`TopP` | `generationConfig.temperature`、`topP` | |
| `StopSequences` | `generationConfig.stopSequences` | 超过 5 个 → 400 |
| `User` | 丢弃 | Gemini 没有对应字段 |

连续两条相同角色的消息原样输出，不合并。

### 10.5 Gemini 响应 → IR（Decoder）

非流式：

| Gemini | IR |
|:--|:--|
| `responseId` | `ID` |
| `candidates` 恰好 1 个 | 取它；不是 1 个且没有 `promptFeedback.blockReason` → 502 `upstream_error` |
| `candidates` 为空且有 `promptFeedback.blockReason` | `Parts` 为空，`StopRefusal` |
| part `text`（非思考） | `TextPart`（空文本不输出，§9.2 第 2 条） |
| part `text` 且 `thought: true` | `ThinkingPart{Vendor: gemini, Text}` |
| part `functionCall` | `ToolCallPart{ID: GID-01, Name, Arguments: args 或 {}}`；`args` 不是对象 → 502 |
| `usageMetadata` | `Usage`，口径同 USE-G-02：`Input = promptTokenCount − cachedContentTokenCount（不小于 0）+ toolUsePromptTokenCount`，`CacheRead = cachedContentTokenCount`，`CacheWrite = 0`，`Output = candidatesTokenCount + thoughtsTokenCount` |

`finishReason` 映射（非流式与流式共用）：

| `finishReason` | IR |
|:--|:--|
| `STOP`，且该候选含 `functionCall` | `StopToolUse` |
| `STOP`（无 `functionCall`）、缺失、`FINISH_REASON_UNSPECIFIED`、`OTHER`、`LANGUAGE` | `StopEndTurn` |
| `MAX_TOKENS` | `StopMaxTokens` |
| `SAFETY`、`RECITATION`、`BLOCKLIST`、`PROHIBITED_CONTENT`、`SPII`、`IMAGE_SAFETY`、`IMAGE_PROHIBITED_CONTENT`、`IMAGE_RECITATION` | `StopRefusal` |
| `MALFORMED_FUNCTION_CALL`、`UNEXPECTED_TOOL_CALL`、`TOO_MANY_TOOL_CALLS`、`MISSING_THOUGHT_SIGNATURE`、`MALFORMED_RESPONSE` | 上游故障：非流式 502 `upstream_error`，流式返回流错误（同 §9.3 第 1 条对 `insufficient_system_resource` 的处理） |
| 其他未列出的值 | `StopEndTurn`（§9.2 第 3 条） |

流式（每个 SSE 事件的 `data` 是一个完整的 `GenerateContentResponse` 片段）：

| 上游 | IR 事件 |
|:--|:--|
| 第一个 chunk | `EventStart`（`ID = responseId`） |
| 文本 part | `EventText`；思考 part → `EventThinking` |
| `functionCall` part | `EventToolStart` → 一个 `EventToolArgs`（`args` 的紧凑 JSON 原样）→ `EventToolEnd`。Gemini 的 `functionCall` 总是完整出现在一个 chunk 中 |
| `usageMetadata` | `EventUsage`（累计值，后到覆盖先到） |
| `finishReason` 非空 | `EventStop`（映射同上表） |
| 上游 EOF，且此前出现过 `finishReason` | `EventEnd` |
| 上游 EOF 但从未出现 `finishReason` | 流错误（`OutcomeNetworkError`，同 Anthropic Decoder 的「`message_stop` 前结束」）。同协议透传（STR-G-02）不检查这一点，转译路径必须检查，否则客户端无法发现截断 |
| 带顶层 `error` 的 chunk | 流错误，分类同 STR-G-01 |

### 10.6 IR → Gemini 响应（Encoder）

非流式：

```json
{
  "candidates": [{"content": {"role": "model", "parts": [...]}, "finishReason": "<见下表>", "index": 0}],
  "usageMetadata": {"promptTokenCount": <Input+CacheRead+CacheWrite>, "candidatesTokenCount": <Output>,
                    "totalTokenCount": <两者之和>, "cachedContentTokenCount": <CacheRead>},
  "modelVersion": "<客户端请求的模型名>",
  "responseId": "<IR ID；为空时网关签发>"
}
```

`cachedContentTokenCount` 为 0 时省略。

| IR | Gemini |
|:--|:--|
| `TextPart`（含 `IsRefusal`） | `{"text"}` |
| `ThinkingPart` | 丢弃（与 T43 B4 一致：只有 Chat 客户端输出思考内容） |
| `ToolCallPart` | `{"functionCall":{"id","name","args"}}`：带上 `id`，客户端回传 `functionResponse.id` 时 Lower 能按 GID-04 精确对应；`args` 原样输出（§9.3 第 3 条），为空时 `{}` |
| `StopEndTurn`、`StopToolUse`、`StopSequence` | `STOP` |
| `StopMaxTokens` | `MAX_TOKENS` |
| `StopRefusal`，或含 `IsRefusal` 的文本 | `SAFETY` |

流式：

| 编号 | 规则 |
|:--|:--|
| GE-01 | 每个 IR 文本事件输出一个 chunk：`candidates[0].content = {"role":"model","parts":[{"text"}]}`，不带 `finishReason` |
| GE-02 | 工具调用：把同一个工具调用的 `EventToolArgs` 拼接起来，在 `EventToolEnd`（或下一个 `EventToolStart`、`EventStop`、`EventEnd`）时输出一个只含该 `functionCall` 的 chunk。拼出的不是 JSON 对象 → 流错误；拼接缓冲超过 1 MiB → 流错误（禁止无界缓冲） |
| GE-03 | `EventEnd` 时输出最后一个 chunk：`parts` 为空数组，带 `finishReason` 与 `usageMetadata`（字段同非流式） |
| GE-04 | **不输出** `[DONE]` 或其他结束标记；最后一个 chunk 之后 Encoder 返回 `io.EOF` |
| GE-05 | 每个 chunk 都带 `modelVersion` 与 `responseId`（整个流中相同） |

### 10.7 必测用例

除 §7 的通用用例外，每个 Gemini 方向还要覆盖：工具 ID（GID-01 ~ GID-04，含并行两个同名调用）；占位签名（GSIG-02）；Schema 转换（GL-03）；§10.5 每一类 `finishReason` 与 `promptFeedback.blockReason`；用量的三个特殊字段；流式 EOF 前无 `finishReason`；GE-02 的非对象与超限；§10.3 中每个标为 400 与丢弃的字段；`TestStreamMatrix` 全部组合；REG-04 的用量一致性。

### 10.8 决策记录（2026-11-10 用户确认）

1. **占位思考签名（GSIG-02）**。Google 文档要求 Gemini 3 在当前轮的工具调用历史中回传签名，缺失时 400；占位值 `skip_thought_signature_validator` 可以绕过校验，但官方说明它会降低模型表现，只应作为最后手段。Vercel AI SDK、LiteLLM 都在跨模型时自动注入它。
   - **采用 a**：注入，但上游模型名以 `gemini-1.` 或 `gemini-2.` 开头时不注入。旧模型不要求签名，这样可避开它们拒绝未知字段的风险，以后的 `gemini-3.x`、`gemini-4` 等自动覆盖。
   - b. 对所有模型都注入（阶段 B 需要用真实上游验证 Gemini 2.5 接受它）。
   - c. 不注入：Claude Code / Codex CLI 用 Gemini 3 做多轮工具调用会直接 400。
2. **`countTokens` 在转译路径上（GL-01）**。
   - **采用 a**：400。Translator 只能生成上游调用，不能本地作答。
   - b. 修改内核让 Translator 本地返回估算结果：要改冻结的 `internal/core` / `internal/protocol`，不建议。
   - 阶段 B 的真实客户端验证要确认 Gemini CLI 在 `countTokens` 失败时能否继续对话；不能的话，部署文档写明「Gemini CLI 只能使用 Gemini 上游」。
3. **工具 Schema 字段（§10.4）**。
   - **采用 a**：`parametersJsonSchema`：标准 JSON Schema 原样透传，`const`、`additionalProperties` 等关键字不丢失，也不用写转换。
   - b. 转换成 OpenAPI 风格的 `parameters`：兼容更旧的模型，但要写反向转换并删除 Gemini 不支持的关键字。
   - 阶段 B 用真实上游确认目标模型接受 `parametersJsonSchema`；不接受时改为 b，并作为 SPEC-GAP 报告。
