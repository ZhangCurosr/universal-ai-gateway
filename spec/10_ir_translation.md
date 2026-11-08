# 10 跨协议转译（M2：IR & Translation）

> **M1 不实现本章。** M1 完成（任务 T01–T14 全部 `DONE`）后才开始 T20 起的任务。
> 代码位置：`internal/ir`（类型已提供）、`internal/translate`（T20–T24）。

## 0. 接入方式

转换器实现 `protocol.Translator`（`internal/protocol/translate.go`）：`Request(env, upstreamModel) (*protocol.Call, error)` 生成上游协议的 Call（`Op` 取上游协议的操作名常量，`Body` 为转换后的请求体）；`Response(env, resp) (*protocol.Response, error)` 转换上游响应：`ResponseBuffered` 转换 `Body`，`ResponseEvents` 返回包装了 `resp.Events` 的新 `EventStream`（逐个转换事件，上游 `io.EOF` 时先输出收尾事件再返回 `io.EOF`）。实现放在 `internal/translate/`，每个方向一个类型（如 `OpenAIToAnthropic`），由 `translate.Register(r *protocol.Registry) error` 统一注册，并在 `cmd/gateway/adapters.go` 中调用。**内核的转译路径在 M1 的 T10 中已经实现并用假转换器测试过（CORE-04），M2 只需要编写转换器本身，不修改内核。**

`internal/translate` 不得导入具体适配器包；需要的协议常量直接写字符串（如 `protocol.Protocol("anthropic")`），并在包内集中定义。

## 1. 原则

1. 转译只用于「入站协议 ≠ 上游协议」。同协议永远走透传（08）。
2. 转译是**有损**的：无法表达的特性必须**明确拒绝**（400 `invalid_request`，消息说明哪个特性不支持）或**明确丢弃**（下表标注「丢弃」），**禁止**默默编造等价物。
3. 思考签名、加密推理内容**只能回传给签发它的厂商**；跨厂商一律丢弃（详见 06_architecture_review ADR-03）。
4. 入站解析（Lower）与出站合成（Synthesize）分开写，每个方向独立测试：`openai → IR`、`IR → anthropic`、`anthropic → IR`、`IR → openai`。

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
| `tools[type=function]` | `Tool{name, description, parameters}` | 非 function 类型 → 400 |
| `tool_choice` `"auto"`/`"none"`/`"required"`/`{function:{name}}` | `auto` / `none` / `any` / `tool+Name` | |
| `max_completion_tokens`，否则 `max_tokens` | `MaxTokens` | |
| `temperature`、`top_p`、`stop`（字符串或数组）、`stream`、`user` | 对应字段 | |
| `n > 1` | — | 400（不支持） |
| `response_format`、`logprobs`、`audio`、`modalities` | — | 400（不支持） |
| `reasoning_content`（历史 assistant 消息） | — | 丢弃 |

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
| `stop_reason`: `end_turn`/`stop_sequence` → `stop`；`max_tokens` → `length`；`tool_use` → `tool_calls`；`refusal` → `content_filter` | `choices[0].finish_reason` |
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

Anthropic 入站 → OpenAI 上游：按 §3/§4 的逆映射实现；任务卡 T23/T24 中给出用例表。M2 开始前会补充本节的详细字段表。

## 7. 必测用例（每个方向至少）

1. 纯文本单轮；2. 多轮对话；3. system 提示；4. 单个工具调用及其结果回填；5. 并行两个工具调用；
6. 图片输入（base64）；7. 流式文本；8. 流式工具调用（参数分多个 delta）；9. 不支持的字段 → 400；
10. 思考块跨厂商被丢弃。测试数据放在 `internal/translate/testdata/<用例名>/{input.json, expected.json}`，用黄金文件比较。
