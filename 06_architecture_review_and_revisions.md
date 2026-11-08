# 架构评审报告与设计修订（Architecture Review & Revisions）
# Universal AI Gateway (Project Titan) — Design Review v1.1

> **评审日期**：2026-11-07 | **评审对象**：README 与 00–05 共 7 份文档（约 8,000 行）
> **结论**：总体方向（纯 Go 单二进制、Hub-and-Spoke IR、控制面/数据面分离、WASM 扩展）成立，
> 但存在 **3 处架构级矛盾、6 处正确性缺陷（含示例代码 bug）、若干安全措辞失实与跨文档不一致**。
> 本文给出问题清单、修订后的架构决策（ADR），并记录本次已同步修改到原文的内容。
> **当原文与本文冲突时，以本文 ADR 为准。**

---

## 目录

1. [评审结论速览（Top 7）](#1-评审结论速览top-7)
2. [问题清单（按严重度分级）](#2-问题清单按严重度分级)
3. [修订后的架构决策（ADR-01 ~ ADR-11）](#3-修订后的架构决策adr-01--adr-11)
4. [修订后的请求生命周期](#4-修订后的请求生命周期)
5. [本次已同步修改的原文内容](#5-本次已同步修改的原文内容)
6. [遗留待办](#6-遗留待办)

---

## 1. 评审结论速览（Top 7）

| # | 问题 | 为什么严重 | 修订方向 |
|:-:|:---|:---|:---|
| 1 | **部署形态自相矛盾**：00/04 坚持「零 Redis、单 SQLite、纯内存账本」，01 §6 却要求 Redis Lua 预扣 + ClickHouse；同时宣称「企业级」，但内存账本 + 单文件 SQLite 只能单实例运行，没有任何 HA / 多副本方案 | 读者无法判断该实现哪一套；多实例部署会导致配额双花、熔断状态不同步（正是 01 §2.6 批评 new-api 的问题） | ADR-01：明确 **Standalone 与 Cluster 两档部署**，通过 `QuotaLedger` / `Store` 接口隔离 |
| 2 | **计费账本不可靠**：内存余额 1s 对账，进程崩溃丢失最多 1s 及全部在途预扣；`flushDeltas` 在事务失败时会丢失增量（代码 bug）；无不可变流水账，无法审计 | 计费是转售/内部分摊的核心，丢账或无法对账即不可用 | ADR-02：**预留-结算两阶段 + 追加式流水账 + 计费事件不可丢弃** |
| 3 | **缺少同协议透传路径**：所有流量强制 Lowering → IR → Synthesize；IR 不含 `cache_control`、未知字段、`metadata` 等 | Claude Code → Anthropic 是主流量，经 IR 往返会丢失 `cache_control`，直接摧毁文档自称的核心收益「Prompt Cache 命中率」 | ADR-03：**同协议透传优先**，IR 仅用于跨协议；IR 补齐缓存断点与扩展字段 |
| 4 | **调度策略相互打架**：快速路径（无视粘性/权重/Pace）、Smart（追逐最早重置）、Pace（匀速）、会话粘性四者没有优先级定义；Pace 迟滞在代码里根本没接入；快速路径不检查 `CooldownUntil` | 实际行为与文档承诺不符，且冷却中的 Key 会被快速路径选中 | ADR-04：**过滤 → 粘性 → 缓存冷点再平衡 → 池策略** 的固定决策顺序 |
| 5 | **Subagent 隔离的论据错误**：上游 Prompt Cache 按「组织/账号 + 精确前缀」匹配，短请求不会把主会话的缓存「挤出显存」；文中依赖的 `X-Claude-Code-Subagent`、`X-Agent-Subtask` 等请求头并无客户端实际发送 | 设计建立在不成立的机理上，识别信号也不存在 | ADR-05：隔离目标改为**限流配额隔离与模型分级**，识别改为可配置提取器 |
| 6 | **安全措辞失实**：「硬件级主密钥」实为与数据库同目录的普通文件；「信封加密」实为单密钥直接加密；AAD 宣称「防重放」；Go 字符串无法 Zeroize；Token 加盐方案三处写法互相矛盾（GlobalSalt / ClientSalt / `mup_` 前缀抄自其他项目） | 安全文档夸大会误导部署者，数据目录一旦整体备份泄露，密文与密钥同时失守 | ADR-07：真信封加密（KEK/DEK + 版本号）、HMAC 令牌索引、如实描述威胁模型 |
| 7 | **接入示例跑不通**：README 中 `cd gateway` 目录名错误；Claude Code 的 `ANTHROPIC_BASE_URL` 不应带 `/v1`；`ANTHROPIC_API_KEY` 走 `x-api-key` 头，而入口只解析 `Authorization: Bearer` | 快速上手第一步就失败 | 已修正 README；ADR-07 规定多协议凭据头解析 |

---

## 2. 问题清单（按严重度分级）

> **P0** = 架构级矛盾或会导致资金/数据错误；**P1** = 正确性缺陷或安全风险；**P2** = 不一致、措辞失实、可维护性。
> 「位置」列给出原文出处，便于对照。

### 2.1 P0：架构级

| ID | 位置 | 问题 | 修订 |
|:--|:--|:--|:--|
| A1 | 00 §2.1、01 §6、04 §2.3 | 「零外部依赖」与「Redis Lua + ClickHouse」两套方案并存；无多实例方案 | ADR-01 |
| A2 | 04 §2.3、00 §4.2 阶段十 | 内存余额崩溃即丢；预扣在途无持久化；无流水账；对账写 `used_quota` 原地更新不可审计 | ADR-02 |
| A3 | 00 §4.1 时序图 | 阶段十才「原子扣减」，与 04 的预扣设计矛盾；若按 00 实现，100 个并发长流式请求可在余额校验通过后集体透支 | ADR-02 |
| A4 | 02 全文 | 无透传路径；IR 无 `cache_control`、无未知字段保留，「语义超集」不成立 | ADR-03 |
| A5 | 03 §4、§7.1 | 快速路径/Smart/Pace/粘性无统一优先级；Pace 迟滞仅存在于公式与测试，`Acquire` 未调用 | ADR-04 |

### 2.2 P1：正确性与安全

| ID | 位置 | 问题 | 修订 |
|:--|:--|:--|:--|
| B1 | 04 §2.3 `flushDeltas` | `BeginTx`/`Prepare`/`Commit` 失败直接 `return`，已 `Swap(0)` 的增量全部丢失；`Exec` 失败时只归还当前一条，之前已回滚的条目与之后未执行的条目均丢失；`Commit` 错误被忽略 | **已修复代码**：失败时整批归还 |
| B2 | 04 §4.4 `ExecuteOnRequest` | `wazero.NewRuntime` 默认不响应 context 取消，「50ms 超时熔断」无效，死循环插件会永久占住 goroutine；未设内存上限；`sync.Pool` 会在 GC 时静默丢弃实例且不 `Close`（泄漏）；trap 后的实例被放回池中复用 | **已修复代码**：`WithCloseOnContextDone`、`WithMemoryLimitPages`、有界 channel 池、trap 实例丢弃 |
| B3 | 05 Sprint 3 `SelectFastPath` | `Load()==0` 后 `Add(1)` 存在 TOCTOU 竞态，两个 goroutine 可同时占用 `MaxInFlight=1` 的本地端点；不检查冷却 | **已修复代码**：CAS 0→1 + 冷却检查 |
| B4 | 03 §7.1 `Acquire` | 快速路径只看 `State==Ready`，不看 `CooldownUntil`（`isKeyUsable` 才看），冷却中的 Key 会被选中；慢速路径 `Select` 后无条件 `Add(1)`，并发下可超过 `MaxInFlight` | **已修复代码**：统一 `tryAcquire` CAS 循环 |
| B5 | 02 §4.1 映射表 | Anthropic 拒绝无签名/伪造签名的 `thinking` 块，「DeepSeek/Gemini 思考 → 构造 thinking 块」会触发 400；DeepSeek reasoner 在输入消息中携带 `reasoning_content` 会被拒绝，「原样注入」错误 | ADR-03：跨厂商思考内容**只降级为不可回传的展示文本或直接丢弃** |
| B6 | 02 §4.2 `CleanSchemaForGemini` | `["null","string"]` 取首元素得到 `NULL` 类型；`anyOf`/`oneOf`/`$ref` 被静默丢弃导致参数约束丢失 | **已修复代码**：联合类型转 `nullable`；`$ref` 需先内联（列入待办） |
| B7 | 03 §3.3 / 03 §6.1 | 401 一律永久隔离：OAuth 访问令牌过期也返回 401，应先刷新；403 混合了「地区/模型权限」与「封禁」；缺少 Anthropic `529 overloaded`；未区分「首字节前」与「流已开始」的错误——流开始后无法透明降级 | ADR-06 |
| B8 | 03 §3.3 | TTFT 超时 8s 对 100k+ 上下文与扩展思考模型过激，会把正常请求误判为故障并级联切换 | ADR-06：按模型/上下文长度/思考预算动态计算 |
| B9 | 03 §3.2 DAG | Claude → DeepSeek 跨模型兜底在会话中途静默换模型：思考签名、工具 schema、上下文窗口全部不兼容，客户端不知情 | ADR-04：跨模型降级必须由虚拟模型显式声明并在响应中如实返回 `model` |
| B10 | 04 §3.1 | AAD = `provider:fingerprint:created_at`，不含行主键与表名，整行三列一起复制即可绕过「防置换」；图中声称绑定 `key_id` 与代码不符 | ADR-07 |
| B11 | 04 §3.3 | 「连续失败后服务端自旋等待 2^N 秒」等于自我 DoS | ADR-08：直接返回 429 + 锁定窗口 |
| B12 | 02 §2.4 | RFC1918 白名单被当作认证；`100.64.0.0/10`（CGNAT）被视为私网；反向代理后 `RemoteAddr` 失真；mDNS 默认广播扩大攻击面；局域网明文 HTTP 传输 Bearer Token | ADR-08 |
| B13 | 02 §5.3 | 「SIMD 启发式扫描 `"content":"` 偏移量」绕过 JSON 解析：转义引号、`\u` 转义、工具参数中的同名字段都会误判；而 JSON 解码开销相对网络 RTT 可忽略 | ADR-03：使用流式 JSON 解码 + 池化缓冲 |
| B14 | 02 §5、04 §2.3 | 未要求向 OpenAI 上游注入 `stream_options.include_usage=true`，流式响应没有 usage，结算只能回退到本地分词估算 | ADR-02 |
| B15 | 01 §3.5、01 §6、03 §2.2 | 订阅账号逆向池化、uTLS 伪造 JA3/JA4 指纹、住宅代理防封被列为目标架构的一层；这违反上游服务条款，带来封号与法律风险，且与「企业级/合规路由（ZDR/HIPAA）」定位冲突 | ADR-11：移出目标架构 |

### 2.3 P2：一致性、措辞与可维护性

| ID | 位置 | 问题 | 处理 |
|:--|:--|:--|:--|
| C1 | README、00、04 | Token 前缀三种写法：`uai_live_`、`uai_cmnl_live_`、`mup_live_`（后者来自 MinerUProxyManager） | **已统一**为 `uai_live_` |
| C2 | 00 §3.7.2 vs 03 §6.2 | `TcpTimedWaitDelay` 默认值 240s vs 120s；`MaxIdleConns` 2048/512 vs 20000/2000 | **已统一**到 03 的取值；现代 Windows 调整端口范围应使用 `netsh int ipv4 set dynamicport`，`MaxUserPort` 仅对旧版系统有效 |
| C3 | 00 §3.3.1 | 示例称「候选需 >13.75 才可切换」，但同图 Band 3 起点为 13.31；分段（跨桶）与比例（>10%）是两种不同规则 | **已修正**：以比例规则为准，分段仅用于展示 |
| C4 | README vs 03 §5.3 | 粘性缓存一处写 LRU，一处写 FIFO Ring；表名 `resource_bindings` vs `session_affinity` | ADR-04：统一为 TTL 驱动、按访问续期的分片 LRU，表名 `resource_bindings` |
| C5 | 03 §2.1 vs 04 DDL vs 05 | 资源状态枚举三套：`Ready/Busy/Cooldown/Quarantined/Degraded`、`active/cooldown/suspended`、`Ready/Cooldown/Quarantined` | ADR-06 给出唯一枚举 |
| C6 | 02、05 | Go module 路径 `github.com/ai-gateway/...` 与实际仓库 `github.com/ZhangCurosr/...` 不符 | **已统一** |
| C7 | README、00 §2.2–2.3 | 35,000 QPS、<50ns、命中率 >90%、成本降至 15–20%、Subagent 挤出率 0% 等均为未经基准验证的断言 | ADR-10：改为「设计目标」并给出测量口径 |
| C8 | 05 Sprint 3 测试 | `TestPaceHysteresisBanding_CacheHitRate` 中两个 Pace 始终在 ±0.2% 内波动，从未接近 10%，测试必然通过，无法证明任何关于缓存命中率的结论；`Benchmark..._50KQPS` 并不测量 QPS | 列入待办：基于真实轨迹回放的命中率仿真 |
| C9 | 04 §3.1 | 对 32 字节随机主密钥做 PBKDF2-10 万轮 + 固定盐无安全收益（PBKDF2 用于低熵口令） | ADR-07：改用 HKDF 按用途派生子密钥 |
| C10 | 02 §2.3 | MCP 版本停留在 2024-11-05 + HTTP SSE 传输，该传输已被 Streamable HTTP 取代；README 又把 MCP 列为「上游 Egress」，MCP 的角色（运维接口 vs 工具中继）未定义 | ADR-09 附注 |
| C11 | README §5.3 vs 04 §3.3 | 管理 API 示例使用 `X-Admin-Key` 头，04 描述 Cookie 会话 + CSRF，两套鉴权并存且未定义初始管理员如何产生 | ADR-08 |
| C12 | 全文 | 示例模型（`claude-3-5-sonnet-20241022`、GPT-4o、Gemini 2.0）多已过时 | 模型名应全部来自配置，示例不影响架构，暂不逐一替换 |

---

## 3. 修订后的架构决策（ADR-01 ~ ADR-11）

### ADR-01：部署形态分两档，存储与账本通过接口隔离

**决策**：

| 档位 | 目标场景 | 存储 | 配额账本 | 共享状态（熔断/粘性） | 实例数 |
|:--|:--|:--|:--|:--|:--|
| **Standalone**（v1 默认） | 个人、小团队、本地 Agent 网关 | 嵌入式 SQLite（WAL） | 进程内存 + SQLite 流水账 | 进程内存 | **严格 1 个**（启动时对数据目录加文件锁，防止误起第二个进程） |
| **Cluster**（v2 规划） | 多租户转售、企业集中出口 | PostgreSQL | Redis（Lua 预留/结算）+ PG 流水账 | Redis（Pub/Sub 广播冷却与隔离事件） | N 个无状态副本 |

```go
// internal/store：两档实现同一组接口，数据面只依赖接口
type QuotaLedger interface {
    Reserve(ctx context.Context, clientID int64, reqID string, units int64) (Reservation, error)
    Settle(ctx context.Context, r Reservation, actualUnits int64) error
    Release(ctx context.Context, r Reservation) error // 失败/取消且未产生上游用量
}
type SharedState interface {
    MarkCooldown(keyID int64, until time.Time)
    MarkQuarantine(keyID int64, reason string)
    Subscribe(func(Event)) // Standalone 下为进程内 fan-out
}
```

**理由**：「零依赖单二进制」是个人/团队场景的核心卖点，应保留为默认；但不能同时宣称它能承担多副本企业场景。01 §6 的 Redis/ClickHouse 方案降级为 Cluster 档的可选实现，而不是与 Standalone 并列的「终极架构」。

**影响**：README 的「企业级」表述限定为「Standalone 档位面向单节点」；新增 `--data-dir` 文件锁。

---

### ADR-02：计费改为「预留 → 结算」+ 追加式流水账，计费事件不可丢弃

**问题回顾**：00 写的是「流结束后才扣」，04 写的是「预扣 + 差额」，且两者都把账本只放在内存。

**决策**：

1. **准入时预留（Reserve）**：
   `reserve = price_in × prompt_tokens_est + price_out × min(max_tokens, model_cap)`；
   请求未给 `max_tokens` 时使用模型配置的 `default_reserve_tokens`（不得为无穷）。
   内存中 `Available = Balance − Σ在途预留`，CAS 扣减，失败返回 402/429（与入站协议的错误格式一致）。
2. **结算（Settle）必须在 `defer` 中执行**：正常结束、客户端断开（`context.Canceled`）、上游中断三种路径都要结算；断开时以已收到的 usage（或已转发 token 的本地计数）结算，**不得直接 Release**——上游已经计费。
3. **用量来源**：向 OpenAI 兼容上游强制注入 `stream_options: {"include_usage": true}`；Anthropic 取 `message_start` + `message_delta` 的 usage；本地分词仅作兜底并在流水账中标注 `usage_source=estimated`。
4. **两条通道分离**：
   - **审计日志通道**（Prompt/响应摘要、耗时）：维持「满则丢弃 + 计数」——可以丢。
   - **计费事件通道**：**不可丢弃**。通道满时对**新请求**返回 503（准入背压），绝不丢已发生的用量。
5. **追加式流水账**（替代原地 `UPDATE used_quota`）：

```sql
CREATE TABLE IF NOT EXISTS usage_ledger (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id    TEXT    NOT NULL UNIQUE,      -- 幂等键：重复写入被唯一约束拒绝
    client_id     INTEGER NOT NULL,
    key_id        INTEGER NOT NULL,
    model         TEXT    NOT NULL,
    input_tokens  INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_units    INTEGER NOT NULL,             -- 1 USD = 1e8 units
    usage_source  TEXT    NOT NULL,             -- upstream | estimated
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ledger_client_time ON usage_ledger(client_id, created_at);
```

   `clients.used_quota` 降级为缓存列，可随时由 `SUM(cost_units)` 重建。
6. **崩溃语义（如实写明）**：启动时 `Balance = total_quota − SUM(ledger)`；崩溃瞬间仍在途的请求最多漏记「在途并发数 × 单请求成本」，批量刷盘间隔（默认 250ms）内已结算但未落盘的事件同样会丢失。需要更强保证的部署把 `flush_interval` 调小或改用 Cluster 档。优雅关闭时必须先停止准入、等待在途请求结算、再刷盘。

---

### ADR-03：同协议透传优先，IR 仅用于跨协议；IR 补齐缓存与扩展字段

**决策**：

```
入站协议 == 目标上游协议 ?
   ├─ 是 → Passthrough 路径：只改写 model / 鉴权头 / 必要的 stream_options，
   │        请求体其余字节原样转发（json 局部改写），SSE 逐帧转发并旁路解析 usage
   └─ 否 → IR 路径：Lowering → ir.Request → Synthesize
```

- Claude Code → Anthropic、OpenAI SDK → OpenAI 等主流量全部走透传，保真度与缓存断点天然无损，CPU 开销最低。
- IR 补充字段：
  - `CacheControl *CacheHint`（挂在 System 块、Message、Tool 上），跨协议时映射为 Anthropic `cache_control` / OpenAI `prompt_cache_key`，无法映射时丢弃并记录；
  - `Extensions map[string]json.RawMessage`（每个 Part 与 Request 级别），保存未识别字段，同协议回写时还原；
  - `ToolResultPart.Content []ContentPart`（工具结果可含图片，Claude Code 读图场景需要）；
  - `RedactedThinkingPart{Data string}`（`redacted_thinking` 的载荷是 `data` 而非文本）；
  - 响应侧 `StopReason`、`Usage{Input, Output, CacheRead, CacheWrite}`。
- **思考内容跨厂商规则**（替换 02 §4.1 表格中的错误行）：
  - 签名只对**签发它的同一厂商**有效。跨厂商时：签名丢弃，思考文本要么丢弃，要么以普通文本（非 thinking 块）放入历史，**禁止**伪造无签名 `thinking` 块发给 Anthropic；
  - DeepSeek reasoner：历史 assistant 消息中的 `reasoning_content` 在发往上游前**必须删除**；
  - OpenAI Responses：推理项以 `reasoning` item + `encrypted_content` 回传，只能回传给 OpenAI。
- **工具调用自愈**改为**可配置且默认仅记录告警**：自动合成 `cancelled` 工具结果会改变对话语义并掩盖客户端 bug，仅在显式开启 `heal_dangling_tool_calls` 时生效。
- **流式解析**：使用逐事件 JSON 解码（`json.Decoder` 或等价的流式解析器）+ `sync.Pool` 缓冲，不再使用字段偏移启发式扫描。FSM 需覆盖：`tool_use` 块与 `input_json_delta`、并行工具调用的交错 index、流中 `error` 事件、`ping`、最终 usage 帧。

---

### ADR-04：调度采用固定决策顺序，粘性优先、在缓存冷点再平衡

**决策**（`Acquire` 的唯一合法顺序）：

```
1. 硬过滤：能力位、合规位（ZDR 等）、模型映射、State ∈ {Ready}、CooldownUntil ≤ now、InFlight < Max
2. 会话粘性：若亲和键已绑定 Key 且通过硬过滤，且该 Key 剩余窗口额度 > 安全水位 → 直接使用（tryAcquire CAS）
3. 缓存冷点再平衡：若绑定 Key 距上次使用已超过上游缓存 TTL（Anthropic 默认 5 分钟；用了 1h 缓存则为 1h），
   缓存已失效，切号没有代价 → 允许按池策略重新选择
4. 池策略：每个池只配置一种策略
     - DirectKey 池 → SWRR（延迟衰减）
     - 订阅/窗口配额池 → Pace（迟滞 10% 仅在步骤 2 的「是否主动迁移」判断中使用）
     - 自托管端点池 → 最少在途（Least InFlight）
5. 绑定：成功后写入亲和缓存（TTL = 上游缓存 TTL，命中即续期）
```

- 原「O(1) 快速路径」降级为步骤 4 中 SWRR/最少在途的实现优化，**仅用于无亲和键的请求**，且必须走同一个 `tryAcquire`（CAS + 冷却检查）。
- `Smart`（追逐最早重置）与 `Pace`（匀速）目标相反，同一池只能二选一，由配置决定。
- **重试与降级预算**：
  - 只有在**向客户端写出第一个字节之前**允许透明重试/降级；之后的错误只能以协议内错误事件结束流；
  - 每请求最多 `max_attempts=3`、总时间预算为客户端超时的 80%；
  - 跨模型降级（Claude → DeepSeek）必须由虚拟模型显式声明 `allow_cross_model: true`，并在响应中返回真实 `model`；默认只在**同一模型的不同供给渠道**之间降级（Direct → Bedrock → Vertex）。
- 亲和缓存：统一为 64 分片 LRU + TTL，容量按「活跃会话数 × 2」配置，不再使用 FIFO（FIFO 会按插入顺序淘汰仍在活跃的长会话）。

---

### ADR-05：Subagent 分流的目标是限流隔离与模型分级，而非「防止缓存挤出」

**机理修正**：Anthropic/OpenAI 的 Prompt Cache 按「组织（或账号）+ 精确前缀」命中，短请求使用不同前缀，不会覆盖主会话的缓存条目；网关也无法感知上游 GPU KV Cache。真正需要隔离的原因是：

1. Subagent 突发并发消耗同一账号的 RPM/TPM 或订阅窗口，导致主会话被 429 后**被迫切号**，从而丢失缓存；
2. Subagent 通常可以用更便宜/更快的模型。

**决策**：
- 识别信号改为**可配置的提取器链**，每个提取器给出置信度，例如：请求体 `metadata.user_id` 中的会话段、系统提示词前缀指纹、模型名（客户端为子任务指定的小模型）、自定义请求头（由用户自行在客户端配置，网关不假设任何客户端会发送）。
- 隔离方式：主会话池为每个 Key **预留并发/额度余量**（如 30%），Subagent 只能使用剩余部分；而不是强制把 Subagent 赶到物理隔离的账号池。
- 删除「Subagent 缓存冲突挤出率 0%」等无法测量的指标。

---

### ADR-06：错误分类与资源状态机修订（唯一枚举）

**资源状态唯一枚举**：`Ready`、`Cooldown`（有截止时间）、`Open`（熔断）、`HalfOpen`、`Suspect`（待验证）、`Quarantined`（需人工处理）。`Busy` 不是状态，而是 `InFlight ≥ Max` 的派生条件。DDL 中 `state` 列采用同一组值。

| 上游信号 | 处置 |
|:--|:--|
| 401（DirectKey） | → `Suspect`，后台用 `GET /v1/models` 验证一次；仍 401 → `Quarantined` + 告警 |
| 401（OAuth 类凭据） | 先走单飞（singleflight）刷新令牌并重试一次；刷新失败 → `Quarantined` |
| 403 `permission_error` / 地区限制 | 仅对该 (Key, 模型) 组合打标「不可用」，Key 本身保持 `Ready` |
| 403 账号停用 / 计费问题 | `Quarantined` |
| 429 | `Cooldown`，优先使用 `retry-after` 与 `anthropic-ratelimit-*-reset`；区分请求数与 token 维度 |
| 529 `overloaded_error` / 503 | 不惩罚单个 Key（这是厂商全局过载），在**渠道级**退避并尝试其他渠道 |
| 500/502/504、连接错误 | 计入滑动窗口熔断 |
| 本地错误（WSAENOBUFS、DNS 失败、TLS 握手被本机代理拦截） | 不惩罚任何 Key，本地背压（保留 03 §6.2 设计） |
| 首字节后出错 | 不重试；向客户端发送协议内错误事件；计入熔断统计 |

- TTFT 超时：`ttft_timeout = base(10s) + k × prompt_tokens + thinking_budget × t_per_token`，并按模型可配置上限；流停滞超时同理与思考预算挂钩。
- 主动探测优先使用**不消耗 token 的端点**（`/v1/models`、`count_tokens`），避免探测本身产生费用或触发风控。

---

### ADR-07：凭据与密钥管理

1. **措辞**：删除「硬件级」。准确表述为「本地主密钥文件，可替换为 KMS / 操作系统密钥库」。只有接入 TPM/HSM/KMS 时才能称为硬件保护。
2. **主密钥来源优先级**：`GATEWAY_KMS_URI`（云 KMS / Vault Transit）> 操作系统密钥库（Windows DPAPI、macOS Keychain、Linux Secret Service）> `--master-key-file`（**默认路径不得位于 `--data-dir` 之下**，避免数据目录整体备份时密文与密钥一起泄露）。
3. **真正的信封加密**：每条上游凭据生成随机 DEK 加密明文；DEK 由 KEK（主密钥）包裹后与密文一起存储；记录 `kek_version`，轮换 KEK 只需重新包裹 DEK。
4. **子密钥派生**：`HKDF-SHA256(master, info="uaig/v1/dek-wrap")`、`HKDF(master, info="uaig/v1/token-pepper")`，不再使用 PBKDF2。
5. **AAD**：`"uaig:v1:upstream_keys:" + id + ":" + provider + ":" + kek_version`，包含表名与主键，整行复制到其他行也会校验失败。插入时先拿到自增 id 再加密（同一事务内两步写）。
6. **下游 Token**：格式 `uai_live_` + 32 字节随机数 Base64URL；索引值为 `HMAC-SHA256(token_pepper, token)`，单一全局 pepper 存放于数据库之外，因此既能 O(1) 查表，又能在只泄露数据库时抵抗离线验证；展示前缀取 `uai_live_` 之后的前 6 个随机字符（原方案前 12 位不含随机部分，无法区分 Token）。上游凭据去重用的 `fingerprint` 同样改为 HMAC。
7. **多协议凭据头**：入口按顺序解析 `Authorization: Bearer`、`x-api-key`（Anthropic SDK / Claude Code 默认）、`x-goog-api-key` / `?key=`（Gemini）。
8. **内存擦除**：Go 字符串不可变且会被复制，无法可靠擦除。如实写明「明文凭据在进程生命周期内常驻内存」，威胁模型中排除「攻击者可读取进程内存」。删除 Zeroize 能防止内存转储的说法。
9. AAD 不提供防重放能力；Bearer Token 天然可重放，防护依赖 TLS 与令牌吊销。删除「防重放」表述。

---

### ADR-08：管理面、局域网与传输安全

- 管理面使用**独立监听**：默认 `127.0.0.1:8081`，与数据面 `:8080` 分离；只有显式配置才可绑定到非回环地址，且必须启用 TLS。
- 管理鉴权统一为一种：首次启动生成一次性 bootstrap token 打印到 stderr，用于创建管理员账号；之后使用 Cookie 会话 + CSRF（04 §3.3 的方案）。`X-Admin-Key` 头仅保留给 CLI，且需签发独立的管理 API Token（存储方式同 ADR-07 第 6 条）。
- 暴力破解：失败计数超过阈值后直接返回 429 并锁定窗口，**不在服务端 sleep**。
- 局域网：IP 段过滤只是网络层的「减少暴露面」，**不能替代认证**；移除 `100.64.0.0/10`；部署在反向代理后需配置可信代理列表才解析 `X-Forwarded-For`；非回环访问默认要求 TLS（可自动生成自签证书并展示指纹）；mDNS 广播默认关闭。

---

### ADR-09：WASM 插件运行在 IR 之上，并给出真实的资源边界

- **挂载点后移**：`OnRequest` 在 Lowering 之后运行，插件读写的是 `ir.Request` 的稳定 JSON 视图（版本化 ABI：`abi_version=1`），而不是 N 种原始协议体；透传路径上仅提供只读视图，插件若需改写，该请求自动切换到 IR 路径。
- **资源边界**：`RuntimeConfig.WithCloseOnContextDone(true)` + 每次调用超时；`WithMemoryLimitPages`（默认 256 页 = 16MB）；有界实例池；trap/超时的实例直接关闭不回收。
- **失败策略**：每个插件声明 `fail_mode: open | closed`（合规类插件默认 closed）。
- **流式审查**：跨 chunk 的敏感词需要滑动窗口，`OnStreamChunk` 采用「延迟窗口」（默认保留最近 N 个字符不下发，确认安全后再刷出），这会引入可配置的额外延迟，文档必须如实说明，不能再宣称「零影响 TTFT」。
- **`OnResponse`** 对流式响应只能做事后审计，不能改写已下发内容（删除「流式水印注入」）。
- **性能表述**：宿主与沙箱之间是「拷贝进 / 拷贝出 + JSON 编解码」，不是零拷贝；调用开销与载荷大小成正比（一次 100k token 的请求体约数百 KB），「<15µs」只适用于小载荷。
- **MCP 角色**：v1 只把 MCP 作为**运维接口**（只读工具：查询额度、命中率、指标），传输层采用 Streamable HTTP，需管理 Token 鉴权；`switch_model` 等写操作需单独授权。「MCP 工具中继」不在 v1 范围。

---

### ADR-10：性能与经济性指标改为「设计目标 + 测量口径」

| 指标 | 原文表述 | 修订后 |
|:--|:--|:--|
| 吞吐 | 35,000+ QPS | 目标：mock 上游、1KB 非流式请求、8 核下 ≥ 20k RPS；以 `bench/` 中的脚本与硬件规格为准 |
| 调度开销 | < 400ns | 目标：P99 < 5µs（含粘性查找），以 `go test -bench` 结果为准 |
| 计费 | < 50ns | 删除。计费开销不会是瓶颈，重点是正确性（ADR-02） |
| TTFT 附加 | < 0.2ms | 目标：透传路径 P99 < 1ms，IR 路径 P99 < 3ms |
| 缓存命中率 | > 90% | 以「真实 Claude Code 会话轨迹回放」测量粘性保持率与上游返回的 `cache_read_input_tokens` 占比；不承诺固定数值 |
| 成本降至 15–20% | — | 删除。缓存收益主要由客户端的 `cache_control` 断点与上游定价决定，网关的作用是「不破坏」 |

---

### ADR-11：合规边界 —— 移除订阅逆向与指纹伪装

- 订阅账号（Claude Pro/Max、ChatGPT Plus 等）的 OAuth 逆向池化、uTLS 伪造 Node.js/Chrome 的 JA3/JA4 指纹、住宅代理 1:1 绑定以规避风控，均违反上游服务条款，存在账号封禁与法律风险，**不进入本项目目标架构**。
- 01 中对 sub2api 的分析保留为「竞品调研」，但其 §6「逆向与防封对抗层」不再作为设计输入。
- 资源类型保留：`DirectKey`、云厂商托管（Bedrock / Vertex / Azure）、企业授权的服务账号、自托管端点。`Smart`/`Pace` 算法仍可用于**有窗口配额的合法凭据**（如按日/按月预算的企业 Key）。

---

## 4. 修订后的请求生命周期

```
 1. Ingress           路由按路径确定协议（嗅探仅作显式开启的兜底）；解析三类凭据头
 2. Auth              HMAC(pepper, token) → 内存索引；未命中 401
 3. RateLimit         RPM 令牌桶；TPM 用预估 token 预占，结算时校正
 4. Reserve           QuotaLedger.Reserve(预估成本) → 失败 402/429
 5. Route             虚拟模型解析 → 决定目标协议
 6. Transform         同协议 → Passthrough；跨协议 → Lowering → IR
 7. Plugin OnRequest  （IR 视图；透传路径只读）
 8. Acquire           过滤 → 粘性 → 冷点再平衡 → 池策略（CAS 占槽）
 9. Upstream          首字节前可在预算内重试/降级；注入 include_usage
10. Stream            逐帧转发/转译；旁路解析 usage；OnStreamChunk 延迟窗口（若启用）
11. Settle (defer)    以上游 usage 结算 → 计费事件（不可丢）→ 流水账
12. Audit log         审计日志（可丢弃，计数告警）
```

---

## 5. 本次已同步修改的原文内容

| 文件 | 修改 |
|:--|:--|
| `README.md` | 修正 `cd` 目录名；Claude Code 接入改为 `ANTHROPIC_BASE_URL` 不带 `/v1` + `ANTHROPIC_AUTH_TOKEN`；入口凭据头说明；删除「硬件级」「防重放」；性能矩阵标注为设计目标；文档索引新增 06 |
| `00_background_philosophy_and_developer_guide.md` | 统一 Windows 网络参数；修正 Pace 切换阈值示例；计费、Subagent、凭据描述加修订指引；红线七、红线九修订 |
| `01_competitive_teardowns.md` | §6 增加修订说明（Redis/ClickHouse 归入 Cluster 档；逆向/防封层移出范围） |
| `02_protocol_translation_and_ir.md` | 修正 module 路径；§4.1 思考映射表错误行；`CleanSchemaForGemini` 联合类型处理；§1 增加透传路径说明 |
| `03_scheduling_pooling_and_resilience.md` | `Acquire` 快速路径冷却检查与慢速路径 CAS 占槽；§3.3、§5.2 增加修订指引 |
| `04_storage_security_and_implementation.md` | `flushDeltas` 失败归还整批；WASM 宿主代码超时/内存/实例池修复；Token 前缀统一为 `uai_live_` 并改为 HMAC；措辞修正 |
| `05_engineering_implementation_checklist.md` | `SelectFastPath` CAS 与冷却检查；module 路径 |

> 原文中大段的 ASCII 图与叙述未逐处改写；凡与本文 ADR 冲突的内容，以本文为准。

---

## 6. 遗留待办

- [ ] 按 ADR-02 重写 04 §2.3 的完整代码（`QuotaLedger` Standalone 实现 + `usage_ledger` 批量写入）。
- [ ] 按 ADR-03 在 02 §6 中补充 `internal/passthrough` 实现骨架与 IR 新字段的 Go 定义。
- [ ] 按 ADR-04 重写 03 §7.1 `Acquire`（冷点再平衡 + 每池单策略），并把 Pace 迟滞真正接入。
- [ ] `CleanSchemaForGemini` 支持 `$ref`/`$defs` 内联与 `anyOf` 降级策略。
- [ ] 用真实会话轨迹替换 05 Sprint 3 的缓存命中率「测试」。
- [ ] 05 Sprint 计划同步：Sprint 1 增加透传路径（应早于 IR，最先交付可用价值）；Sprint 4 增加流水账与崩溃恢复测试。
- [ ] 补充威胁模型章节（资产、攻击者能力、明确不防护的范围）。
