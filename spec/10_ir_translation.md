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
type TextPart struct { Text string; CacheControl *CacheControl }
type ImagePart struct { MediaType string; Data []byte; URL string }
type ToolCallPart struct { ID, Name string; Arguments json.RawMessage }
type ToolResultPart struct { ToolCallID string; Content []Part; IsError bool }
type ThinkingPart struct { Vendor ir.Vendor; Text string; Signature string; Redacted bool; RedactedData string }
type Tool struct { Name, Description string; InputSchema json.RawMessage; CacheControl *CacheControl }
type ToolChoice struct { Mode string; Name string }    // auto | any | none | tool
type CacheControl struct { Type string; TTL string }
type Response struct { ID, Model string; Parts []Part; StopReason StopReason; Usage pricing.Usage }
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
| `messages[role=user\|assistant].content` 字符串 | 一个 TextPart | 其他 role → 400 |
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

§7 的 10 个用例全部适用，另加：服务端工具 → 400；`top_k` → 400；`thinking` 被丢弃；`tool_result` 带 `is_error`；
`stop_sequences` 超过 4 个 → 400；流式 usage chunk 映射到 `message_delta.usage`；`content_filter` → `refusal`。

### 6.5 待定项（起草时的取舍，审批时请确认）

1. `max_tokens` 而非 `max_completion_tokens`：前者兼容 DeepSeek / Qwen 等，后者是 OpenAI 官方 o 系列推理模型的要求。若需要直连 OpenAI 官方推理模型，应改为可按池配置。
2. `reasoning_content` → Anthropic 一律丢弃：保守做法；代价是 Claude Code 看不到 DeepSeek 的思考过程。
3. `thinking` 参数丢弃而不是 400：否则 Claude Code（默认开启 thinking）无法使用 Chat 上游。
4. `top_k`、`disable_parallel_tool_use` 选择 400 而不是丢弃：按 §1 第 2 条「改变采样/调用语义的不默默丢弃」。

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
| 第一段文本 | `response.output_item.added`（message 项）→ `response.content_part.added`（output_text，`text: ""`） |
| 每段文本增量 | `response.output_text.delta`（`item_id`、`output_index`、`content_index: 0`、`delta`） |
| 文本结束（遇到工具调用或流结束） | `response.output_text.done`（完整 text）→ `response.content_part.done` → `response.output_item.done` |
| 工具调用开始 | `response.output_item.added`（function_call 项，`arguments: ""`） |
| 工具参数增量 | `response.function_call_arguments.delta` |
| 工具调用结束 | `response.function_call_arguments.done`（完整 arguments）→ `response.output_item.done` |
| 上游正常结束 | `response.completed`（完整 response，含 `usage`）；达到输出上限或 `refusal` 时改为 `response.incomplete` |
| 上游流中错误 | 入站适配器的 `StreamError`（IN-R07）后结束 |

思考增量（Anthropic `thinking_delta`、Chat `reasoning_content`）不输出。`output_index` 按项出现顺序从 0 编号。

自 Chat 上游（T27）时的来源映射：文本 = `delta.content` / `message.content`，Chat 上游的 `refusal` 文本亦作为 `output_text` 输出（非流式与流式均拼接为文本，不生成 `refusal` 内容部件）；工具调用 = `tool_calls[]`（按 `index` 区分，交错输出 → 流错误）；
`finish_reason: length` → `incomplete`，其余 → `completed`；`usage.input_tokens = prompt_tokens`（已含缓存）、
`input_tokens_details.cached_tokens = prompt_tokens_details.cached_tokens`、`output_tokens = completion_tokens`；
流式在收到 `[DONE]` 时输出 `response.completed`（用量取之前的 usage chunk；§6.2 保证合成请求带 `include_usage`）。

### 8.4 必测用例

§7 的 10 个用例全部适用，另加：`previous_response_id` → 400；内置工具 → 400；`input` 字符串与数组两种形式；`function_call_output` 回填后的多轮；达到输出上限 → `incomplete`。
