# T43 交付说明：响应转换经过 IR（流式 IR）

- 实现：`task/t43-stream-ir` 分支前 5 个提交（`63f9976` … `bb7c258`）
- 验收修复：`21692bd`
- 合入 main：`6efea9e`
- 检查：`go run ./tools/check` 全部通过（Windows，无 `-race`；Linux `-race` 复跑见 [T46 §9](../T46_race_verify_linux.md#9-复跑)）

## 1. 结构

```
上游响应/流 ──Decoder──▶ ir.Response / ir.StreamEvent ──Encoder──▶ 客户端响应/流
```

| 组件 | 文件 | 说明 |
|:--|:--|:--|
| 接口 | `decode.go`、`encode.go` | `decoder{Body, Stream}`、`irStream{Next, Close}`、`encoder{Body, Stream}` |
| Decoder | `decode_anthropic.go`、`decode_chat.go` | 上游 → IR；不读 `env`，不知道客户端协议 |
| Encoder | `encode_chat.go`、`encode_anthropic.go`、`encode_responses.go`（+ `responses_emit.go`） | IR → 客户端；不知道上游协议 |
| 组合 | `register.go` 的 `translateBuffered` / `translateEvents` | 4 个转换器的 `Response` 方法统一为 Decoder + Encoder |

## 2. 行为编号 → 测试

| # | 行为 | 测试 |
|:--|:--|:--|
| B1 | Chat / Anthropic 客户端 id 前缀互换；缺失时签发 | `TestAnthropicToChatUsageAndID`（含 `chatID`）、`TestAnthropicMsgID`；各方向黄金文件 |
| B2 | Responses 的 `resp_` / `msg_` / `fc_` 总由网关签发 | `TestResponsesEncoderBody`、`TestResponsesEncoderStream`、`TestNewGatewayID`；`*_to_responses` 黄金文件 |
| B3 | 用量口径（Anthropic 不含缓存；Chat 含缓存，`cached_tokens` 优先，`prompt_cache_hit_tokens` 兜底） | `TestAnthropicToChatUsageAndID`、`TestChatDecoderBody`；黄金 `chat_to_*/01_text`、`04_tool_call`（cached_tokens）、`05_text_then_two_tools`（prompt_cache_hit_tokens） |
| B4 | 思考：Chat 客户端输出 `reasoning_content`，其他客户端丢弃 | 黄金 `anthropic_to_chat/10_thinking_reasoning`、`anthropic_to_responses/10_thinking_dropped`、`chat_to_*/10_reasoning_dropped` |
| B5 | Responses 连续文本合并为一个 message 项，工具调用后另起 | 黄金 `*_to_responses/05_text_then_two_tools`；`TestResponsesEncoderBody` |
| B6 | Anthropic 客户端块 index 从 0 递增；`message_start` 用量为 0 | `checkAnthropicEvents`（`TestChatToAnthropicStreamGolden`）、`TestAnthropicEncoderStream` |
| B7 | 仅 `include_usage: true` 时输出 usage chunk | `TestIncludeUsage`、`TestAnthropicToChatStreamWithoutUsage`、`TestAnthropicToChatStreamGolden` |
| B8 | 停止原因映射与兜底 | `TestFinishReasonFor`、`TestStopReasonFor`、`TestStatusFor`、`TestChatToResponsesStopReasons`；黄金 `15_*`、`16_content_filter` |
| B9 | Chat 上游 refusal：Anthropic 客户端单独 text 块 + `stop_reason: refusal`；Responses 客户端拼进 output_text | 黄金 `chat_to_anthropic/17_refusal`、`chat_to_responses/17_refusal`；`TestChatDecoderBodyRefusal`、`TestChatToAnthropicStreamRefusal` |
| B10 | 流异常 → `*protocol.AttemptError`，Outcome 不变 | `TestChatToAnthropicStreamErrors`、`TestChatToResponsesStreamErrors`、`TestAnthropicToChatStreamErrors`、`TestAnthropicToResponsesStreamErrors`、`TestChatToResponsesStopReasons`（流式部分） |
| B11 | 非流式 choices 不是 1 个、工具参数不是 JSON 对象 → `upstream_error` | `TestChatToAnthropicBufferedErrors`、`TestChatToResponsesBufferedErrors` |
| B12 | 空参数输出 `{}`；参数原样透传 | `TestChatToAnthropicToolArgsVerbatim`；黄金 `05_text_then_two_tools` |

另外：`TestStreamMatrix` 把每个 Decoder 的流式黄金输入接到每个 Encoder 上（共 24 组），检查各客户端协议的结构约束。`cmd/gateway` 的 4 个转译端到端测试（`TestE2EResponsesToAnthropic`、`TestE2EChatToAnthropic`、`TestE2EAnthropicToChat`、`TestE2EResponsesToChat`）没有改动，全部通过。

## 3. 与重构前相比的行为变化（spec/10 §9.3）

| 变化 | 原因 |
|:--|:--|
| Chat 上游 → Responses 客户端：`content_filter` 由 `completed` 改为 `incomplete` + `content_filter`；`insufficient_system_resource` 由 `completed` 改为上游错误 | 同一个 IR 停止原因对所有客户端含义相同；与 T44 G3/G4 的决策一致 |
| Chat 上游流式 refusal → Anthropic 客户端：以前丢弃，现在输出为 text，`stop_reason: refusal` | 和非流式一致 |
| Anthropic 上游 → Responses 客户端（非流式）：空 text 块不再生成空 message 项 | spec/10 §9.2 第 2 条（T43 卡片「待定 2」的建议方案） |

现有黄金文件都没覆盖这三种情况，全部逐字节不变。

## 4. 行数（`internal/translate` 非测试代码）

| | 重构前（`a9f34be`） | 重构后（`6efea9e`） |
|:--|--:|--:|
| 包内合计 | 3583 | 3953 |
| 响应方向 | 1378（4 个成对转换器 + `responses_emit.go`） | 1736（2 个 Decoder 721 + 3 个 Encoder 1015，含 `responses_emit.go`） |

协议只有 3 个时，响应方向反而多了约 360 行：组件从 4 个成对转换器变成了 2 + 3 个，每个都要处理完整的事件生命周期。收益在新增协议时才体现（见 §5）：成对结构下每多一个互通方向，就要多写一个完整的流式状态机；新结构下每个协议最多写 1 个 Decoder 和 1 个 Encoder。

## 5. 新增一个协议（例如 Gemini）要写什么（M3 输入）

| 用途 | 组件 | 位置 |
|:--|:--|:--|
| 作为客户端协议（客户端用 Gemini 格式调用网关） | 入站适配器 | `internal/adapter/gemini` |
| | 请求解析 Lower：Gemini 请求 → `ir.Request` | `internal/translate/gemini_lower.go` |
| | 响应 Encoder：IR → Gemini 响应 / 流 | `internal/translate/encode_gemini.go` |
| 作为上游协议（网关调用 Gemini 上游） | 出站适配器 | `internal/adapter/gemini` |
| | 请求合成 Synthesize：`ir.Request` → Gemini 请求 | `internal/translate/gemini_synth.go` |
| | 响应 Decoder：Gemini 响应 / 流 → IR | `internal/translate/decode_gemini.go` |
| 接入 | 新的转换器类型（每个客户端 × 上游组合各一个，只是把 Lower / Synthesize / Decoder / Encoder 组合起来的胶水），在 `Registrar` 中注册 | `internal/translate/register.go` |

不需要改 `internal/core` 和 `internal/protocol`。`TestStreamMatrix` 加入新的 Decoder / Encoder 后，会自动覆盖新协议和现有协议的全部流式组合。

需要注意：Gemini 的停止原因、用量口径（是否含缓存）、工具参数（对象而不是字符串）要在 Decoder / Encoder 里映射到 §9 的 IR 约定；IR 表达不了的内容按 spec/10 §1 的原则处理，不编造等价物。

## 6. 未完成或有风险的地方

- 合并后的代码还没在 Linux 上用 `-race` 跑过：要做的事见 T46 §9。
- 阶段 A 的规范和阶段 B 的实现是一起交来的，没有先单独审批；规范内容已在本次验收中由用户确认。
