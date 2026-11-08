# Universal AI Gateway — 实现规格（SPEC）总索引

> 版本：spec v1.0（2026-11-07）｜适用里程碑：M1（透传 MVP）、M2（跨协议转译）、M3（运维增强）
> 本目录是**实现的唯一依据**。根目录 `00_*.md`–`06_*.md` 是设计背景，与本目录冲突时以本目录为准。
> 工作纪律见根目录 [`AGENTS.md`](../AGENTS.md)。

## 阅读顺序

| 顺序 | 文件 | 内容 | 谁必须读 |
|:-:|:--|:--|:--|
| 1 | [01_scope_and_architecture.md](./01_scope_and_architecture.md) | 做什么/不做什么、模块地图、分层规则、请求生命周期 | 所有人 |
| 2 | [02_engineering_rules.md](./02_engineering_rules.md) | Go 编码规范、错误处理、并发、日志、测试、依赖白名单 | 所有人 |
| 3 | [03_config.md](./03_config.md) | 配置文件结构、默认值、校验规则、命令行参数、HTTP 服务端超时、可信代理 | 改 `config`、`cmd` 的人 |
| 4 | [04_storage.md](./04_storage.md) | SQLite 表结构、迁移、仓储接口与语义 | 改 `store` 的人 |
| 5 | [05_security_auth.md](./05_security_auth.md) | 主密钥、信封加密、Token、鉴权、管理员引导 | 改 `crypto`、`auth`、`admin` 的人 |
| 6 | [06_billing_ledger.md](./06_billing_ledger.md) | 价格、成本计算、预留-结算账本、计费事件、崩溃语义 | 改 `pricing`、`ledger`、`core` 的人 |
| 7 | [07_scheduler.md](./07_scheduler.md) | Key 池、状态机、亲和缓存、选择策略、错误分类、重试 | 改 `scheduler`、`core` 的人 |
| 8 | [08_adapters.md](./08_adapters.md) | 入站 / 出站协议适配器契约（大模型与非大模型 API）、一致性测试套件、**新增协议的扩展指南**、搜索 / 文档解析接入范例 | 改 `protocol`、`adapter/*` 的人；新增协议的人 |
| 9 | [09_http_api.md](./09_http_api.md) | 由注册表生成的数据面路由、中间件、错误格式约定、管理 API | 改 `httpapi`、`admin` 的人 |
| 10 | [10_ir_translation.md](./10_ir_translation.md) | （M2）跨协议 IR 与转译规则 | 改 `ir`、`translate` 的人 |
| 11 | [11_tasks.md](./11_tasks.md) | **任务卡**：顺序、依赖、涉及文件、验收标准 | 所有人 |
| 12 | [12_deployment.md](./12_deployment.md) | 局域网 / 域名对外部署、反向代理示例、调参、开机自启 | 部署者；做 T14、T15 的人 |
| 13 | [13_core.md](./13_core.md) | 内核：协议无关的处理流程、透传/转译选择、三种响应形态的转发、结算、超时、**冻结接口与变更流程** | 改 `core` 的人；任何想改契约的人 |
| 14 | [14_ai_access.md](./14_ai_access.md) | 供 AI 调用：工具契约、MCP 服务端、命令行、Claude Code / Codex CLI 等客户端的配置 | 改 `mcp`、实现工具的人；部署者 |

## 规格写法约定

- **必须 / MUST**：不满足即为缺陷。**禁止 / MUST NOT**：出现即为缺陷。**应当 / SHOULD**：有充分理由可偏离，但需在交付说明中写明。**可以 / MAY**：可选。
- 代码块中的 Go 签名与仓库代码一致；**以仓库代码为准**，若发现不一致，报告而不是自行修改。
- 每条可测试的行为都有编号（如 `LED-07`）。测试函数名或注释应引用编号，例如 `// LED-07`。
- 「示例」只用于说明，不是穷举；「测试用例表」是最低要求，必须全部实现为测试。

## 术语表

| 术语 | 含义 |
|:--|:--|
| 数据面（data plane） | 处理下游客户端模型请求的路径：鉴权 → 限流 → 预留 → 选 Key → 转发 → 结算。**禁止**磁盘 I/O |
| 控制面（control plane） | 后台 worker 与管理 API：持久化、加载、对账、配置变更 |
| 下游 / 客户端（client） | 调用网关的程序（Claude Code、OpenAI SDK……），持有 `uai_live_` Token |
| 上游（upstream） | 模型服务商 API（Anthropic、OpenAI、DeepSeek……） |
| 上游 Key（upstream key） | 网关保存的、用于调用上游的 API Key，加密存储 |
| 池（pool） | 同一上游、同一 base_url 的一组上游 Key |
| 入站协议（ingress protocol） | 客户端使用的协议，由请求路径决定 |
| 上游协议（upstream protocol） | 目标池使用的协议 |
| 透传（passthrough） | 入站协议 == 上游协议时，只改写少数字段、其余字节原样转发 |
| 转译（translate） | 入站协议 != 上游协议时，经 IR 转换（M2） |
| 预留（reserve） / 结算（settle） | 准入时冻结预估成本；结束时按真实 usage 扣费并释放差额 |
| 计费事件（usage event） | 每个请求结算后产生的一条不可丢弃的用量记录 |
| 亲和键（affinity key） | 用于把同一会话路由到同一上游 Key 的字符串 |
| nano-USD | 金额单位：1 USD = 1,000,000,000 nano-USD，使用 `int64` |
| 首字节（first byte） | 网关向客户端写出的第一个响应字节；之后不得重试 |
