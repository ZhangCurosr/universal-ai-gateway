# T44 SPEC-GAP 决策与落地

- **类型**：用户决策（§2）+ 小改动（§3，由执行者完成）
- **前置**：无
- **需要用户审批**：**是**。§2 的决策栏由用户填写，执行者只按填好的决策落地。
- **必读**：[`AGENTS.md`](../../AGENTS.md) §2 第 7 条（SPEC-GAP 的含义）；下表各行「位置」列出的代码。

## 1. 背景

实现过程中，凡是规格没写清楚的地方，都按 AGENTS.md 的要求选择了最保守的做法，并在代码里标记了 `// SPEC-GAP(Txx)`。
目前非测试代码中共有 9 处标记。其中 3 处（T20、T23、T25 的「未知字段一律 400」）由 [T42](T42_field_tolerance.md) 统一处理，**不在本卡范围内**。
其余 7 处列在下表，每一处都需要用户决定：把当前行为写进规格（「采纳现状」），还是改成别的行为。

列出全部标记：

```bash
git grep -n "SPEC-GAP(" -- '*.go' ':!*_test.go'
```

## 2. 决策表（用户填写「决策」列）

| # | 标记 | 位置 | 规格缺了什么 | 当前行为 | 可选项 | 建议 | 决策 |
|:--|:--|:--|:--|:--|:--|:--|:--|
| G1 | `SPEC-GAP(T20)` | `internal/translate/chat_lower.go`，`chatMessage.Name` | Chat 消息里的 `name`（参与者名）怎么处理 | 丢弃（IR 与 Anthropic 都没有对应字段） | a. 采纳现状（丢弃）；b. 400；c. 拼进文本，如 `[name]: ...` | a。多数客户端不发；c 属于「编造等价物」，违反 spec/10 §1 第 2 条 | a（采纳现状：丢弃） |
| G2 | `SPEC-GAP(T22)` | `internal/translate/chat_from_anthropic.go`，`finishReasonFor` | §5.1 没列出的 Anthropic `stop_reason`（如 `pause_turn`）和缺失值映射成什么 | → `"stop"` | a. 采纳现状；b. 缺失值 → `null`，未知值 → `"stop"` | a | a（采纳现状：映射为 "stop"） |
| G3 | `SPEC-GAP(T24)` | `internal/translate/anthropic_from_chat.go`，`stopReasonFor` | §6.3 没列出的 Chat `finish_reason`（如 DeepSeek 的 `insufficient_system_resource`）和缺失值 | → `end_turn` | a. 采纳现状；b. `insufficient_system_resource` 视为上游错误：非流式返回 502，流式返回流错误 | b 更准确：DeepSeek 文档说明这个值表示推理因资源不足被中断，输出不完整。但要先确认上游真的会发这个值。如果拿不准，选 a | b（视为上游错误：非流式 502，流式流错误） |
| G4 | `SPEC-GAP(T26)` | `internal/translate/responses_from_anthropic.go`，`statusFor` | §8.3 只规定了正常结束、工具调用、输出上限。`refusal`、`pause_turn` 等其他 `stop_reason` 怎么处理 | → `completed` | a. 采纳现状；b. `refusal` → `incomplete` + `incomplete_details.reason: "content_filter"` | b。Responses 的 `incomplete_details.reason` 本身就有 `content_filter`，语义对得上 | b（`refusal` / `pause_turn` 映射为 incomplete + content_filter） |
| G5 | `SPEC-GAP(T27)` | `internal/translate/responses_from_chat.go`，`chatToResponsesBody` 与流式 `textDelta(Refusal)` | Chat 上游的 `refusal` 在 Responses 中怎么表示 | 与正文拼接为 `output_text` | a. 采纳现状；b. 输出为 Responses 的 `refusal` 内容部件（`{"type":"refusal","refusal":...}`） | b 更符合协议，但 Codex CLI 是否能识别 `refusal` 部件需要实测（可以并入 T41）。实测前选 a | a（采纳现状：作为 output_text 输出） |
| G6 | `SPEC-GAP(T13)` | `internal/audit/audit.go`，保留期清理 | `audit.retention_days <= 0` 时怎么办 | 静默按 30 天处理 | a. 采纳现状；b. 新增 CFG 规则：`retention_days >= 1`，否则网关拒绝启动；c. 约定 `0` 表示「永不清理」 | b。配置错误应该在启动时暴露，而不是被静默改成另一个值 | b（CFG-14 规则：retention_days >= 1，fail-fast） |
| G7 | `SPEC-GAP(T31)` | `internal/adapter/mineru/egress.go`，`afterSubmit` | MinerU 任务提交之后，轮询或下载失败时能不能换 Key 重试 | 不重试：上游已经按页计费，重试会再创建一个付费任务。错误消息里带上任务 ID，便于用 `GET /v1/tasks/{id}` 找回结果 | a. 采纳现状；b. 允许重试 | a。重试会重复计费 | a（采纳现状：提交后失败不重试，带任务 ID） |

> G3 中关于 DeepSeek `insufficient_system_resource` 含义的说法来自 DeepSeek API 文档，用户决策前最好再核对一次最新文档。

## 3. 落地（执行者，在用户填完 §2 之后）

对每一行：

### 选「采纳现状」时

1. 在对应的 spec 小节写明这条行为，用规格的语气写，不要写「SPEC-GAP」。
   - G1 → spec/10 §3 表格，加一行 `messages[].name`，说明列写「丢弃」。
   - G2 → spec/10 §5.1 `stop_reason` 那一行补「其他值与缺失 → `stop`」。
   - G3 → spec/10 §6.3 `finish_reason` 那一行补「其他值与缺失 → `end_turn`」。
   - G4 → spec/10 §8.3「停止原因」一条补「其他 → `completed`」。
   - G5 → spec/10 §8.3 补「Chat 上游的 `refusal` 文本作为 `output_text` 输出」。
   - G6 → spec/03 的 audit 小节补「`retention_days <= 0` 按 30 处理」。
   - G7 → spec/08 的 MinerU 小节（或 spec/11 T31 卡引用的小节）补「任务提交之后的失败不可重试；错误消息带任务 ID」。
2. 把代码里的 `SPEC-GAP(Txx)` 注释改为引用新条款，例如 `// spec/10 §5.1：其他 stop_reason 按 stop 处理。`。

### 选其他方案时

1. 同上，先改 spec。
2. 改代码实现新行为。
3. 新增或修改对应的测试。**修改已有测试的期望值属于用户已经批准的变更**，但交付说明里要逐条列出：「测试名、旧期望 → 新期望、对应决策 G#」。
4. 如果改动涉及黄金文件（G4、G5 可能会），只更新受影响的用例，并在交付说明中贴出 diff。
5. G6 选 b 时：新规则编号接着 spec/03 现有的最大编号。如果 T42 已经占用了 CFG-14，就用下一个。同时更新 `internal/config` 的 `Validate` 与测试。

### 注意

- T43（流式 IR 重构）如果已经开始或已经合入，G2–G5 的代码位置会移到新的 `encode_*.go` / `decode_*.go` 中，以 T43 合入后的代码为准。
- 本卡不处理 T20/T23/T25 的「未知字段」类 SPEC-GAP（见 T42）。

## 4. 涉及文件

- spec：`spec/03_config.md`、`spec/08_adapters.md`、`spec/10_ir_translation.md`、`spec/11_tasks.md`（登记本卡）
- 代码：§2「位置」列中的文件，以及对应的测试文件；G6 选 b 时还有 `internal/config`

## 5. 验收标准

- [ ] §2 每一行都有用户填写的决策。
- [ ] 每个决策都已写进 spec，代码中对应的 7 处 `SPEC-GAP` 标记已全部清除，`git grep` 只剩 T42 负责的那几处（或者 T42 合入后一处不剩）。
- [ ] 行为有变化的决策，都有能体现新行为的测试；修改的期望值逐条列在交付说明中。
- [ ] `go run ./tools/check` 全绿。

## 6. 建议提交拆分

1. `docs(spec): T44 将 SPEC-GAP 决策写入规格`
2. 每个改变行为的决策一个提交：`fix(<范围>): T44 G# <简述>`
