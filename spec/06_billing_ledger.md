# 06 计费与账本（Billing & Ledger）

代码位置：`internal/pricing`（已提供参考实现）、`internal/ledger`（接口已定义，实现为任务 T05/T06）。

## 1. 金额单位

- 所有金额是 `int64` 的 **nano-USD**：1 USD = 1,000,000,000。
- 配置中的价格是 `float64` 的「USD / 百万 token」，加载时转换为 `int64` 的「nano-USD / 百万 token」：`round(price * 1e9)`。
- **禁止**在成本计算中使用 `float64`。

## 2. 成本计算（`pricing`，已提供）

```go
type Price struct { InputPerMTok, OutputPerMTok, CacheReadPerMTok, CacheWritePerMTok int64 } // nano-USD / 百万 token
type Usage struct { InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64 }
func Cost(p Price, u Usage) int64
func EstimateReserve(p Price, bodyBytes int, maxOutputTokens int64) int64
```

| 编号 | 规则 |
|:--|:--|
| PRC-01 | `Cost = ceil((In*Pin + Out*Pout + CR*Pcr + CW*Pcw) / 1_000_000)`，各项先累加再除，结果向上取整 |
| PRC-02 | `Usage.InputTokens` 是**不含**缓存读写的普通输入 token（与 Anthropic 一致）。OpenAI 的 `prompt_tokens` 包含缓存命中部分，适配器必须先减去 `cached_tokens`（见 08 §5） |
| PRC-03 | `EstimateReserve`：输入 token 估算为 `ceil(bodyBytes / 3)`（偏保守），按**输入单价**计；输出按 `maxOutputTokens × 输出单价`；不考虑缓存折扣 |
| PRC-04 | 任一 token 数为负数时视为 0 |

`maxOutputTokens` 的取值（由 `proxy` 计算，LED-01）：请求体中 `max_tokens`（Anthropic）或 `max_completion_tokens` / `max_tokens`（OpenAI）存在且 > 0 时取它与模型 `max_output_tokens` 的较小值；否则取 `min(limits.default_max_output_tokens, 模型 max_output_tokens)`。

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

每个客户端一个账户：

```
quota      int64        // 额度上限（unlimited 时忽略）
spent      atomic.Int64 // 已结算成本（启动时 = SUM(usage_events.cost)）
reserved   atomic.Int64 // 在途预留合计
available  = quota - spent - reserved
```

### 3.2 行为规格

| 编号 | 方法 | 行为 |
|:--|:--|:--|
| LED-01 | — | 预留金额由 `proxy` 用 `pricing.EstimateReserve` 计算，`amount <= 0` 时按 1 处理（保证每个请求都有预留记录） |
| LED-02 | `Reserve` | 账户不存在 → `apperr.CodeUnauthenticated`（客户端刚被删除） |
| LED-03 | `Reserve` | 非 unlimited 且 `spent + reserved + amount > quota` → `apperr.CodeInsufficientQuota`，**不修改任何状态** |
| LED-04 | `Reserve` | 事件队列长度 ≥ `capacity × admission_high_watermark` → `apperr.CodeOverloaded`（计费持久化跟不上时拒绝新请求，而不是丢事件） |
| LED-05 | `Reserve` | 成功时用 **CAS 循环**增加 `reserved`（读 spent/reserved → 检查 → `CompareAndSwap`），并发 1000 个 Reserve 时不得超额 |
| LED-06 | `Settle` | `reserved -= r.Amount`；`spent += ev.CostNanoUSD`；把 `ev` 发送到事件队列。**允许**实际成本大于预留（透支），这是预期行为：上游已经计费，不能拒绝 |
| LED-07 | `Settle` | 事件队列满时**阻塞发送**（不丢弃）。理由：此时客户端响应已结束，阻塞只影响该请求 goroutine 的退出；LED-04 已在准入处限流 |
| LED-08 | `Settle` / `Release` | 同一个 `Reservation` 只能结束一次：第二次调用 `Settle` 返回 `ErrAlreadyFinished`，第二次 `Release` 什么也不做。用 `atomic.Bool` 实现 |
| LED-09 | `Release` | `reserved -= r.Amount`，不产生事件。**只在确定上游没有产生费用时调用**：上游请求从未发出、或上游在返回任何内容前返回了 4xx/5xx 错误 |
| LED-10 | `UpsertAccount` | 更新 quota/unlimited；`spent` 与 `reserved` 保持不变；账户不存在则新建（`spent` 取参数值） |
| LED-11 | `Settle` | `ev.CostNanoUSD < 0` 视为编程错误，返回 `apperr.CodeInternal` 且不修改状态 |

### 3.3 什么时候 Settle，什么时候 Release（`proxy` 中实现，LIFE-15）

| 情况 | 调用 |
|:--|:--|
| 上游 2xx 且正常结束，拿到上游 usage | `Settle`，`usage_source = "upstream"` |
| 上游 2xx 但流中途断开 / 客户端取消，已拿到部分 usage（如 Anthropic 的 `message_start`） | `Settle`，用已拿到的 usage；若输出 token 未知，按已转发的文本字节数 `ceil(bytes/4)` 估算，`usage_source = "estimated"` |
| 上游 2xx 但完全没有 usage 信息 | `Settle`，输入按 PRC-03 估算、输出按已转发字节估算，`usage_source = "estimated"` |
| 所有尝试都在收到 2xx 之前失败 | `Release` |
| 鉴权、限流、模型校验等在 Reserve 之前失败 | 不需要（还没有预留） |

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

- 进程被强制杀死时：内存中已结算但尚未刷盘的事件（最多 `flush_interval` 时长内的量）会丢失，这些请求不会被计费。
- 在途请求的预留随进程消失，不计费。
- 重启后 `spent` 由 `SUM(usage_events.cost_nano_usd)` 重建，因此**不会**出现重复扣费。
