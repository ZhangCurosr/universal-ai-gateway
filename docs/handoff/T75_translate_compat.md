# T75 转译兼容：Claude Code 的 system 消息与 Codex 的 Responses 事件（已批准，已完成）

- **类型**：规格 + 实现（小卡，修复）。2026-11-14 用户批准全部建议（「全部按建议」）。
- **前置**：T73（`DONE`，实测中发现这两个问题）。
- **约束**：只改 `internal/translate` 与 spec/10；不改 `internal/core`、`internal/protocol` 的导出 API；不加依赖。透传路径（客户端协议与上游协议相同）不受影响。

## 1. 问题与原因（已查明）

| # | 现象 | 原因 |
|:-:|:--|:--|
| 1 | Claude Code 2.1.316 经网关用**非 Anthropic 协议**的服务（如 OpenAI Chat 上游）时，所有请求返回 400 `messages[1].role must be user or assistant` | Claude Code 会在 `messages` 数组里发送 `role: "system"` 的消息（实测角色序列 `["user", "system"]`，内容是 `<system-reminder>` 这类附加说明）。Anthropic → IR 的转换（`anthropic_lower.go`）只接受 user / assistant |
| 2 | Codex 0.166 经 `/v1/responses`（转译到其他上游）时打印 `ERROR codex_core::util: OutputTextDelta without active item`，回答虽然正常，但 Codex 没有把这段文本记成一个消息项 | 抓到的事件流里，`response.output_item.added` 的 message 项**没有 `content` 字段**：代码里是空数组，但 JSON 标签带 `omitempty` 被省略了。Codex 按 OpenAI 的格式解析 message 项时要求 `content`，解析失败就忽略了这个项，之后的文本增量找不到所属的项 |

## 2. 规则

| 编号 | 规则 |
|:--|:--|
| TC-01 | Anthropic 请求的 `messages[]` 允许 `role: "system"`（转译路径）：出现在第一条 user / assistant 消息**之前**的，与顶层 `system` 一样追加到 IR 的 `System`；出现在**之后**的，内容（只允许文本，否则 400）作为 user 文本保留在原位置：上一条 IR 消息是 user 时追加到它的末尾，否则新建一条 user 消息；紧随其后的 user 消息也并入这一条（不产生连续两条 user 消息，Gemini 等上游要求角色交替）。其他角色仍然 400 |
| TC-02 | Responses 流式事件与非流式响应中，`type: "message"` 的输出项**总是**带 `content` 字段（刚打开时为 `[]`）；`function_call` 项不带 `content`（与 OpenAI 一致） |

## 3. 改动文件

- `internal/translate/anthropic_lower.go`（TC-01）、`encode_responses.go`（TC-02：message 项的序列化）。
- 测试：`anthropic_lower_test.go`（system 在前、在中间、在末尾、紧跟 user、含非文本内容 → 400）；`encode_responses_test.go`（`output_item.added` 的 message 项含 `"content":[]`，function_call 项不含）；`real_clients_test.go` 增加 Claude Code 2.1.316 的请求形状。
- 规格：spec/10 §6.1（Anthropic → IR 的 messages 表）加 TC-01，§8.3 加 TC-02；spec/11 新增 T75。

## 4. 实测（验收）

本地假上游（OpenAI Chat 协议，总是回答 `pong`）+ 本机 Claude Code 与 Codex：

1. `claude -p` 对 OpenAI Chat 协议的服务回答 `pong`（之前 400）。
2. `codex exec` 不再打印 `OutputTextDelta without active item`。
3. 按 `live-test` 技能清理。

### 4.1 实测结果（2026-11-14，通过）

- [x] Claude Code 2.1.316 `claude -p` 对 OpenAI Chat 协议的服务回答 `pong`（之前 400），上游收到的是转译后的 `/v1/chat/completions` 请求。
- [x] Codex 0.166 `codex exec` 回答 `pong`，不再打印 `OutputTextDelta without active item`。
- [x] 12 个 Responses 黄金文件的变化只有 message 项多了 `"content": []`；`TestLowerAnthropicRejects` 中原来用 `system` 演示「非法角色」的用例改为 `tool`（system 现在按 TC-01 接受），另加「system 消息含图片 → 400」。

## 5. 决策（已批准）

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 对话中间的 system 消息放到哪里 | A. 开头的并入 System，中间的作为 user 文本留在原位置（TC-01）；B. 全部并入 System（丢失位置）；C. 保持 400 | **A**：Claude Code 在对话中间插入的说明只和当前这一轮有关，放到全局 System 会让模型以为是长期指令；留在原位置最接近原意，也不需要上游支持 system 角色 |
| 2 | 中间的 system 文本要不要加标记（如 `[system]` 前缀） | A. 原样保留；B. 加前缀 | **A**：Claude Code 的内容本身已经用 `<system-reminder>` 包起来；再加前缀反而改变了内容 |
