# T42 转译层字段容忍策略

- **类型**：规格 + 实现
- **前置**：T41（需要它的报告与回归用例）
- **需要用户审批**：**是**。先做阶段 A（把 §3 整理进 spec 并提交审批），批准后再做阶段 B。
- **必读**：
  - T41 的报告 `docs/handoff/reports/T41_compat_report.md` 与用例 `internal/translate/testdata/real_clients/`
  - [`spec/10_ir_translation.md`](../../spec/10_ir_translation.md) §1、§3、§6.1、§8.2
  - [`spec/03_config.md`](../../spec/03_config.md)（配置结构、默认值、CFG 校验规则）
  - [`internal/translate/chat_lower.go`](../../internal/translate/chat_lower.go)、[`anthropic_lower.go`](../../internal/translate/anthropic_lower.go)、[`responses_lower.go`](../../internal/translate/responses_lower.go)、[`register.go`](../../internal/translate/register.go)
  - [`cmd/gateway/adapters.go`](../../cmd/gateway/adapters.go)、[`cmd/gateway/serve.go`](../../cmd/gateway/serve.go) 中 `newRegistry()` 的调用处
  - [`internal/config/config.go`](../../internal/config/config.go)

## 1. 背景

三个解析器（Chat、Anthropic、Responses → IR）对**未列出的字段**一律返回 400。这是 T20、T23、T25 留下的 `SPEC-GAP`，规格本身没有规定。
问题在于：客户端每次升级都可能新增字段，只要多出一个字段，这条转译路径就整体不可用。
T41 会给出真实客户端到底发了哪些字段。本卡做两件事：

1. **静态修正**：把 T41 中归为 A 类（可以安全丢弃）和 B 类（应该映射）的字段写进 spec/10 的字段表，并实现。
2. **兜底开关**：新增配置项。运维方可以选择把**未知字段**改为「丢弃并告警」，而不是 400。

明确拒绝的字段（spec 表里标了 400 的，例如 `top_k`、内置工具、`response_format`）**不受开关影响，始终拒绝**。

## 2. 当前代码中的相关位置

| 解析器 | 顶层字段表 | 消息级 / 块级检查 |
|:--|:--|:--|
| Chat → IR | `chatTopLevel`、`chatRejected`（`chat_lower.go`） | `chatMessageKeys`：消息里的未知键返回 400 |
| Anthropic → IR | `anthropicTopLevel`（`anthropic_lower.go`；值为 `false` 的是明确拒绝） | 消息只允许 `role`、`content`；内容块里的未知键**已经**按 `SPEC-GAP(T23)` 丢弃 |
| Responses → IR | `responsesTopLevel`（`responses_lower.go`；值为 `false` 的是 §8.2 的丢弃字段） | 输入项里的未知键：见 `lowerResponsesInput` |

三个解析器的签名都是 `func(body []byte) (*ir.Request, error)`，转换器类型（`ChatToAnthropic`、`ResponsesToAnthropic`、`AnthropicToChat`、`ResponsesToChat`）由 `translate.Register` 以零值注册。

## 3. 规格草案（阶段 A：整理进 spec 后提交审批）

> 以下文字是建议稿。执行者要把它整理进对应的 spec 文件，并结合 T41 的结果补全 §3.3 的字段清单。标了「待定」的地方请用户逐条回答。

### 3.1 spec/10 §1 新增第 5 条

> 5. **未知字段策略**：请求中出现本章字段表未列出的字段，按配置 `translation.unknown_fields` 处理：
>    - `reject`（默认）：400 `invalid_request`，消息为 `<字段路径> is not supported when translating to another protocol`；
>    - `drop`：丢弃该字段继续处理，并按 §1.6 记日志。
>
>    「字段表未列出」指：顶层字段、消息（或 Responses 输入项）级字段、内容块级字段。
>    字段表中**标为 400 的字段**始终拒绝，不受此配置影响；**标为丢弃的字段**始终丢弃，不记日志。
>    值为 JSON `null` 的字段视为未提供，不触发此规则。

### 3.2 spec/10 §1 新增第 6 条（告警日志）

> 6. `drop` 模式下，每个进程对每个（转换方向、字段路径）组合只记**一次** `Warn` 日志：`msg="translation dropped unknown field"`，属性 `from`、`to`、`field`（字段路径，如 `messages[].foo`，数组下标统一写成 `[]`）。
>    日志**不得**包含字段的值。
>    *（待定 1：要不要再加一个按方向和字段计数的指标？如果要，等 T45 的指标端点做好后再加。）*

### 3.3 spec/10 §3 / §6.1 / §8.2 字段表补充

按 T41 报告的分类补全：

| T41 分类 | 处理 |
|:--|:--|
| A 可以安全丢弃 | 写进对应表格，说明列写「丢弃」 |
| B 应该映射 | 写进对应表格，给出 IR 映射；IR 没有对应字段的，写「待定」，由用户决定是否扩展 IR |
| C 应该继续拒绝 | 写进对应表格，说明列写「400」，消息说明原因 |
| D 不确定 | 列在审批请求里，请用户决定 |

*（待定 2：内容块级的未知键现在已经在 Anthropic 方向被丢弃（`SPEC-GAP(T23)`）。是保持「始终丢弃」，还是也改成受 `unknown_fields` 控制？建议保持始终丢弃，避免默认的 `reject` 让 Claude Code 每次加提示性字段都失败。）*

### 3.4 spec/03 新增配置

```yaml
translation:
  unknown_fields: reject     # reject | drop，见 spec/10 §1 第 5 条
```

| 键 | 默认值 |
|:--|:--|
| `translation.unknown_fields` | `reject` |

| 编号 | 规则 |
|:--|:--|
| CFG-14 | `translation.unknown_fields` 只能是 `reject` 或 `drop` |

*（待定 3：默认值保持 `reject`，还是改为 `drop`？保持 `reject` 更安全，但部署者必须知道要打开它。建议保持 `reject`，并在 DEPLOYMENT.md §6.1 中写明：接入 Claude Code、Codex CLI 时建议设为 `drop`。）*

### 3.5 spec/11

在 `spec/11_tasks.md` 末尾登记本卡（引用本文档），状态 `TODO`。

## 4. 实现要求（阶段 B：用户批准后）

### 4.1 配置

- `internal/config`：新增 `TranslationConfig{UnknownFields string}` 与 `Config.Translation`，默认值在 `Normalize`（或默认值函数）中填充，`Validate` 中实现 CFG-14。
- `gateway.example.yaml`：增加带注释的 `translation:` 段。
- `config` 已有的测试：增加 CFG-14 的正反用例和默认值用例。

### 4.2 转译包的选项

不改变 `translate.Register` 的签名（它被 `adapters.go` 以 `func(*protocol.Registry) error` 的形式使用）。新增：

```go
// Options 是转译层的可配置行为（spec/10 §1 第 5、6 条）。零值等价于默认配置。
type Options struct {
	DropUnknownFields bool
	Logger            *slog.Logger // nil 时不记日志
}

// Registrar 返回按 opts 注册全部转换器的函数；Register 等价于 Registrar(Options{})。
func Registrar(opts Options) func(*protocol.Registry) error
```

- 四个转换器类型各加一个 `Opts Options` 字段，零值行为与现在完全一致。
- 三个解析器改成接收选项，例如 `lowerOpenAIChat(body []byte, opts Options)`。解析器未导出，可以改签名，但要同步更新所有调用处和测试。
- 「只记一次」：用包级 `sync.Map`，键为 `from|to|field`。它是进程级状态，测试里要能清空（例如提供未导出的 `resetDropLog()`），或者让测试只断言「至少一次、不超过一次」。

### 4.3 装配

- `cmd/gateway/adapters.go`：`newRegistry()` 改为 `newRegistry(cfg *config.Config, log *slog.Logger)`。适配器列表里的 `translate.Register` 改为 `translate.Registrar(translate.Options{DropUnknownFields: cfg.Translation.UnknownFields == "drop", Logger: log})`。
- `cmd/gateway/serve.go`：更新调用处。
- 这是 `cmd/` 里的组装代码，不涉及内核。

### 4.4 字段表落地

按审批通过的 §3.3 修改三个解析器的字段表与映射。每新增一个丢弃或映射的字段，都要有单元测试。

### 4.5 清理 SPEC-GAP

规格写清楚之后，删除 `chat_lower.go`、`responses_lower.go`、`anthropic_lower.go` 中已经解决的 `SPEC-GAP(T20)`、`SPEC-GAP(T25)`、`SPEC-GAP(T23)` 注释，改为引用新的 spec 条款。`chat_lower.go` 里关于 `name` 字段的那条 `SPEC-GAP(T20)` 如果仍未解决，保留。

### 4.6 回归用例

- T41 的 `testdata/real_clients/**/expect.txt`：被本卡修复的用例，从 `error: ...` 改为 `ok`。**这是本卡唯一允许修改的期望值**，每一处都要在交付说明里列出。
- `TestRealClientFixtures` 要在 `reject` 和 `drop` 两种模式下各跑一遍。在 `drop` 模式下，只有 C 类（明确拒绝）的用例还应该失败。
- 已有的拒绝测试（例如 `TestLowerOpenAIChatRejects` 里的 `unknown top-level field`）在默认模式下**必须保持原样通过**。另外为 `drop` 模式新增对应的用例。

## 5. 涉及文件

- spec：`spec/10_ir_translation.md`、`spec/03_config.md`、`spec/11_tasks.md`
- 配置：`internal/config/config.go`、`internal/config/*_test.go`、`gateway.example.yaml`
- 转译：`internal/translate/{chat_lower,anthropic_lower,responses_lower,register}.go` 及其测试，`internal/translate/testdata/real_clients/**/expect.txt`
- 装配：`cmd/gateway/adapters.go`、`cmd/gateway/serve.go`
- 文档：`docs/DEPLOYMENT.md` §6.1（加一句接入建议）

## 6. 验收标准

- [ ] 阶段 A 的 spec 提交已获用户批准（交付说明中引用批准内容）。
- [ ] 默认配置下，全部已有测试与黄金文件不变并通过。
- [ ] `drop` 模式下，未知字段被丢弃，日志对每个字段只出现一次且不含字段值；明确拒绝的字段仍然返回 400。
- [ ] CFG-14 有正反用例；配置为非法值时网关拒绝启动。
- [ ] T41 中 A、B 类的用例全部变为 `ok`；C 类在两种模式下都仍然是 `error`。
- [ ] 已解决的 `SPEC-GAP` 注释已删除，没有新增未说明的 `SPEC-GAP`。
- [ ] `go run ./tools/check` 全绿。

## 7. 建议提交拆分

1. 阶段 A：`docs(spec): T42 转译层未知字段策略与字段表补充（待审批）`
2. `feat(config): T42 新增 translation.unknown_fields（CFG-14）`
3. `feat(translate): T42 实现未知字段策略并补充字段映射`
4. `docs(deploy): T42 说明接入 Claude Code / Codex 时的字段策略`
