# 07 调度器与容错（Scheduler & Resilience）

代码位置：`internal/scheduler`（类型与 `tryAcquire` 已提供；亲和缓存 T07、选择与状态机 T08）、`internal/protocol/classify.go`（错误分类，已提供）、`internal/core`（重试循环，T10）。

## 1. 核心类型

```go
type Key struct {
    ID          int64
    Pool        string
    Name        string
    Secret      []byte // 解密后的上游 Key 明文，只读
    Weight      int
    MaxInFlight int32

    inFlight      atomic.Int32
    persisted     atomic.Int32 // PersistedState：ready / quarantined / disabled
    cooldownUntil atomic.Int64 // UnixNano；0 = 无冷却
    authFailures  atomic.Int32 // 连续鉴权失败次数
    breaker       breaker      // 内部用 mutex 保护
}

type AcquireRequest struct {
    Pool        string
    AffinityKey string  // 空 = 无亲和
    Strict      bool    // 只能用 AffinityKey 记录的 Key（SCH-17）
    Exclude     []int64 // 本请求已经尝试失败的 Key
}

type Lease struct { Key *Key /* 私有字段 */ }
func (l *Lease) Finish(o protocol.Outcome) // 释放并上报，只生效一次

type Scheduler struct { /* ... */ }
func New(pools []PoolConfig, opts Options) *Scheduler
func (s *Scheduler) SetKeys(keys []*Key)
func (s *Scheduler) Acquire(req AcquireRequest) (*Lease, error)
func (s *Scheduler) KeyStatus() []KeyStatus // 管理 API 展示
```

`Options` 包含 `Now func() time.Time`、`Logger *slog.Logger`、`OnQuarantine func(keyID int64, reason string)`（控制面在回调中异步写库）。

## 2. Key 的可用性判定（`usable`）

Key 在时刻 `now` 可被选中，当且仅当以下**全部**成立：

| 编号 | 条件 |
|:--|:--|
| SCH-01 | `Pool == req.Pool`，且 ID 不在 `req.Exclude` 中 |
| SCH-02 | `persisted == ready` |
| SCH-03 | `cooldownUntil <= now` |
| SCH-04 | 熔断器允许放行：`closed`；或 `half_open` 且当前没有进行中的探测请求 |
| SCH-05 | `inFlight < MaxInFlight` |
| SCH-21 | 未过期：没有过期时间，或 `now < ExpiresAt`（T62；到期时刻本身即视为过期） |

## 3. 选择顺序（`Acquire`）

```
1. 取池中所有满足 SCH-01..05 的 Key 作为候选集 C。C 为空 → 返回 ErrNoKey（§3.1）。
2. 若 AffinityKey 非空，且亲和缓存中绑定的 Key ∈ C，且 tryAcquire 成功 → 选中它（SCH-10）。
3. 否则按池策略对 C 排序，依次 tryAcquire，第一个成功的即选中（SCH-11/12）。
4. 全部 tryAcquire 失败（并发竞争）→ ErrNoKey。
5. 选中后：若 AffinityKey 非空，写入/刷新亲和缓存（SCH-13）；若该 Key 处于 half_open，标记探测进行中。
```

| 编号 | 规则 |
|:--|:--|
| SCH-10 | 亲和优先：绑定的 Key 可用时**必须**选它，不允许为了「负载更均衡」而切走（保护 Prompt Cache） |
| SCH-11 | `least_inflight`：按 `inFlight / MaxInFlight` 升序；相同时按 `Weight` 降序；仍相同时按轮转游标起点决定（避免总选 ID 最小的） |
| SCH-12 | `weighted_round_robin`：平滑加权轮询（Nginx SWRR），在池级 mutex 内执行；只在候选集 C 上运算 |
| SCH-13 | 亲和缓存 TTL = 池的 `affinity_ttl`，每次命中刷新。会话空闲超过 TTL 后条目过期，下次请求自然重新选择——这就是「在缓存冷点再平衡」，不需要额外逻辑 |
| SCH-14 | `tryAcquire` 必须是 CAS 循环（已提供），保证 `inFlight` 永远不超过 `MaxInFlight` |
| SCH-15 | `Acquire` 全程不做 I/O，不调用回调；耗时应在微秒级 |
| SCH-17 | `Strict == true`：亲和缓存未命中（或已过期）→ `CodeNotFound`（消息 `unknown or expired task; resubmit`）；命中但该 Key 不在候选集 C 中或 `tryAcquire` 失败 → `CodeUpstreamUnavailable`（带 §3.1 的 Retry-After）。**绝不**回落到其他 Key。严格模式不刷新 TTL |
| SCH-18 | `Lease.Pin(key, ttl)`：把 `key` 绑定到本 Lease 的 Key，TTL 为 `ttl`（池的 `pin_ttl`）；与普通亲和共用同一个缓存。缓存满时 LRU 淘汰可能淘汰固定条目——这是已知限制，写进部署文档（固定条目数 ≈ 并发未完成任务数，远小于默认容量 65536） |
| SCH-19 | `PinnedPool(key)`：亲和缓存命中且未过期、且该 Key 仍存在时返回其池名；不刷新 TTL。供内核为不带服务名的严格亲和请求确定路由（CORE-11） |
| SCH-20 | `SetPools(pools)`：新池加入；已有池更新参数，Key 及其运行时状态保留（熔断参数按新配置重新设置），已发出的租约按发出时的池参数结束；不在 `pools` 中的池停止调度，`Acquire` 视为不存在，其中的 Key 从调度器移除（`KeyStatus` 不再包含）。并发安全，与 `Acquire`、`Lease.Finish`、`SetKeys` 可以同时调用；`Acquire` 不加全局锁。 |
| SCH-21 | 过期时间（T62）：`Key.SetExpiresAt(t)` / `Key.ExpiresAt()`，零值表示不过期，由 `LoadKeys` 从 `UpstreamKey.ExpiresAt` 设置。过期是计算出来的状态，不写回数据库，延长过期时间后立即可用。过期时间不属于 SCH-16 的身份字段：`SetKeys` 复用旧对象时同时更新过期时间，冷却、熔断与 `inFlight` 保留。过期的 Key 也不服务严格亲和（SCH-17，返回 `CodeUpstreamUnavailable`）。§3.1 的错误消息在有过期 Key 时附加 `(N expired)`（只有数量），过期 Key 不参与 `Retry-After` 计算。`KeyStatus` 增加 `ExpiresAt` 与 `Expired` |
| SCH-16 | `SetKeys` 对同 ID 的 Key：若 `Pool/Name/Secret/Weight/MaxInFlight` 全部不变，**继续使用旧的 `*Key` 对象**，只更新持久化状态；若有任一改变，使用新对象，并从旧对象复制 `cooldownUntil` 与 `authFailures`，熔断器重新从 closed 开始，`inFlight` 从 0 开始（旧租约在旧对象上结束，过渡期内并发可能短暂超过上限，这是可接受的）。导出字段在对象被放入池后永不修改，因此读取它们不需要加锁 |

### 3.1 无可用 Key

返回 `*apperr.Error{Code: CodeUpstreamUnavailable}`，`RetryAfter` = 池中所有 `persisted==ready` 的 Key 里最早的 `cooldownUntil - now`（向上取整到秒，至少 1 秒；若没有冷却中的 Key 则为 0）。

## 4. 亲和缓存（`affinity.go`，任务 T07）

```go
type affinityCache struct { /* 16 个分片，每片 mutex + map + 双向链表 */ }
func newAffinityCache(capacity int) *affinityCache
func (c *affinityCache) Get(key string, now time.Time) (keyID int64, ok bool)
func (c *affinityCache) Put(key string, keyID int64, ttl time.Duration, now time.Time)
func (c *affinityCache) Delete(key string)
func (c *affinityCache) Len() int
```

| 编号 | 行为 |
|:--|:--|
| AFF-01 | 分片：`fnv32a(key) % 16` |
| AFF-02 | 每片容量 `ceil(capacity/16)`；超过时淘汰该片中**最久未使用**的条目（LRU，不是 FIFO） |
| AFF-03 | `Get` 遇到已过期条目：删除并返回 `ok=false` |
| AFF-04 | `Get` 命中：移动到最近使用位置，**并把过期时间延长为 `now + 原 ttl`** |
| AFF-05 | `Put` 已存在的 key：覆盖 keyID 与过期时间，移动到最近使用位置 |
| AFF-06 | 默认容量 65536（`Options.AffinityCapacity`，0 时取默认） |

## 5. 结果上报与状态机（`Lease.Finish`，任务 T08）

`Finish(o)` 先把 `inFlight` 减 1（只减一次），再按 `o.Kind` 更新状态：

| 编号 | Outcome | 对 Key 的影响 | 本请求是否换 Key 重试 |
|:--|:--|:--|:--|
| ST-01 | `Success` | 熔断器记一次成功；`authFailures = 0`；half_open → closed | — |
| ST-02 | `ClientError`（400/404/413/422 等） | 无 | 否（原样返回客户端） |
| ST-03 | `Canceled`（客户端断开） | 无 | 否 |
| ST-04 | `LocalError`（本机端口耗尽等） | **无**（不是 Key 的错） | 否，返回 503 `overloaded` |
| ST-05 | `RateLimited`（429） | `cooldownUntil = now + clamp(RetryAfter 或 cooldown.default, 1s, cooldown.max)` | 是 |
| ST-06 | `AuthFailed`（401） | `authFailures++`；若 `== 1`：冷却 60 秒；若 `>= 2`：`persisted = quarantined`，调用 `OnQuarantine(id, "upstream 401 twice")` | 是 |
| ST-07 | `PermissionDenied`（403） | 冷却 `cooldown.default` | 是 |
| ST-08 | `Overloaded`（529、503） | 无（厂商全局过载，不惩罚单个 Key） | 是 |
| ST-09 | `ServerError`（500/502/504）、`Timeout`、`NetworkError` | 熔断器记一次失败 | 是 |

`Finish` 第二次及以后的调用什么也不做（`atomic.Bool`）。

### 5.1 熔断器（`breaker.go`，任务 T08）

- 滑动窗口 `breaker.window`，分 10 个桶，每桶记 `total`、`failures`。
- **closed → open**：窗口内 `total >= min_requests` 且 `failures/total >= failure_ratio`。打开时长 `d = open_duration`。
- **open → half_open**：`now >= openedAt + d` 时，在下一次可用性检查中转换（惰性转换，不需要定时器）。
- **half_open**：同一时间只允许 1 个探测请求（SCH-04）。探测成功 → closed，清空窗口，`d` 重置为 `open_duration`；探测失败 → open，`d = min(d*2, max_open_duration)`。
- 只有 ST-09 计为失败，只有 ST-01 计为成功；其他 Outcome 不进入窗口统计。

## 6. 重试循环（`core`，任务 T10）

```
exclude := []
for attempt := 1; attempt <= pool.MaxAttempts; attempt++ {
    lease, err := sched.Acquire({Pool, AffinityKey, Strict, Exclude: exclude})
    if err != nil { break }                       // 无可用 Key
    resp, err := route.Egress.Do(ctx, upstream(lease.Key), call)   // 一次尝试，可含多次上游调用
    if err == nil { 开始转发；转发结束后 lease.Finish(最终 outcome)；return }
    outcome := err 是 *AttemptError ? 修正后的 Outcome（CORE-10）: LocalError（CORE-09）
    lease.Finish(outcome)
    if !outcome.Kind.Retryable() || call.BodyStream != nil || Strict { 把错误返回客户端；return }
    exclude = append(exclude, lease.Key.ID)
}
返回最后一次失败对应的错误（§6.1）
```

| 编号 | 规则 |
|:--|:--|
| RTY-01 | 只有在**尚未向客户端写出任何字节**时才允许重试。读到上游 2xx 响应头即进入转发阶段，之后任何错误都不重试 |
| RTY-02 | 每次重试前必须重新执行 `Acquire`，并排除已失败的 Key |
| RTY-03 | 重试不等待（不 sleep）；换 Key 本身就是退避 |
| RTY-04 | 上游失败响应体在重试前必须读完（上限 8 KiB）并关闭，以便连接复用（`protocol.Send` 已实现） |
| RTY-06 | 流式请求体（`call.BodyStream != nil`）与严格亲和（`Strict`）的请求只尝试一次（CORE-07、SCH-17） |
| RTY-05 | 客户端 `ctx` 已取消时立即停止循环，`Outcome = Canceled` |

### 6.1 全部失败时返回给客户端的错误

| 最后一次失败 | 返回 |
|:--|:--|
| `Acquire` 返回 ErrNoKey | 503 `upstream_unavailable`，带 `Retry-After`（§3.1） |
| `RateLimited` | 429 `rate_limited`，`Retry-After` 取上游值 |
| `ClientError` | 同协议透传：**原样转发** `AttemptError` 中的上游状态码、`Content-Type` 与错误体（截断 8 KiB），这是客户端请求本身的问题。转译路径：400 `invalid_request`（`upstream rejected the request`），不转发上游原文（格式属于另一个协议） |
| `LocalError` | 502 `upstream_error`（如上游响应过大）；`Do` 返回 `*apperr.Error` 时原样返回（CORE-09） |
| 严格亲和未命中 | 404 `not_found`（SCH-17） |
| `Overloaded` | 503 `upstream_unavailable` |
| `Timeout` | 504 `timeout` |
| 其他 | 502 `upstream_error` |

## 7. 亲和键计算（`core.AffinityKey`，任务 T10）

职责划分：**入站适配器**从请求中提取原始线索（spec/08 §2.4，`Envelope.Affinity`）；**内核**决定优先级并做哈希。内核不读取请求体字段。

```go
func AffinityKey(clientID int64, h http.Header, hint protocol.AffinityHint) string
```

| 优先级 | 来源 | 取值 |
|:-:|:--|:--|
| 0 | `hint.Strict == true` | 只用 `hint`（来源 `j`），忽略请求头——任务固定不能被客户端头覆盖 |
| 1 | `h` | 请求头 `X-UAIG-Session` 非空且 ≤ 128 字节（所有协议通用；超长视为不存在）；`h` 为 nil 时跳过 |
| 2 | `hint.Source`（`u`、`p` 或 `j`） | `hint.Value` 非空 |

返回值格式 `c<clientID>:<来源>:<hex(sha256(取值))[:16]>`；都没有时返回空字符串（无亲和）。

### 7.1 任务固定（异步 API）

异步 API 的任务只存在于创建它的上游账号下，查询必须使用同一个 Key：

1. 创建任务的响应中，出站适配器把任务 ID 放进 `Response.Pins`（spec/08 EG-06）。
2. 内核在转发成功后，对每个 pin 调用 `lease.Pin(AffinityKey(clientID, nil, AffinityHint{Source: AffinityPin, Value: pin}), pool.pin_ttl)`（CORE-08）。
3. 查询任务的请求，入站适配器给出 `AffinityHint{Source: AffinityPin, Value: 任务 ID, Strict: true}`（spec/08 IN-G07）；内核以 `Strict` 调用 `Acquire`（SCH-17），该请求不换 Key 重试。
4. 亲和键包含 clientID，因此客户端 A 创建的任务，客户端 B 无法查询（返回 404）。
5. 固定记录只在内存中：网关重启或条目过期后查询返回 404，客户端需要重新提交。

### 7.2 运行时状态的持久化（任务卡 T68）

冷却截止时间、连续鉴权失败次数与任务固定记录跨重启保留（含 T55 的进程内重启）。隔离状态本来就写库（ST-06）；熔断器状态与普通会话亲和不保留。

| 编号 | 规则 |
|:--|:--|
| RT-01 | 亲和缓存的条目带 `pinned` 标记：`Lease.Pin` 写入的为 true，普通会话亲和为 false；普通亲和覆盖同一条目时不取消标记 |
| RT-02 | `Scheduler.RuntimeSnapshot() RuntimeState`：冷却截止时间晚于现在或失败计数 > 0 的 Key，以及未过期的 `pinned` 条目；按 Key ID、亲和键升序。只读内存，逐个分片短暂加锁，不阻塞 `Acquire` |
| RT-03 | `Scheduler.RestoreRuntime(RuntimeState) (cooling, pins int)`：只恢复当前存在的 Key；冷却截止时间已过的忽略，未过的不晚于「现在 + 池当前的 `cooldown.max`」，只延长不缩短；失败计数取较大值；固定记录按剩余有效期写回并标记为固定 |
| RT-04 | 快照与恢复都不访问数据库；读写数据库由装配代码负责 |
| RT-10 | 启动时在 `SetKeys` 之后、开始服务之前读取并恢复，记一条 Info（只有数量）；读取失败记 Warn 并照常启动 |
| RT-11 | 后台 worker `runtime_snapshot` 每 5 秒取一次快照，与上次成功写入的内容不同才写库；写库失败记 Warn，下次重试 |
| RT-12 | 关闭时在数据面排空之后（与计费刷盘同一阶段）总是写最后一次快照 |
| RT-13 | 进程崩溃时最多丢失最近 5 秒的变化 |
| RT-14 | 快照与相关日志中不出现任务 ID、Key 明文或客户端 Token；亲和键本身是哈希 |
