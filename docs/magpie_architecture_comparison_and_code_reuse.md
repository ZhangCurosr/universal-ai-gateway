# Universal AI Gateway 与 Magpie：架构比较与代码复用评估

> 核对日期：2026-11-11。本文是源码比较和候选方案评估，不是实现规格或移植授权；实现依据为 [spec](../spec/README.md)、现有接口和测试，工作规则见 [AGENTS.md](../AGENTS.md)。

## 1. 比较基线与阅读方式

| 项目 | 本次核对基线 | 说明 |
|:--|:--|:--|
| Universal AI Gateway | `2ae50e5e120333628578e5465d8685c32f610fb6`（含 T52 加固） | 本文的相对链接指向仓库内文件；实现基线已提交，可检出该版本复核 |
| Magpie | `7476328901e9c9236446640e9fbd33fe67930a6d`，核对时工作区干净 | 以下 Magpie 链接固定到该提交；源码中的客户端行为记录仅代表其记录的版本和场景 |

本文把已有实现称为“现状”，把尚未在本网关复现的问题称为“待验证”，把可能采用的机制称为“提案”。源码存在某个兼容补丁，不代表本网关一定存在同样的生产故障。本文不以代码行数、未经测量的并发能力、用户数量或节省比例作为选型依据。

本文覆盖十一类比较主题与十项复用候选，提供固定版本的完整源码入口及接入要求。复用应从完整函数、调用链及测试开始，避免将省略校验和收尾逻辑的示意代码当作可部署实现，并核对许可证与署名要求。

## 2. 定位和现有能力

### 2.1 两个项目的取舍

本网关按 [范围与架构](../spec/01_scope_and_architecture.md) 面向单实例部署：SQLite 存储，内存鉴权索引、预留账本与调度状态，数据面不直接访问数据库。支持容器部署不等于支持共享账本的多副本集群；当前没有 PostgreSQL 实现。全局默认并发上限是 256（[配置实现](../internal/config/config.go)），实际容量应由负载测试证明。

金额以 `int64` nano-USD 表示，成本计算用有理数精确累加后取整（[pricing](../internal/pricing/pricing.go)）。nano-USD 是金额单位，不是“纳秒级账本”的性能证明；这也不是浮点计费。计费事件在正常运行时不可丢弃，但强制退出仍可能丢失全部尚未持久化的事件，见 [计费规格 §5](../spec/06_billing_ledger.md#5-崩溃语义写进用户文档不要宣称更强保证)。

Magpie 将客户端配置集成、账号路由、协议兼容、桌面体验和同步集中在一个产品中。其 [agent][m-agent]、[gateway][m-routing]、[usage][m-page]、[davsync][m-dav] 等模块可提供机制参考；它们的状态归属和依赖关系不能直接替代本网关的微内核设计。

### 2.2 十一类子系统比较

| 主题 | 本网关现状与证据 | Magpie 参考入口 | 比较结论 |
|:--|:--|:--|:--|
| 1. 协议与流式处理 | 同协议局部改写；跨协议经 IR。SSE 单行上限 4 MiB，见 [sse](../internal/sse/sse.go) | [SSE][m-sse]、[裸错误处理][m-bare]、[签名][m-signature] | 超长行与裸错误值得构造样例；总输出 token 多不等于单行一定超限 |
| 2. 调度与容灾 | 支持 `least_inflight`、平滑加权轮询、会话亲和、严格任务固定及分类冷却，见 [调度规格](../spec/07_scheduler.md) | [provider 路由][m-provider-routing]、[gateway 路由][m-routing]、[流速][m-speed] | 可比较具体失败分类和选择策略；不能概括为“所有 4xx 都熔断” |
| 3. Agent 配置集成 | 已有客户端接入说明、CLI 与 MCP，见 [AI 接入规格](../spec/14_ai_access.md) | [agent][m-agent]、[rewire][m-rewire]、[claudelock][m-lock] | 自动修改客户端配置属于独立客户端工具能力，需备份、漂移检测和恢复设计 |
| 4. 隐私、JSON 与存储 | 凭据加密、日志掩码；透传用局部 JSON 操作，见 [jsonx](../internal/jsonx)、[crypto](../internal/crypto/keyring.go) | [redact][m-redact]、[sqlitedb][m-sqlitedb]、[OTLP][m-otlp] | 日志掩码与内容脱敏是不同能力；不能据此声称现有透传做了整包 JSON 往返 |
| 5. 多模态与搜索 | 有图像内容转译及 Tavily、MinerU、REST 等适配器，见 [适配器规格](../spec/08_adapters.md) | [搜索抓取][m-fetch]、[搜索引用][m-cite]、[H.264][m-h264] | 本地转码、网页抓取、视频合成属于新增产品范围，不作为数据面通用补丁 |
| 6. 网络安全 | 管理面启用 Host 检查，MCP 检查 Origin，上游禁止自动重定向，见 [装配](../cmd/gateway/serve.go)、[MCP](../internal/mcp/server.go) | [netguard][m-netguard]、[lan][m-lan] | 需按威胁来源评估 SSRF；不能写成“本网关只依赖防火墙” |
| 7. Coding Agent 协议 | Responses 同协议保留原始内容；跨协议有明确的支持和拒绝规则，见 [Responses 转译](../internal/translate/responses_lower.go) | [Magpie WebSocket][m-ws] | 必须区分透传兼容性和转译能力，客户端私有行为需固定版本和请求样本 |
| 8. WebSocket | 暂无 WebSocket 传输；HTTP/SSE 有取消、读空闲和客户端写超时 | [responses_ws][m-ws]、[codex_socket_keep][m-keep] | 虚拟 HTTP 请求回路可参考，但鉴权、资源预算与连接生命周期必须重新验证 |
| 9. 运维与桌面常驻 | 有管理 API、Web UI、CLI、Prometheus 指标，见 [指标规格](../spec/15_metrics.md) | [诊断保护][m-heap]、[Bark][m-bark]、[awake][m-awake] | 可先评估受保护的诊断接口；TUI、移动通知、防休眠按部署需求另立任务 |
| 10. 备份与同步 | 单实例 SQLite；主密钥是文件，没有机器硬件绑定 | [backup][m-backup]、[davsync][m-dav]、[SigV4][m-sigv4] | 安全迁移需配套数据与密钥；离线恢复和多实例在线账本同步必须分开 |
| 11. 模型、计量与工程实践 | 有配置驱动的模型目录；计量为任意单位映射；检查覆盖分层、race、零 CGO | [catalog][m-catalog]、[SameModel][m-samemodel]、[usage][m-page]、[LESSONS][m-lessons] | 外部元数据、细分单位和报表优化均可评估，不能据此替换精确路由或账本 |

### 2.3 已有机制应作为比较起点

- **调度分类**：429 进入有上限的冷却；401 按连续失败次数冷却或隔离；403 默认冷却；客户端错误、取消、本地资源错误不计熔断失败；服务端故障、超时和网络错误进入失败统计。详见 [结果状态机](../spec/07_scheduler.md#5-结果上报与状态机leasefinish任务-t08) 与 [分类实现](../internal/protocol/classify.go)。402 默认落入客户端错误，不应未经协议依据改成永久禁用 Key。
- **会话与观测**：亲和优先，命中刷新 TTL；严格固定不允许换 Key。审计包含 TTFB 与总时延，不能描述为“只记录 RTT”。
- **超时与收尾**：已有响应等待、非流式总时限、流空闲取消和可续期的客户端写截止时间；T52 还加入了共享响应缓冲预算与已付费失败的结算处理，见 [内核规格](../spec/13_core.md)。
- **Magpie Drain 语义**：优先使用一个 Key 或账号直至耗尽，再继续下一个（[声明][m-provider-routing]）；不表示停止分配新请求的维护排空。
- **SQLite 兼容性**：实际环境初始化入口是 [internal/sqlitedb/sqlitedb.go][m-sqlitedb]，不是 `gateway/sqlite_modernc.go`。本仓库 libc 为 v1.80.1，Magpie 基线为 v1.79.0；版本不同，需先复现再决定是否需要补丁，不能仅凭该文件存在断言本网关会崩溃。

## 3. 十项代码复用候选与接入边界

本节的验证用例是未来实现任务的最低要求，本次文档修改不代表这些能力已经开发或测试完成。候选涉及新包、依赖或冻结契约时，仍须遵循现有架构变更流程。

### 3.1 原子文件替换与 Windows 短暂占用处理

**源码事实**：[steady][m-steady] 在同目录创建临时文件，写入、设置权限、Sync、关闭后重命名；Windows 分支对短暂占用做有界重试，并区分文件短暂消失。重试使用固定间隔，不是指数退避。成功写入后还会清理符合规则的过期临时文件。

**提案与边界**：先检查控制面文件的具体写入流程。[master.key 初始化](../internal/crypto/keyring.go) 已有临时文件与重命名，候选增益是刷盘、唯一临时名和错误恢复，不是把所有 `os.WriteFile` 一律替换。临时写入、原子可见性和掉电持久性是不同保证；文件 Sync 并不自动证明目录项持久化，强制杀进程也不会执行 defer 清理。不得承诺“终结所有断电损坏”。

**最低验证**：写入、Sync、关闭和 Rename 各阶段失败时旧文件保留；权限正确；Windows 文件占用能恢复且等待有上限；崩溃残留只清理本工具的过期临时文件；主密钥初始化失败后不覆盖已有密钥。磁盘操作只在控制面或启动流程执行。

### 3.2 SSE 边界与裸 JSON 错误识别

**源码事实**：[Magpie SSE][m-sse] 的 Scanner 上限是 32 MiB；[bareErrors][m-bare] 识别独立 JSON 错误对象，支持跨读取和多行，候选对象暂存上限为 1 MiB。它并不是上一版那个无界 `ReadBytes` 包装器。

**待验证与提案**：本网关的 4 MiB 是单行限制，多条 `data:` 行组成的事件还需单独评估累计大小。先取得超限或裸错误的真实脱敏样本，测量并发内存，再决定上限。单纯把所有流扩大到 32 MiB 会提高最坏内存占用，不能直接列为修复。

裸 JSON 是否为协议错误及如何分类，应由适配器或转译解码器决定；通用 SSE 层保持协议无关。不能在 `core` 中识别厂商错误 JSON，也不能把任意未知行都转换成错误事件。对客户端输出的错误仍需遵守脱敏和固定错误消息规则。

**最低验证**：CRLF、多行 data、跨 chunk、EOF、正常 JSON 文本、截断错误对象、超长行和累计事件大小；首次事件前失败与输出开始后失败分别处理；取消、租约释放和计量完整。**响应头或字节已经写出后禁止重试或换 Key**；即使尚未写出，已付费失败也必须遵守 `Response + error` 的结算、不重试语义。

### 3.3 Gemini 思考签名跨轮次保持

**源码事实**：[gemini_signature.go][m-signature] 包含 ID 携带签名或短引用、长度校验、截断检测、缺失回退，以及仅回给适用上游的处理；它不等价于给 ID 简单追加 `__sig_` 加 Base64。

**现状与边界**：[spec/10 §10.2](../spec/10_ir_translation.md#102-思考签名) 明确跨厂商不传原签名，向 Gemini 合成工具历史时按模型规则注入占位值，`gemini-1.`、`gemini-2.` 除外；同协议透传则保留请求内容。因此不能概括为“所有 Gemini 2.0+ 签名都被丢弃，必须修复”。

**提案**：只有在固定模型和客户端版本下复现签名往返失败后，才设计兼容层。若需要签名存储，应明确租户隔离、容量、TTL、重启恢复及控制面加载方式；不能照搬请求中同步读写本地存储的做法。协议细节留在适配器或转译层，不改变内核导出 API。

**最低验证**：同源往返、跨厂商切换、ID 截断或伪造、超长签名、引用丢失、并行工具调用、不同客户端与租户之间不串用。

### 3.4 工具调用历史修复

**源码事实**：[pairToolBlocks][m-anthropic] 的输入是消息序列 `[]aMessage`。它跟踪 assistant 调用和后续 user 结果，为缺失结果合成错误块，并处理孤立、重复结果。上一版把它缩成单层 ContentBlock 列表，会丢失角色和轮次约束。

**提案与边界**：历史修复会改变请求语义，不能默认加在同协议入站流程。若有明确兼容需求，应单独定义可选策略、适用上游、修改记录及严格模式行为；在协议边界实现，不让内核理解 `tool_use`。人工合成的失败结果不能表示工具真实执行过。

**最低验证**：多轮、并行调用、重复 ID、孤立结果、结果含图像、中断后继续、原本合法的历史保持不变；合成结果必须处于正确角色与顺序，不跨租户或会话关联。

### 3.5 任意 URL 下载的 SSRF 防护

**源码事实**：[netguard][m-netguard] 的包注释限定在 Magpie 没有主动选择的 URL 下载场景。`Public` 排除环回、私网、链路本地、CGNAT、特殊保留段，并检查 IPv4 映射、NAT64、6to4 中嵌入的地址。Fake-IP、Loon 配对和其他代理特例由独立函数与调用方上下文处理，`Public` 本身不将 Fake-IP 判为公网。

**提案与边界**：首先区分管理员配置的池地址、用户提供的下载 URL、上游响应中的下载 URL，以及经代理访问的目标。合法内网模型上游不能被通用“仅公网”规则误伤；受限下载也不能因为使用代理就失去目标校验。不要把简化的 `SocketControl` 无条件装入共享 `newUpstreamClient`。

上一版函数对 `100.64.0.1`、`fe80::1`、`64:ff9b::a00:1` 和 `198.18.0.1` 均返回 `true`，不能作为公网判定实现。仅凭地址落入 Fake-IP 网段，不能证明请求经过可信代理或真实目标安全。

**最低验证**：IPv4/IPv6 与嵌入地址、DNS 多答案、解析后地址变化、禁止地址混入、重定向、代理解析路径、连接复用和合法内网上游。需明确校验结果与实际拨号地址绑定；共享客户端现有禁止自动重定向的规则继续生效。

### 3.6 SigV4 与对象存储备份传输

**源码事实**：[sigv4.go][m-sigv4] 使用转义后的路径、规范化并排序的查询参数、规范化排序的头部及 Host 回退；[davsync][m-dav] 的存储交互还涉及条件请求与冲突处理。签名函数不是完整备份系统，也不是覆盖所有 AWS 服务与签名变体的通用 SDK。

**提案与边界**：如确需远端备份，可将签名算法作为独立客户端的一部分评估。还须设计超时、凭据保护、响应大小限制、条件写、上传一致性、重试和恢复校验。上一版直接签 `URL.Path` 与 `RawQuery` 的简化代码不适用。

**最低验证**：固定签名向量、空路径、空值和重复查询参数、空格与百分号编码、Host 含端口、多值头、UTC 日期边界；传输失败不产生半成品可恢复快照。新增包和分层登记按任务流程处理。

### 3.7 口令加密的离线备份与恢复

**源码事实**：[backup.go][m-backup] 使用标准库 `crypto/pbkdf2` 和 AES-GCM，认证信封头，并在解密时检查格式、版本、KDF、迭代数及 nonce/salt 条件。仅有 Seal 函数不足以证明备份格式安全可用。当前工具链已有标准库实现，不需引入上一版的 `golang.org/x/crypto/pbkdf2`。

**提案与边界**：先定义备份范围和离线恢复流程，明确如何迁移凭据、主密钥及账户标识。当前文件主密钥并非机器硬件绑定；迁移时应按一致性备份方案配套保管密钥与数据。不要直接复制运行中的 SQLite 主文件并假定已覆盖 WAL。

加密解决保密性和完整性，不解决多实例额度预留与结算一致性。配置的三路合并、LWW 和按设备分片的遥测聚合，均不能直接用于财务账本的多写者同步；在线账本同步需要另行设计状态权威、幂等、冲突和故障恢复。

**最低验证**：错误口令、密文和头部篡改、截断、未知版本、异常 KDF 参数、超限文件、密钥匹配、完整恢复演练；恢复只能在明确的停机或隔离流程中执行，失败不得破坏现有数据。

### 3.8 WebSocket 虚拟 HTTP 请求回路

**源码事实**：[responses_ws.go][m-ws] 将每个 `response.create` 重新构造成 HTTP 请求并经过入口处理，维护 socket 注册、轮次上下文、历史和关闭流程；[codex_socket_keep.go][m-keep] 还管理上游连接复用。实现依赖 `github.com/coder/websocket`、gjson 等，不能视作无依赖代码片段。

**提案与边界**：可评估复用现有鉴权与计费入口的模式，但这是传输能力扩展。先记录需要支持的客户端版本、握手、帧类型、HTTP 回退行为；不能从“本网关没有 WS”推导出“所有 Codex 会话默认失败”。Magpie 文件本身记录过以 426 触发 HTTP 回退的场景，这不是对所有版本的保证。

设计必须覆盖每轮重新鉴权、Token 禁用后的处理、独立 request_id、速率与全局并发限制、历史和帧的内存预算、写入背压、读写截止时间及服务关闭。握手的准入不能替代轮次准入；中断已付费轮次仍须结算。需评估 ResponseWriter 的 Flush、截止时间与错误能力，Pipe 必须双向关闭并等待协程退出。

**最低验证**：认证过期或禁用、慢读客户端、长事件、多行 SSE、超限帧、断开与打断并发、活跃轮次关闭服务、每轮只结算一次、不泄漏协程与预留。若现有契约不足，按 [spec/13 §7](../spec/13_core.md#7-冻结接口与变更流程) 提交 ADR；新增依赖也需单独批准。

### 3.9 模型元数据与显式别名

**源码事实**：[catalog][m-catalog] 用于元数据查找；[SameModel][m-samemodel] 用于名称分组与匹配，包含版本点号、分隔符和部分快照处理，并明确保留 OpenAI 的连字符日期快照。它不是任意删除后缀的通用模型身份函数。

**现状与边界**：本网关已有按配置和客户端权限输出的模型目录；缺少的是类似 Magpie 的内置外部元数据库。[路由](../internal/core/core.go) 与 [模型白名单](../internal/auth/index.go) 按名称精确匹配，未知模型返回 400。命名空间、快照、上下文规格可能对应不同上游、价格和权限，不能模糊合并，也不应在冻结的 `protocol` 中添加全局清洗。

**提案**：先考虑管理面的元数据提示；若引入别名，应由管理员显式配置，并定义别名到规范资源的解析、权限、价格和审计语义。外部目录价格只能作为参考，不能自动覆盖已配置的账本价格。

**最低验证**：同名不同供应商、移动别名与固定快照、不同参数规模和上下文规格、别名冲突、权限拒绝、审计可追溯；任何别名都不能扩大客户端授权。

### 3.10 协议计量与用量报表

**源码事实**：Magpie 的 [chat.go][m-chat]、[anthropic.go][m-anthropic] 有协议相关用量处理；[packedRow][m-page] 和 [分块聚合][m-cells] 属于分析存储设计。协议口径、报表数据布局、财务结算是三个不同问题。

本网关已按 [PRC-02](../spec/06_billing_ledger.md#2-成本计算pricing已提供) 定义普通输入不含缓存：[OpenAI Meter](../internal/adapter/openai/egress.go) 扣除缓存命中，[Anthropic Meter](../internal/adapter/anthropic/egress.go) 直接映射普通输入。因此不能引入“输入数大于缓存数就是 OpenAI”的猜测算法。反例：Anthropic 普通输入 100、缓存读取 20，普通输入应保持 100，而不是扣为 80。其他兼容上游的字段口径也必须逐项确认。

计量是 `pricing.Usage map[string]int64`，支持 `requests`、`credits`、`pages` 等任意合法单位，不限于四项 token。新增缓存 TTL 或 reasoning 维度时，先定义它是现有量的子集还是额外计费量，避免重复收费；按协议在适配器声明 Units 并补齐价目，不向内核、账本注入 token 专用逻辑。

**待验证与提案**：[QueryUsage](../internal/store/sqlite/usage.go) 确实通过 `json_group_array(usage)` 获取字符串数组，再逐项解析 map。可对不同记录量、查询窗口和维度基数测量延迟、分配量及峰值内存，再比较 SQL 聚合、预聚合或分析索引。尚无基准证明当前查询已导致生产 GC 停顿，也不能承诺照搬 packedRow 就能将秒级压到毫秒级。

**最低验证**：普通输入与缓存互斥口径、零值和缺失、流中累计覆盖、取消后的估算、细分量不重复计费、任意计量单位、价格缺项；报表新旧结果一致，原始事件和 nano-USD 总额可核对，分析缓存不成为额度权威。

## 4. 其他能力的范围与工程约束

### 4.1 Responses 兼容性先按路径判定

| 路径 | 当前行为 | 后续验证方向 |
|:--|:--|:--|
| Responses → Responses | [入站](../internal/adapter/openairesp/ingress.go) 读取必要字段，[出站](../internal/adapter/openairesp/egress.go) 局部改写，保留其余请求体 | 用固定客户端请求验证未知字段、自定义工具和 namespace 的原样保留；是否被上游接受仍取决于上游 |
| Responses → 其他协议 | [Lower](../internal/translate/responses_lower.go) 按有损转译规则处理；`client_metadata` 已明确丢弃 | 分别验证 `reject`、`drop` 和明确不支持的语义特性 |
| WS 或客户端私有信令 | 不由普通 HTTP 未知字段配置自动实现 | 先补客户端版本、脱敏请求/帧及失败位置，再决定是否扩展传输或 IR |

`translation.unknown_fields: drop` 允许丢弃未知字段，不代表任意输入项或工具类型都能正确转换。不要把它作为全局兼容性修复，也不要声称 `client_metadata` 必然触发 `json: unknown field`。动态改路由、子代理标头、自动压缩等行为只按具体样本评估；未经版本依据，不保留“突破官方并发实例限制”等结论。

### 4.2 客户端、媒体、隐私与桌面运维

- **Agent 接管与 CLI 桥接**：实际配置框架是 `internal/agent/`，不是 `internal/agents/`。本地进程桥接、订阅凭据管理与自动写客户端配置均会改变部署模型，应独立设计。源码中的实现不构成“费率减半”或官方订阅规则的证据。
- **媒体与搜索**：实际搜索入口在 `internal/gateway/search*.go`；[网页抽取][m-fetch] 导入 HTML 解析包，不能称为仅用标准库。图片解码、视频编码和抓取需要单独的输入、像素、内存、CPU、网络与超时预算；不能把每次请求调用 `debug.FreeOSMemory` 当作通用内存治理。
- **可逆内容脱敏**：[redact][m-redact] 可作模式参考，但必须验证工具参数、转义、跨 chunk、映射碰撞和租户隔离。日志掩码、敏感内容检测、可逆替换分别建模，不能承诺完全不改变模型或工具行为。
- **诊断与通知**：[profileRefused][m-heap] 的环回、Host、浏览器判定可参考；pprof 可能包含凭据和请求内容，应在受保护的管理入口提供。Bark 或其他通知是控制面可选功能，不能在计费热路径同步推送，也不能把原始错误或凭据放进通知。
- **桌面能力**：TUI 与 [防休眠][m-awake] 按本地部署需求评估，不列为服务器可靠性前提。监听端口复用也不能单独保证在途连接平滑迁移；仍需连接排空和关闭编排。

### 4.3 实现任务必须保持的约束

1. 协议知识留在适配器与转译层；`core` 只处理通用准入、路由、转发和结算，`ledger` 只认计量单位。
2. 不改变冻结导出 API、不擅自新增包的分层登记、不新增未经批准的依赖；当前直接依赖白名单仍只有 SQLite 和 YAML。
3. 数据面不直接访问磁盘或数据库；所有读写、队列、缓存、网络调用都有资源上限和取消路径。
4. 向客户端输出后不重试；已产生费用但交付失败时保留 Meter、结算一次，不因取消或 error 释放成未计费请求。
5. 借鉴 [LESSONS][m-lessons] 的失败注入与收尾原则，按本仓库规则用假时钟、同步信号和有界等待验证；race 不能替代死锁或资源泄漏测试，不使用固定 sleep 等待业务状态。

## 5. 建议推进顺序与验收门槛

这里的顺序是调查与设计顺序，不是已批准的开发任务，也不把候选机制直接定为 P0。

| 顺序 | 工作 | 进入实现前的证据 | 验收重点 |
|:--|:--|:--|:--|
| 1 | 收集兼容失败样本 | 固定客户端、模型、上游、请求路径；注明透传还是转译 | 本地可重现，密钥和用户内容已脱敏 |
| 2 | 验证已有保护与边界 | SSE 行/事件上限、慢读、流中断、SSRF URL 来源、分类冷却 | 不退化现有取消、预算、重试和结算保证 |
| 3 | 选择独立小改动 | 原子写或具体协议兼容问题有失败样例；报表优化有基准 | 明确涉及文件、最小方案及前后结果，不成批搬包 |
| 4 | 设计范围扩展 | WS、签名状态、显式别名、离线备份等有明确需求 | 独立任务卡；必要时先做 ADR、依赖与分层审查 |
| 5 | 评估产品能力 | Agent 接管、媒体、同步、TUI、通知的部署需求成立 | 单独定义资源、状态、安全、运维与恢复边界 |

每项实现任务应在任务卡写明涉及文件、必读规格、兼容模式、失败语义和最低测试；涉及 API 或行为契约的变化先按现有流程决定。代码验证运行 `go run ./tools/check`；性能提案另提供同机基准，备份提案另提供恢复演练。仅修改本文档不代表任何候选能力已落地。

<!-- Magpie 来源均固定到同一提交；更新版本时需重新核对符号、调用链与测试。 -->
[m-agent]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/agent/agent.go
[m-rewire]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/agent/rewire.go
[m-lock]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/claudelock/claudelock.go
[m-routing]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/routing.go
[m-provider-routing]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/provider/routing.go
[m-speed]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/speed.go
[m-sse]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/sse.go
[m-bare]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/sse_bare.go
[m-signature]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/gemini_signature.go
[m-anthropic]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/anthropic.go
[m-chat]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/chat.go
[m-redact]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/redact/redact.go
[m-sqlitedb]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/sqlitedb/sqlitedb.go
[m-otlp]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/otlp/otel_wire.go
[m-fetch]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/search_fetch.go
[m-cite]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/search_cite.go
[m-h264]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/h264/encode.go
[m-netguard]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/netguard/netguard.go
[m-lan]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/lan.go
[m-ws]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/responses_ws.go
[m-keep]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/codex_socket_keep.go
[m-heap]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/gateway/heap.go#L117
[m-bark]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/bark/bark.go
[m-awake]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/awake/awake.go
[m-backup]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/backup/backup.go
[m-dav]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/davsync/davsync.go
[m-sigv4]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/davsync/sigv4.go
[m-catalog]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/catalog/catalog.go
[m-samemodel]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/catalog/samemodel.go
[m-page]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/usage/request_page.go
[m-cells]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/usage/request_cells.go
[m-steady]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/internal/steady/steady.go
[m-lessons]: https://github.com/yetone/magpie/blob/7476328901e9c9236446640e9fbd33fe67930a6d/LESSONS.md
