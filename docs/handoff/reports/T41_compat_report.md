# T41 真实客户端兼容性实测报告

- **日期**：2026-11-09
- **任务**：T41 真实客户端兼容性实测（调研 + 回归用例）
- **依据**：[`spec/10_ir_translation.md`](../../spec/10_ir_translation.md)、[`docs/handoff/T41_real_client_compat.md`](../T41_real_client_compat.md)

---

## 1. 测试环境与版本

| 组件 | 版本 / 标识 | 运行说明 |
|:---|:---|:---|
| **Claude Code** | `2.1.319 (Claude Code)` | 本机已安装，使用 `--setting-sources ""` 与 `--settings` 指定网关接入端点 |
| **Codex CLI** | `codex-cli 0.167.0-alpha.3.2` | 本机已安装（配套桌面端组件 `OpenAI.Codex 26.1103.3517.0`），通过 `-c` 注入自定义 `model_providers.uaig` (`wire_api = "responses"`) |
| **OpenAI Chat 客户端** | 标准 OpenAI Chat 协议兼容客户端（OpenAI Python/Node SDK、Cursor、Cherry Studio 等） | 发起对 `/v1/chat/completions` 的跨协议转译请求 |
| **网关提交号** | `8d9902aa1bc275fff7bc134af36ad9f6f4ebd6a8` (`main` 分支最新 HEAD) | 基于 Go 1.26+ 纯 Go 构建，`tools/check` 检验通过 |
| **上游测试池模型** | `claude-sonnet-4-5` (`anthropic` 池)<br>`deepseek-chat` (`openai_chat` 池)<br>`gpt-5` / `gpt-4o` (`openai_responses` 对照池) | 覆盖 Anthropic Messages、OpenAI Chat Completions、OpenAI Responses 三大主要协议 |

---

## 2. 兼容性实测矩阵（路径 × 场景）

状态定义：
- **通过**：请求能够被当前转译层成功解析并合成为目标协议，且语义与功能完整。
- **失败**：请求由于携带特定不可兼容参数或特性，被当前转译层明确返回 400（`invalid_request`）或引发转换错误。
- **阻塞**：客户端默认行为中必然发送某些未被白名单收录的字段或内置工具，导致在进入该场景前就被前置校验拦截。
- **不适用**：该客户端协议或工具集在对应场景中不存在。

| 编号 | 场景简述 | P1: Claude Code<br>→ `openai_chat` | P2: Codex CLI<br>→ `anthropic` | P3: Codex CLI<br>→ `openai_chat` | P4: OpenAI Chat 客户端<br>→ `anthropic` | 对照组 (同协议透传)<br>Claude / Codex |
|:---|:---|:---:|:---:|:---:|:---:|:---:|
| **S1** | 一句话问答（非流式） | **通过** | **阻塞** ⁽¹⁾ | **阻塞** ⁽¹⁾ | **通过 / 失败** ⁽²⁾ | **通过** |
| **S2** | 一句话问答（流式） | **通过** | **阻塞** ⁽¹⁾ | **阻塞** ⁽¹⁾ | **通过 / 失败** ⁽²⁾ | **通过** |
| **S3** | 读文件并总结（单工具调用+回填） | **通过** | **阻塞** ⁽¹⁾ | **阻塞** ⁽¹⁾ | **通过** | **通过** |
| **S4** | 并行读文件（多工具调用） | **通过** | **阻塞** ⁽¹⁾ | **阻塞** ⁽¹⁾ | **通过** | **通过** |
| **S5** | 连续 5 轮以上对话（历史带思考/加密/缓存） | **通过** | **阻塞** ⁽¹⁾ | **阻塞** ⁽¹⁾ | **通过** | **通过** |
| **S6** | 开启思考/推理（thinking / reasoning.effort） | **通过** | **阻塞** ⁽¹⁾ | **阻塞** ⁽¹⁾ | **失败** ⁽³⁾ | **通过** |
| **S7** | 粘贴图片（多模态 input_image / image） | **通过** ⁽⁴⁾ | **阻塞** ⁽¹⁾ | **阻塞** ⁽¹⁾ | **通过** | **通过** |
| **S8** | 中途按 Esc / Ctrl+C 取消 | **通过** | **阻塞** ⁽¹⁾ | **阻塞** ⁽¹⁾ | **通过** | **通过** |
| **S9** | 客户端内置工具（web_search、namespace、bash等） | **失败** ⁽⁵⁾ | **失败** ⁽⁶⁾ | **失败** ⁽⁶⁾ | **不适用** | **通过** |

**矩阵注释**：
- ⁽¹⁾ **Codex CLI 阻塞原因**：Codex CLI 0.167 无论执行何种操作，默认必发未被网关白名单允许的顶层字段 `client_metadata`，同时其默认工具集加载了类型为 `namespace` 和 `web_search` 的工具；导致进入任何转译场景前均被直接 400 阻断。若在代理层将 `client_metadata` 过滤并将工具集裁剪为仅含 `function` 工具，则 S1–S8 均可**通过**。
- ⁽²⁾ **OpenAI Chat 客户端差异**：标准 OpenAI SDK 若仅传递 model 与 messages 则通过；但主流客户端（如 Cursor、Cherry Studio）通常默认附加 `presence_penalty: 0`、`frequency_penalty: 0`、`seed`、`service_tier` 等，由于不在 `chatTopLevel` 中，会被 400 拦截失败。
- ⁽³⁾ **OpenAI Chat 思考参数**：Chat 客户端发送 `reasoning_effort`（o1/o3 模型参数）或 `response_format: {"type":"json_object"}` 时，当前转译层会直接 400 拒绝。
- ⁽⁴⁾ **Claude Code 多模态**：用户输入的图片块（`type: image`）可通过转译；但若工具执行结果（`tool_result`）返回图片，由于 OpenAI Chat `role: tool` 仅支持文本 content，出站合成器会明确拒绝。
- ⁽⁵⁾ **Claude Code 内置服务端工具**：若声明 `web_search_*` 或 `bash_*` 等 Anthropic 服务端内置工具，转译层按 spec/10 §6.1 明确 400 拒绝。
- ⁽⁶⁾ **Codex 内置工具**：Codex 默认传递 `tools` 中含有 `type: "namespace"`（用于 `multi_agent_v1` 与 `mcp__*` 命名空间）及 `type: "web_search"`，转译层按 spec/10 §8.2 明确 400 拒绝。

---

## 3. 失败清单（按触发原因分类汇总）

分类标准（依据任务卡 §4.2）：
- **A 可以安全丢弃**：只影响客户端提示、调试追踪或计费标签，对生成结果没有语义影响。
- **B 应该映射**：目标上游有等价参数，当前规格未收录或合成层未适配。
- **C 应该继续拒绝**：丢弃会改变生成结果或工具调用语义（例如强制特定格式输出、服务端独占工具、超出协议能力限制）。
- **D 不确定**：需要规格制定方结合业务场景决定的边界行为。

| # | 路径 | 场景 | 字段路径 | 实际取值（脱敏） | 网关错误消息 | 分类 | 建议策略 | 对应回归用例目录 |
|:---|:---|:---|:---|:---|:---|:---:|:---|:---|
| 1 | P2, P3 | S1–S8 全局 | `client_metadata` | `{"thread_id":"<redacted>","turn_id":"<redacted>","session_id":"<redacted>"}` | `client_metadata is not supported for model "claude-sonnet-4-5"` | **A** | **安全丢弃**。这是 Codex CLI 用于本地会话关联的客户端元数据，对模型输出无任何语义影响。放入 `responsesTopLevel` 作为丢弃字段即可打通 Codex CLI 基本接入。 | `codex/err_client_metadata/` |
| 2 | P2, P3 | S9 | `tools[].type` | `"web_search"` | `built-in tool "web_search" is not supported for model "claude-sonnet-4-5"` | **C / B** | **分类讨论**。若目标模型无原生 web_search 能力（如转译到 deepseek-chat 或 claude），丢弃可能导致模型不知道无法搜索。但在智能体客户端场景中，客户端常默认挂载内置工具集。建议：若客户端 `tool_choice` 未强制指定该工具，可由 T42 评估是否作为容忍策略静默过滤，否则拒绝。 | `codex/s9_builtin_web_search/` |
| 3 | P2, P3 | S9 | `tools[].type` | `"namespace"` (工具名 `multi_agent_v1`, `mcp__*`) | `built-in tool "namespace" is not supported for model "claude-sonnet-4-5"` | **A / C** | **评估过滤**。Codex CLI 用于多智能体协作与本地 MCP 插件的命名空间定义，其他协议上游不理解 namespace 工具类型。若客户端并非强制调用，在入站降级时直接剔除非 `function` 类型工具，避免导致整请求瘫痪。 | `codex/s9b_builtin_namespace/` |
| 4 | P2, P3 | S5 | `previous_response_id` | `"resp_01X9TEST"` | `previous_response_id is not supported for model "claude-sonnet-4-5"; send the full input instead` | **C** | **继续拒绝**。符合硬规则 RX-01。转译上游（非 Responses）无状态保存能力，客户端必须发送完整上下文。 | `codex/err_previous_response_id/` |
| 5 | P2, P3 | S5 | `conversation` | `"conv_01X9TEST"` | `conversation is not supported for model "claude-sonnet-4-5"; send the full input instead` | **C** | **继续拒绝**。符合硬规则 RX-01。 | `codex/err_conversation/` |
| 6 | P2, P3 | S1 | `background` | `true` | `background is not supported for model "claude-sonnet-4-5"` | **C** | **继续拒绝**。符合硬规则 RX-03，转译链路不支持离线轮询生成。 | `codex/err_background/` |
| 7 | P2, P3 | S1 | `text.format` | `{"type":"json_schema",...}` | `text.format "json_schema" is not supported for model "claude-sonnet-4-5"` | **C** | **继续拒绝**。Responses 结构化输出依赖上游严格保证，非 Responses 上游无法提供同等契约保障。 | `codex/err_text_format_json/` |
| 8 | P2, P3 | S7 | `input[].content[].file_id` | `{"type":"input_image","file_id":"<redacted>"}` | `input[0].content[0]: image_url required (file_id is not supported when translating to another protocol)` | **C** | **继续拒绝**。`file_id` 为 OpenAI 专属托管资产，无法跨厂商直接读取；多模态转译要求内嵌 data URL 或公网 URL。 | `codex/err_input_file/` |
| 9 | P2, P3 | S1 | 任意未在白名单的顶层字段 | 如 `prompt_cache_clean: true` | `prompt_cache_clean is not supported for model "claude-sonnet-4-5"` | **A / D** | **建议容忍**。当前策略是一律 400，导致客户端升级增添实验性参数时直接失效。T42 应建立明确的忽略策略。 | `codex/err_unknown_top_level/` |
| 10 | P1 | S3, S7 | `messages[].content[].content[]` (tool_result) | 包含 `type: "image"` 的工具结果块 | `messages[2].content[0]: images in tool results are not supported by this upstream` | **C** | **继续拒绝**。OpenAI Chat 的 `role: tool` 仅接收纯字符串文本内容，无法直接表达图片块。 | `claude_code/err_tool_result_image/` |
| 11 | P1 | S9 | `tools[].type` | `"web_search_20250101"` | `built-in tool "web_search_20250101" is not supported for model "claude-sonnet-4-5"` | **C** | **继续拒绝**。Anthropic 服务端内置搜索，无法交由不支持该能力的上游执行。 | `claude_code/s9_builtin_web_search/` |
| 12 | P1 | S9 | `tools[].type` | `"bash_20250124"` | `built-in tool "bash_20250124" is not supported for model "claude-sonnet-4-5"` | **C** | **继续拒绝**。Anthropic 云端沙箱执行工具，Chat 上游无此对应服务端能力。 | `claude_code/s9b_builtin_bash/` |
| 13 | P1 | S1 | `mcp_servers` | `[{"name":"<redacted>","url":"<redacted>"}]` | `mcp_servers is not supported when translating to another protocol` | **A / B** | **安全丢弃或由网关代理**。若由客户端自行连接 MCP 则对请求体无语义影响（丢弃）；若是服务端远程连接则继续拒绝。当前可按 spec/10 保持拒绝。 | `claude_code/err_mcp_servers/` |
| 14 | P1 | S1 | `container` | `{"image":"<redacted>"}` | `container is not supported when translating to another protocol` | **C** | **继续拒绝**。涉及服务端容器环境能力，跨厂商不可迁移。 | `claude_code/err_container/` |
| 15 | P1 | S1 | `top_k` | `40` | `top_k is not supported when translating to another protocol` | **A** | **建议安全丢弃**。OpenAI Chat 上游普遍无 `top_k` 参数，绝大多数客户端在采样时即使忽略 `top_k` 依然能稳定生成，不应因一个辅助采样参数 400 截断会话。 | `claude_code/err_top_k/` |
| 16 | P1 | S3 | `tool_choice.disable_parallel_tool_use` | `true` | `tool_choice.disable_parallel_tool_use is not supported when translating to another protocol` | **C** | **继续拒绝**。Chat 转译无法精确保证上游单工具顺序调用语义（OpenAI 对应的是 `parallel_tool_calls: false`，IR 契约目前无法表达）。 | `claude_code/err_disable_parallel_tool_use/` |
| 17 | P1 | S7 | `messages[].content[].type` | `"document"` | `messages[0].content[0].type document is not supported when translating to another protocol` | **C** | **继续拒绝**。Anthropic 原生 Citations/PDF document 块，普通 Chat 上游无法原生解析。 | `claude_code/err_document_block/` |
| 18 | P1 | S1 | `stop_sequences` | 包含超过 4 个元素 | `stop_sequences: at most 4 are supported by this upstream` | **C** | **继续拒绝**。OpenAI Chat Completions 规范明确要求 `stop` 数组最多 4 个。 | `claude_code/err_too_many_stops/` |
| 19 | P1 | S1 | 未知顶层字段 | 如 `speed: "fast"` | `speed is not supported when translating to another protocol` | **A / D** | **建议容忍**。客户端升级带来的提示类字段不应阻断转译。 | `claude_code/err_unknown_top_level/` |
| 20 | P4 | S1 | `presence_penalty` / `frequency_penalty` | `0` 或浮点数值 | `presence_penalty is not supported when translating to another protocol` | **A / B** | **建议丢弃（取值为 0 时丢弃，非 0 记录或映射）**。这是几乎所有常见 OpenAI SDK / 客户端默认附带的参数，当前转译层一律 400 是 P4 路径最常见失败点。 | 见下文 P4 分析 |
| 21 | P4 | S6 | `reasoning_effort` | `"medium"` | `reasoning_effort is not supported when translating to another protocol` | **A** | **安全丢弃**。与 Responses 路径下的 `reasoning.effort` 保持一致，跨厂商不做猜测性映射直接丢弃。 | 见下文 P4 分析 |

---

## 4. 非 400 类问题排查与实测分析

除了入站参数校验被 400 拦截外，对数据流传输、流事件顺序、生命周期和取消机制进行了细致检查：

### 4.1 流式输出与用量（S2）
- **现象**：Claude Code 使用流式接收 Anthropic 事件时，网关转译器 `Chat -> Anthropic` 依次输出 `message_start`、`content_block_start`、`content_block_delta`、`content_block_stop`、`message_delta`、`message_stop`。
- **验证结论**：
  - 合成器严格遵守了 spec/10 §6.2，在请求 Chat 上游时自动注入了 `stream_options: {"include_usage": true}`，使得上游即使在流模式下也会在结尾发送 usage chunk。
  - 响应转换器在收到 `[DONE]` 之前成功提取到了用量，并封装在 `message_delta` 的 `usage` 结构体中，Claude Code 控制台正常显示 token 统计，没有出现用量归零或缺失现象。

### 4.2 工具调用参数交错与索引（S3, S4）
- **现象**：当模型流式生成工具调用时，参数以 `input_json_delta` 分块流出。
- **验证结论**：
  - 单工具调用（S3）与并行工具调用（S4）均能正确按 `index` 维护活跃的工具块。
  - 当上游输出下一个工具调用时，状态机能够正确触发前一个工具块的 `content_block_stop`，工具调用 ID 和参数字符串在客户端侧拼接后能够完整通过 `json.Unmarshal` 解析。

### 4.3 会话取消与账本释放（S8）
- **复现步骤**：
  1. 客户端发起一个预计长文本输出的流式请求；
  2. 客户端在输出前 10 个 token 后主动断开连接（如按 `Ctrl+C` 或关闭连接）；
  3. 观察网关日志及账本变更。
- **实测结果**：
  - 客户端断开连接触发 HTTP 请求的 `r.Context().Done()`；
  - `core.Handler` 捕获到上下文取消，发往上游的 HTTP 请求随之被取消（上游不再继续生成计费）；
  - 账本（`ledger`）正确介入结算：未消耗的预留额度（`reserve`）被立即全额释放回客户端配额，已产生的少数 token 按已消费计入账本。无内存泄漏或账本死锁。

---

## 5. 总结与后续建议

### 5.1 各路径当前状态一览

1. **P1 (Claude Code → `openai_chat`)：基本可用，边缘受阻**
   - **当前状态**：在普通问答、代码编辑（使用原生 custom 工具如 Read/Write/Edit/Bash）、流式思考、多轮对话场景下**完全可用**。
   - **主要阻断点**：工具结果回传图片（`tool_result` 含 image）、使用服务端内置搜索/工具、配置 `mcp_servers` 或传入 `top_k`。

2. **P2 / P3 (Codex CLI → `anthropic` / `openai_chat`)：目前完全不可用（启动即阻塞）**
   - **当前状态**：**不可用**。只要使用真实的 Codex CLI（0.167），无论发送什么提示词，网关第一道门槛就会返回 400。
   - **根本原因**：
     1. Codex CLI 默认必发顶层对象 `client_metadata`；
     2. Codex CLI 默认激活 `web_search` 和 `namespace`（`multi_agent_v1` 等）工具。
   - **转机**：一旦转译层允许丢弃 `client_metadata` 并过滤非 `function` 的内置工具，Codex CLI 的核心功能（文本生成、代码命令执行 `exec_command`、加密推理块丢弃、多模态图片）即可无缝在 Anthropic 和 OpenAI Chat 上游运行。

3. **P4 (OpenAI Chat 客户端 → `anthropic`)：部分可用，受常见客户端默认参数拦截**
   - **当前状态**：原生 SDK 极简调用可用；但生产/开源前端（Cursor, Cherry Studio, NextChat 等）普遍不可用。
   - **根本原因**：前端组件默认填入 `frequency_penalty: 0`、`presence_penalty: 0` 等无效默认值，均被转译层严格的未知字段策略判为 400。

### 5.2 交付给 T42（字段容忍策略）的最小修改建议

为了在维持微内核与转译契约安全的前提下，让真实客户端真正开箱即用，建议 T42 实施以下最小放宽策略：

1. **Codex Responses 放行名单**：
   - 将 `client_metadata` 加入 `responsesTopLevel` 并设置为 `false`（安全丢弃）。
   - 在 `lowerResponsesTools` 中：对于 `type != "function"` 的内置工具（如 `namespace`、`web_search`），若客户端未在 `tool_choice` 中强行指定要求调用该工具，则视为客户端环境自带的辅助声明予以忽略/过滤，不触发全局 400。
2. **OpenAI Chat Completions 容忍策略**：
   - 将 `presence_penalty`、`frequency_penalty`、`seed`、`reasoning_effort` 加入 `chatTopLevel`；当惩罚项为 0 或未实质改变生成语义时安全忽略。
3. **Anthropic Messages 容忍策略**：
   - 对 `top_k` 参数予以丢弃，避免客户端配置该参数时导致会话中断。
