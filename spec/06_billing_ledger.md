# 06 计费与账本（Billing & Ledger）

代码位置：`internal/pricing`（已提供参考实现）、`internal/ledger`（接口已定义，实现为任务 T05/T06）。

## 1. 金额与计量单位

- 所有金额是 `int64` 的 **nano-USD**：1 USD = 1,000,000,000。**禁止**在成本计算中使用 `float64`。
- 计费以**计量单位**为基础。单位是出站适配器定义的可计数量，名称满足 `^[a-z][a-z0-9_]{0,63}$`：

| 单位 | 含义 | 典型协议 |
|:--|:--|:--|
| `input_tokens` | 不含缓存读写的普通输入 token（PRC-02） | 大模型 |
| `output_tokens` | 输出 token | 大模型 |
| `cache_read_tokens` / `cache_write_tokens` | 缓存命中 / 写入的输入 token | 大模型 |
| `requests` | 调用次数 | 按次计费的 API |
| 其他（`credits`、`pages`、`characters`、`seconds`……） | 由适配器在 spec/08 中定义 | 搜索、文档解析、语音…… |

- 每个出站适配器用 `Egress.Units()` 声明它可能报告的单位；每个模型的价目表必须为这些单位全部定价（CORE-05）。

## 2. 成本计算（`pricing`，已提供）

```go
type Usage map[string]int64                 // 单位 → 数量
type Rate  struct { NanoUSD, Per int64 }     // 每 Per 个单位 NanoUSD
type Price map[string]Rate                   // 单位 → 单价
func ParseRate(s string) (Rate, error)       // "3/1M"、"0.008"、"1.5/1K"
func Cost(p Price, u Usage) (nano int64, unpriced []string)
func ReserveUsage(estimate, caps Usage) Usage
func EstimateInputTokens(bodyBytes int) int64      // 大模型入站适配器用
func EstimateOutputTokens(forwardedBytes int64) int64 // 大模型出站适配器用
```

| 编号 | 规则 |
|:--|:--|
| PRC-01 | `Cost = ceil(Σ 用量 × NanoUSD / Per)`：各项**精确**累加（有理数）后整体向上取整；超过 int64 饱和为 `MaxInt64`；有用量但未定价的单位按 0 计并在 `unpriced` 中返回 |
| PRC-02 | `input_tokens` 是**不含**缓存读写的普通输入 token（与 Anthropic 一致）。OpenAI 的 `prompt_tokens` 包含缓存命中部分，适配器必须先减去（spec/08 USE-04） |
| PRC-03 | 预留用量 `ReserveUsage(env.Estimate, models[].reserve)`：Estimate 中有的单位取 `min(估计, reserve)`（reserve 中没有该单位则取估计）；Estimate 中**没有该键**、reserve 中有的单位取 reserve。即 `reserve` 既是上限也是默认值。Estimate 中**显式为 0** 的单位表示确定不会产生，不会被 reserve 补上 |
| PRC-04 | 任一用量为负数时视为 0 |
| PRC-05 | 价格字符串 `USD[/PER]`：USD 为非负十进制，最多 9 位小数（精确换算为 nano-USD，不经过浮点）；PER 为正整数，可带 `K`（×1000）或 `M`（×1,000,000）后缀，缺省为 1 |

例：`claude-sonnet` 配置 `price: {input_tokens: "3/1M", output_tokens: "15/1M", ...}`、`reserve: {output_tokens: 64000}`；请求 `max_tokens: 1024`、请求体 3000 字节 → Estimate `{input_tokens: 1000, output_tokens: 1024}` → 预留 `1000 × 3e9 / 1e6 + 1024 × 15e9 / 1e6 = 3,000,000 + 15,360,000 = 18,360,000` nano-USD。
`tavily-search` 配置 `price: {credits: "0.008"}`、`reserve: {credits: 2}`；请求未声明深度 → Estimate 为空 → 预留 2 credits = 16,000,000 nano。

## 3. 账本接口（`internal/ledger/ledger.go`，已提供签名）

```go
type Ledger interface {
    Reserve(clientID int64, requestID string, amount int64) (*Reservation, error)
    Settle(r *Reservation, ev store.UsageEvent) error
    Release(r *Reservation)
}

type Account struct { ClientID int64; Unlimited bool; QuotaNanoUSD int64; SpentNanoUSD int64 }

// Memory 是 Standalone 部署的实现。
func NewMemory(cfg MemoryConfig) *Memory
func (m *Memory) LoadAccounts(accts []Account)          // 启动时调用
func (m *Memory) UpsertAccount(a Account)               // 管理 API 修改额度后调用（保留 Spent 与在途预留）
func (m *Memory) Snapshot(clientID int64) (AccountSnapshot, bool)
func (m *Memory) Events() <-chan store.UsageEvent       // 供 Flusher 消费
```

### 3.1 内存账户状态

每个客户端一个账户，以 `atomic.Pointer` 发布不可变的整体快照：

```
unlimited  bool         // 与额度一起原子更新
quota      int64        // 额度上限（unlimited 时忽略）
spent      int64        // 已结算成本（启动时 = SUM(usage_events.cost)）
reserved   int64        // 在途预留合计
available  = quota - spent - reserved
```

预留、结算、释放、管理更新都以整体快照 CAS 为原子边界；已发布的快照永不修改、不复用为后续版本。只对 `reserved` 做 CAS 无法排除结算与再预留造成的 ABA。`Snapshot` 一次读取整个版本。

### 3.2 行为规格

| 编号 | 方法 | 行为 |
|:--|:--|:--|
| LED-01 | — | 预留金额由 `core` 计算：`pricing.Cost(price, pricing.ReserveUsage(env.Estimate, reserve))`，`amount <= 0` 时按 1 处理（保证每个请求都有预留记录） |
| LED-02 | `Reserve` | 账户不存在 → `apperr.CodeUnauthenticated`（客户端刚被删除） |
| LED-03 | `Reserve` | 非 unlimited 且 `spent + reserved + amount > quota` → `apperr.CodeInsufficientQuota`，**不修改任何状态** |
| LED-04 | `Reserve` | 事件队列长度 ≥ `capacity × admission_high_watermark` → `apperr.CodeOverloaded`（计费持久化跟不上时拒绝新请求，而不是丢事件） |
| LED-05 | `Reserve` | 成功时用**整体快照 CAS 循环**增加 `reserved`；检查与提交之间 quota/unlimited/spent/reserved 任意变化都必须重新检查。与 Settle/Release/Upsert 混合并发时不得超额准入。预留合计将超过 int64 时返回 `CodeOverloaded`；已消费累计溢出时饱和为 `MaxInt64`，原始费用仍完整进入事件 |
| LED-06 | `Settle` | `reserved -= r.Amount`；`spent += ev.CostNanoUSD`；把 `ev` 发送到事件队列。**允许**实际成本大于预留（透支），这是预期行为：上游已经计费，不能拒绝 |
| LED-07 | `Settle` | 事件队列满时**阻塞发送**（不丢弃）。理由：此时客户端响应已结束，阻塞只影响该请求 goroutine 的退出；LED-04 已在准入处限流 |
| LED-08 | `Settle` / `Release` | 同一个 `Reservation` 只能结束一次：第二次调用 `Settle` 返回 `ErrAlreadyFinished`，第二次 `Release` 什么也不做。用 `atomic.Bool` 实现 |
| LED-09 | `Release` | `reserved -= r.Amount`，不产生事件。只在尚未得到任何计费结果时调用。`Egress.Do` 同时返回非 nil Response 和 error 表示已产生费用但交付失败（CORE-12），此时必须结算，不能因 error 释放预留 |
| LED-10 | `UpsertAccount` | 更新 quota/unlimited；`spent` 与 `reserved` 保持不变；账户不存在则新建（`spent` 取参数值） |
| LED-11 | `Settle` | `ev.CostNanoUSD < 0` 视为编程错误，返回 `apperr.CodeInternal` 且不修改状态 |

### 3.3 什么时候 Settle，什么时候 Release（`core` 中实现，spec/13 §2.1）

| 情况 | 调用 | 结算用量 / `usage_source` |
|:--|:--|:--|
| `Egress.Do` 返回了 `*Response`，Meter 来源 `UsageUpstream` | `Settle` | 上游用量（可以为空 = 不计费）/ `upstream` |
| 同上，来源 `UsageEstimated`（如流中断，适配器按已转发文本估算了输出） | `Settle` | 适配器给出的单位 + 其余单位取预留用量 / `estimated` |
| 同上，来源 `UsageNone`（适配器拿不到任何用量） | `Settle` | 预留用量 / `estimated` |
| 所有尝试都在得到 `*Response` 之前失败 | `Release` | — |
| `Egress.Do` 同时返回 `*Response` 与 error（已产生费用但无法交付） | `Settle`，不重试，不转发部分结果 | 同上，按 Meter 来源结算；客户端取消也必须结算 |
| 鉴权、限流、服务校验等在 Reserve 之前失败 | 不需要（还没有预留） | — |

## 4. 计费事件刷盘（`ledger.Flusher`，任务 T06）

```go
type Flusher struct { /* store.Store, <-chan store.UsageEvent, batch size, interval, logger, Now */ }
func NewFlusher(st store.Store, events <-chan store.UsageEvent, cfg FlusherConfig, log *slog.Logger) *Flusher
func (f *Flusher) Run(ctx context.Context) error
```

| 编号 | 行为 |
|:--|:--|
| FLU-01 | 从 channel 收集事件，达到 256 条或 `flush_interval` 到期时调用 `store.InsertUsageEvents` |
| FLU-02 | 写入失败：**保留整批**，`Warn` 日志，指数退避重试（100ms 起，翻倍，上限 5s）；重试期间继续从 channel 接收新事件追加到待写缓冲（缓冲上限 = 队列容量 × 4，超过则停止从 channel 接收，靠 LED-04 背压） |
| FLU-03 | 连续失败超过 1 分钟：每分钟一条 `Error` 日志 |
| FLU-04 | `ctx` 取消后：继续把 channel 中剩余事件读空（非阻塞读）并尝试最终写入，使用独立的 5 秒超时 ctx；最终写入仍失败则把未写入事件以 JSON Lines 追加写到 `FlusherConfig.UnflushedPath`（`cmd/gateway` 传入 `data_dir/unflushed_usage.jsonl`），每行是 `json.Marshal(store.UsageEvent)` 的结果（无 json 标签，字段名即 Go 字段名），记录 `Error`，然后返回 nil |
| FLU-05 | 启动时若 `unflushed_usage.jsonl` 存在：先读取并写入数据库（幂等，因为 `request_id` 唯一），成功后删除该文件。此逻辑在 `ledger.RecoverUnflushed(ctx, st, path)` 中，由 `cmd/gateway` 在加载账户**之前**调用 |

## 5. 崩溃语义（写进用户文档，不要宣称更强保证）

- 进程被强制杀死时：事件 channel、Flusher 批次与重试缓冲中**全部尚未持久化的事件**都会丢失，这些请求重启后不会被计费。`flush_interval` 是正常运行时的刷盘触发间隔，不是丢失时间窗口的上界；数据库故障、慢写与重试可能令事件跨多个周期积压，积压时长没有保证。
- 在途请求的预留随进程消失，不计费。
- 重启后 `spent` 由 `SUM(usage_events.cost_nano_usd)` 重建，因此**不会**出现重复扣费。
