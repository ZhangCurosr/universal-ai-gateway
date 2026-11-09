# T43 响应转换经过 IR（流式 IR）

- **类型**：规格 + 重构（**对外行为不变**）
- **前置**：无。但它与 T42 都会改 `internal/translate`，不要同时进行，建议在 T42 合入之后开工。
- **需要用户审批**：**是**。先做阶段 A（§3 整理进 spec/10 并提交审批），批准后再做阶段 B。
- **必读**：
  - [`spec/10_ir_translation.md`](../../spec/10_ir_translation.md) 全文，尤其是 §2、§5、§6.3、§8.3
  - [`internal/ir/types.go`](../../internal/ir/types.go)
  - `internal/translate` 下现有的 4 个响应转换器和公共事件生成器：

    | 文件 | 方向 | 行数 |
    |:--|:--|:--|
    | `chat_from_anthropic.go` | Anthropic 上游 → Chat 客户端 | 约 340 |
    | `responses_from_anthropic.go` | Anthropic 上游 → Responses 客户端 | 约 300 |
    | `anthropic_from_chat.go` | Chat 上游 → Anthropic 客户端 | 约 330 |
    | `responses_from_chat.go` | Chat 上游 → Responses 客户端 | 约 200 |
    | `responses_emit.go` | Responses 事件生成器（T27 抽出，前两个 Responses 方向共用） | 约 180 |

  - [`internal/translate/register.go`](../../internal/translate/register.go)：4 个转换器类型

## 1. 背景

请求方向已经经过 IR：每个客户端协议一个解析器（Lower），每个上游协议一个合成器（Synthesize），成本是 N + M。
**响应方向没有经过 IR**：每对（上游协议，客户端协议）单独写了一个转换器，包括非流式和流式状态机，成本是 N × M。
`ir.Response` 类型虽然定义了，但全仓库没有使用。

M3 计划加入 Gemini 协议。按现在的结构，Gemini 和现有协议之间每多一个互通方向，就要多写一个完整的流式状态机。
本卡把响应转换改成：

```
上游响应/流 ──Decoder（每个上游协议一个）──▶ IR 事件 / ir.Response ──Encoder（每个客户端协议一个）──▶ 客户端响应/流
```

做完之后，新增一个协议只需要写 1 个 Decoder + 1 个 Encoder（如果它既能做上游又能做客户端）。

**硬约束：对外行为完全不变。** 现有黄金文件与端到端测试是这次重构的安全网，必须逐字节通过。

## 2. 现有行为中必须保留的细节

重构前请逐条确认现有代码确实如此，再设计 IR，确保这些行为都能表达出来：

| # | 行为 | 现在的位置 |
|:--|:--|:--|
| B1 | Chat 客户端的 `id` = 上游 Anthropic 消息 id 去掉 `msg_` 后加 `chatcmpl-`；Anthropic 客户端的 `id` = 上游 Chat id 去掉 `chatcmpl-` 后加 `msg_`；上游没有 id 时由网关签发 | `chatID`、`anthropicMsgID` |
| B2 | Responses 客户端的 `resp_` / `msg_` / `fc_` ID **总是**由网关签发（RX-05），不使用上游 id | `respEmitter` |
| B3 | 用量口径：Anthropic 的 `input_tokens` 不含缓存；Chat 的 `prompt_tokens` 含缓存（缓存命中先读 `prompt_tokens_details.cached_tokens`，没有时读 `prompt_cache_hit_tokens`） | `anthropicUsage`、`chatInUsage` |
| B4 | 思考内容：发给 Chat 客户端时作为 `reasoning_content` 输出；发给 Responses 客户端和 Anthropic 客户端时丢弃 | 4 个文件各自处理 |
| B5 | Responses 客户端：连续的文本合并为一个 message 项，遇到工具调用或流结束才关闭；工具调用之后的文本另起一个 message 项 | `respEmitter.textDelta/closeText` |
| B6 | Anthropic 客户端（来自 Chat）：文本与每个工具调用各占一个 content block，index 从 0 递增；`message_start` 的用量全为 0，真实用量放在 `message_delta` | `anthropicStream` |
| B7 | Chat 客户端（来自 Anthropic）：只有原始请求带 `stream_options.include_usage: true` 时，才在 `[DONE]` 之前输出 `choices: []` 的 usage 块 | `includeUsage`、`chatStream` |
| B8 | 停止原因映射与兜底：未列出的 Anthropic `stop_reason` → Chat `stop`、Responses `completed`；未列出的 Chat `finish_reason` → Anthropic `end_turn`；`max_tokens`/`length` → Responses `incomplete` + `max_output_tokens` | `finishReasonFor`、`statusFor`、`stopReasonFor`、`chatStatusFor` |
| B9 | Chat 上游的 `refusal`：发给 Anthropic 客户端时是一个单独的 text 块并且 `stop_reason: refusal`；发给 Responses 客户端时与正文拼接成同一个 output_text | `chatToAnthropicBody`、`chatToResponsesBody` |
| B10 | 流异常：上游 error 事件或 error chunk、在结束标记（`message_stop` / `[DONE]`）之前断流、`[DONE]` 前没有 `finish_reason`、Chat 工具参数交错输出，都返回 `*protocol.AttemptError`，Outcome 分类与现在相同 | 4 个文件的 `Next/handle` |
| B11 | 非流式：Chat 上游 `choices` 不是恰好 1 个、工具参数不是 JSON 对象，返回 `upstream_error` | `chatToAnthropicBody`、`chatToResponsesBody` |
| B12 | 无参数的工具调用：参数为空时输出 `{}` | 多处 |

执行者如果发现上表与代码不符，以**代码和黄金文件**为准，并在阶段 A 的审批请求里指出差异。

## 3. 规格草案（阶段 A：整理进 spec/10 后提交审批）

> 建议作为 spec/10 新增的 §9「响应的 IR」，并修改 §2 的 IR 类型列表。标了「待定」的地方请用户回答。

### 3.1 IR 类型（`internal/ir`）

```go
// TokenUsage 是规范化的 token 用量。Input 不含缓存（Anthropic 口径）。
type TokenUsage struct {
	Input, CacheRead, CacheWrite, Output int64
}

// Response 的 Usage 改为 TokenUsage（该类型目前没有使用者）。
type Response struct {
	ID         string     // 上游 id，已去掉上游协议自己的前缀（如 msg_、chatcmpl-）；可以为空
	Model      string
	Parts      []Part     // TextPart / ToolCallPart / ThinkingPart，按上游顺序
	StopReason StopReason // 无法识别的上游停止原因映射为 StopEndTurn
	Usage      TokenUsage
}

// StreamEventKind 是流式 IR 事件的类型。
type StreamEventKind int

const (
	EventStart         StreamEventKind = iota // 流开始；ID 字段有效（可能为空）
	EventText                                 // 文本增量；Text 有效（非空）
	EventThinking                             // 思考增量；Text 有效（仅供展示，签名不进入流式 IR）
	EventToolStart                            // 工具调用开始；ToolIndex、ToolID、ToolName 有效
	EventToolArgs                             // 工具参数增量；ToolIndex、Text（JSON 片段）有效
	EventToolEnd                              // 工具调用结束；ToolIndex 有效（Encoder 可以忽略，按需隐式关闭）
	EventUsage                                // 用量（累计值，后到的覆盖先到的）；Usage 有效
	EventStop                                 // 停止原因；Stop 有效
	EventEnd                                  // 上游正常结束（收到结束标记）。之后 Decoder 返回 io.EOF
)

type StreamEvent struct {
	Kind      StreamEventKind
	ID        string
	Text      string
	ToolIndex int    // 本响应内第几个工具调用，从 0 开始
	ToolID    string // 上游工具调用 id（Anthropic tool_use.id / Chat tool_calls[].id）
	ToolName  string
	Usage     TokenUsage
	Stop      StopReason
}
```

`StopReason` 增加取值，覆盖现有的映射：已有 `end_turn`、`max_tokens`、`tool_use`、`stop_sequence`、`refusal`；*（待定 1：要不要增加 `pause_turn`，或者保持「无法识别 → end_turn」？建议保持现状，与 B8 一致。）*

### 3.2 Decoder 与 Encoder（`internal/translate`，未导出）

```go
// 上游 → IR
type decoder interface {
	Body(body []byte) (*ir.Response, error)                  // 非流式；B11 的错误在这里返回
	Stream(in protocol.EventStream) irStream                 // 流式
}
type irStream interface {
	Next() (ir.StreamEvent, error) // B10 的错误以 *protocol.AttemptError 返回；正常结束先给 EventEnd 再给 io.EOF
	Close() error                  // 关闭上游
}

// IR → 客户端
type encoder interface {
	Body(resp *ir.Response, env *protocol.Envelope) ([]byte, error)
	Stream(in irStream, env *protocol.Envelope) protocol.EventStream
}
```

- Decoder：`anthropicDecoder`、`chatDecoder`。
- Encoder：`chatEncoder`、`anthropicEncoder`、`responsesEncoder`（基于现有的 `respEmitter`）。
- Encoder 需要的请求信息（客户端模型名、`include_usage`）从 `env` 读取；时钟与 ID 生成器作为 Encoder 的字段注入，测试时替换。
- 转换器的 `Response` 方法统一为：`ResponseBuffered` → `encoder.Body(decoder.Body(...))`；`ResponseEvents` → `encoder.Stream(decoder.Stream(...))`；`ResponseRaw` 原样返回。

### 3.3 不变量（写进 spec）

1. Decoder 只做「上游协议 → 规范化事件」，不读 `env`，不知道客户端协议。
2. Encoder 只做「规范化事件 → 客户端协议」，不知道上游协议。B4、B5、B6、B7、B9 里因客户端而异的选择都由 Encoder 决定。
3. 用量与计费仍然取上游原始响应的 `Meter`（CORE-04），IR 里的用量只用于给客户端展示。
4. Decoder 不产生空文本事件，也不产生空的 TextPart。*（待定 2：现在 `responses_from_anthropic.go` 的非流式路径，在上游给出空 text 块时会生成一个空的 message 项。黄金文件没有覆盖这种情况。是统一为「不输出」，还是保留现状？建议统一为不输出，并在 spec 中写明。）*

## 4. 实现步骤（阶段 B：用户批准后）

每一步结束时 `go run ./tools/check` 都必须是绿的，黄金文件一个字节都不能变。

1. **IR 类型**：在 `internal/ir` 中加入 §3.1 的类型，为 `TokenUsage` 的辅助方法写单元测试（如果有的话）。
2. **Decoder**：新建 `decode_anthropic.go`、`decode_chat.go`，把现有 4 个文件里「解析上游」的部分搬过来，包括 B3、B8 的上游侧、B10、B11。
   为它们写独立的单元测试：输入沿用 `testdata/anthropic_to_*`、`testdata/chat_to_*` 下现有的 `input.json` / `input.sse`，断言产出的 IR（可以新增 `testdata/decode_*/expected.json` 黄金文件）。
3. **Encoder**：新建 `encode_chat.go`、`encode_anthropic.go`、`encode_responses.go`，把「生成客户端格式」的部分搬过来。`responses_emit.go` 成为 `encode_responses.go` 的一部分或保持独立文件。
4. **切换转换器**：逐个把 4 个转换器的 `Response` 方法改为「Decoder + Encoder」。**每切换一个，跑一次对应方向的黄金测试**，确认逐字节一致后再切下一个。
5. **删除旧代码**：删除 4 个 `*_from_*.go` 中已经搬走的代码。测试文件中直接调用旧内部函数的地方（例如 `anthropicToChatBody`、`finishReasonFor`、`chatID`、`stopReasonFor`），改为调用新函数。**只能改调用的函数名，不能改输入和期望值。**
6. **组合测试**：新增一个测试，对每个 Decoder 的每个流式黄金输入，接上每一个 Encoder，断言不报错、事件序列满足各客户端协议的结构约束（复用现有的 `checkSequence`、`checkChatChunks`、`checkAnthropicEvents`）。
   这一步顺带覆盖了「Anthropic 上游 → Anthropic 客户端」这类目前不存在的组合，用来证明结构是通的；它不需要黄金文件。
7. **文档**：更新 `internal/translate/doc.go`，说明 Lower / Synthesize / Decode / Encode 四类组件，以及新增协议时要写哪些。

## 5. 涉及文件

- spec：`spec/10_ir_translation.md`（§2、新增 §9）、`spec/11_tasks.md`（登记本卡）
- IR：`internal/ir/types.go`（可以新增 `internal/ir/stream.go`）
- 转译：`internal/translate/` 下的 `*_from_*.go`、`responses_emit.go`、`register.go`、`doc.go`，新增的 `decode_*.go`、`encode_*.go`，以及对应测试
- **不要改**：`internal/translate/testdata/` 下现有的任何 `expected.*`；`cmd/gateway/e2e_test.go`；`internal/core`、`internal/protocol`

## 6. 验收标准

- [ ] 阶段 A 的 spec 提交已获用户批准。
- [ ] 现有全部黄金文件**逐字节不变**并通过：`anthropic_to_chat`、`anthropic_to_responses`、`chat_to_anthropic`、`chat_to_responses`，以及请求方向的全部用例。
- [ ] `cmd/gateway` 的 4 个转译端到端测试不变并通过。
- [ ] §2 的 B1–B12 每一条都有测试覆盖。交付说明中给出「行为编号 → 测试函数名」的对照表。
- [ ] Decoder 和 Encoder 各自有独立的单元测试；§4 第 6 步的组合测试通过。
- [ ] 4 个旧的 `*_from_*.go` 已删除，或者只剩少量与具体方向相关的胶水代码。交付说明中给出重构前后 `internal/translate` 非测试代码的行数对比。
- [ ] `ir.Response` 被实际使用。
- [ ] `go run ./tools/check` 全绿。
- [ ] 交付说明中写一段「新增一个协议（例如 Gemini）需要写哪些组件」的说明，作为 M3 的输入。

## 7. 建议提交拆分

1. 阶段 A：`docs(spec): T43 响应的 IR 与流式 IR 事件（待审批）`
2. `feat(ir): T43 新增 TokenUsage 与流式 IR 事件类型`
3. `refactor(translate): T43 新增 Anthropic / Chat 响应 Decoder`
4. `refactor(translate): T43 新增 Chat / Anthropic / Responses 响应 Encoder`
5. `refactor(translate): T43 转换器改为 Decoder + Encoder 组合，删除旧的成对转换代码`

## 8. 风险提示

- 最容易出错的是**事件的先后顺序**，例如 Responses 里 `output_item.done` 与下一个 `output_item.added` 的相对顺序、Anthropic 里 `content_block_stop` 的时机。黄金文件会抓到这类问题；出现差异时，先修代码，不要怀疑黄金文件。
- `respEmitter` 的「工具调用打开时先关闭文本」这类隐式关闭逻辑，要放在 Encoder 里，不要放进 Decoder。
- 有缓存时 `TokenUsage.Input` 必须是「不含缓存」的值。Chat Decoder 要用 `prompt_tokens − cached` 计算，结果小于 0 时取 0。这与 openai 出站适配器 USE-03 的口径一致。
