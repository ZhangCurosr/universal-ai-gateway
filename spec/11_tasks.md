# 11 任务卡（Task Cards）

> 一次只做一张卡。开始前读 `AGENTS.md`；完成后把「状态」改为 `DONE`。
> 状态取值：`TODO`（未开始）→ `DOING`（进行中，开工时改）→ `DONE`（满足 AGENTS.md §4 全部条件）。

## 依赖图与并行建议

```
M1：
  T01 ──▶ T02 ──┬──▶ T06（还需 T05）
                ├──▶ T08（还需 T07）
                └──▶ T13
  T01 ──▶ T03
  T04、T05、T07、T09A、T09B、T09C 无前置，可并行
  T10 需要 T05、T08、T09A、T09B
  T11 需要 T03、T04、T09A、T09B（只依赖入站适配器的 WriteCatalog，不依赖 T10）
  T12 需要 T02、T03、T05、T08
  T32 需要 T10、T09C；T33 需要 T32
  T14 需要以上全部
  T15 需要 T14
扩展适配器：T30、T31 无前置（只依赖已冻结的契约与一致性套件），可随时并行
M2：T20 → T21 → T22，T23 → T24（全部需要 T14）
```

可以同时开工的第一批：**T01、T04、T05、T07、T09A、T09B、T09C**（互不修改同一文件）。

## 通用验收（每张卡都要满足，不再重复）

- `go run ./tools/check` 全绿。
- 卡片「测试」一栏中点名的测试函数已删除 `t.Skip("TODO(Txx)")` 并通过。
- 卡片「涉及文件」之外没有改动（`spec/11_tasks.md` 的状态字段除外）。

---

## T00 代码骨架与规则 — 状态：`DONE`

由架构负责人完成：目录结构、接口签名、带 `t.Skip("TODO(Txx)")` 的契约测试，以及以下**参考实现**（可以照着它们的风格写代码）：

| 包 | 已实现的内容 |
|:--|:--|
| `apperr` | 错误码、`New/Wrap/As/CodeOf`、三种错误响应格式 `WriteHTTP` |
| `config` | 结构、默认值、`Parse/Load/Validate`（CFG-01 ~ CFG-09） |
| `obs` | `NewLogger`、`MaskSecret`、请求 ID 生成与 context 存取 |
| `crypto` | 主密钥文件、HKDF、信封加密、AAD（SEC-01 ~ SEC-07） |
| `jsonx` | 顶层字段改写与路径读取 |
| `sse` | SSE `Reader` / `Writer` |
| `pricing` | 价格换算、成本、预留估算 |
| `store` / `store/sqlite` | 接口与记录类型；`Open`、迁移、`AcquireDirLock`、`isUniqueViolation` |
| `auth` | Token 生成/哈希、`ExtractCredential`、`Index`、Principal 的 context 存取 |
| `protocol` | **冻结契约**：`Ingress` / `Egress` / `Translator` 接口、`Envelope`、`Registry`（含 `Seal` 校验）、`Outcome`、`ClassifyHTTP` / `ClassifyNetError` / `ParseRetryAfter` |
| `adapter/adaptertest` | 适配器一致性测试套件（CONF-I01 ~ I07、CONF-E01 ~ E06） |
| `adapter/anthropic`、`adapter/openai` | `Protocol` 常量、`Register`、入站 `Routes` / `Claims` / `WriteError`、`ErrorType` |
| `core` | `New`（CORE-01 校验）、`Models`、`Route` |
| `scheduler` | `Key`、`NewKey`、`tryAcquire`、`PoolsFromConfig` |
| `clientip` | 可信代理解析真实客户端 IP（CIP-01 ~ CIP-05） |
| `httpapi` | 由注册表生成路由与冲突分派（RTE-01 ~ RTE-04）、`/healthz`、`RequestID`、`ClientIP`、`Recover`、`trackingWriter`、`chain`；`OverloadGuard` 结构 |
| `cmd/gateway` | `adapters.go`：唯一装配适配器的地方 |
| `cmd/gateway` | `version`、`check-config` 子命令 |
| `archtest`、`tools/check` | 分层检查与一键检查 |

查看还剩哪些待办：`git grep -n "TODO(T"`。

---

## T01 SQLite 仓储：客户端与设置 — 状态：`DONE`

- **前置**：无
- **必读规格**：04 §1–§4（STO-01 ~ STO-05、STO-15）、02 §4 §10
- **涉及文件**：`internal/store/sqlite/clients.go`、`internal/store/sqlite/settings.go`、`internal/store/sqlite/clients_test.go`
- **要做的事**：
  1. 实现 `CreateClient`、`GetClient`、`ListClients`、`UpdateClient`、`CountAdmins`、`GetSetting`、`PutSetting`。
  2. `allowed_models` 用 `encoding/json` 编解码；读到空字符串或 `null` 时解为空切片。
  3. 时间：写入 `db.now().UnixMilli()`，读出 `time.UnixMilli(v).UTC()`。
  4. 唯一约束冲突用已有的 `isUniqueViolation(err)` 转换为 `store.ErrConflict`。
- **测试**：`TestClientsCRUD`、`TestClientConflict`、`TestCountAdmins`、`TestSettings`（文件中已写好，去掉 Skip）；另补充：`UpdateClient` 不存在的 id 返回 `ErrNotFound`。
- **陷阱**：`database/sql` 扫描 `INTEGER` 布尔值要扫到 `int64` 再转 `bool`；不要用 `SELECT *`，显式列出列名（列顺序以后可能变化）。

## T02 SQLite 仓储：上游 Key、计费事件、审计 — 状态：`DONE`

- **前置**：T01
- **必读规格**：04 §3–§4（STO-06 ~ STO-15）、05 §2.3
- **涉及文件**：`internal/store/sqlite/keys.go`、`internal/store/sqlite/usage.go`、`internal/store/sqlite/audit.go`、`internal/store/sqlite/keys_test.go`、`internal/store/sqlite/usage_test.go`
- **要做的事**：
  1. `InsertUpstreamKey` 严格按 STO-06 的四步在**一个事务**中完成；`seal` 返回错误时回滚并返回该错误。
  2. `InsertUsageEvents` 使用 `INSERT OR IGNORE`；`InsertAuditRecords` 普通 `INSERT`；两者都复用一条 prepared statement。
  3. `QueryUsage` 用一条 `GROUP BY client_id, model` 的 SQL 完成，返回按 `client_id, model` 排序。
- **测试**：`TestInsertUpstreamKeySealUsesID`、`TestInsertUpstreamKeySealErrorRollsBack`、`TestUpstreamKeyUpdates`（`keys_test.go`）；`TestUsageEventsIdempotent`、`TestSumCostByClient`、`TestQueryUsage`、`TestAuditInsertAndDelete`（`usage_test.go`）。
- **陷阱**：`seal` 必须拿到**真实的自增 id**（`res.LastInsertId()`），这是 AAD 绑定行主键的前提。

## T03 客户端索引重载 — 状态：`DONE`

- **前置**：T01
- **必读规格**：05 §3.3–§3.4
- **涉及文件**：`internal/auth/reloader.go`、`internal/auth/reloader_test.go`
- **要做的事**：实现 `Reloader.Reload(ctx)`：`ListClients` → `Index.Replace`。错误用 `fmt.Errorf("reload clients: %w", err)` 包装。
- **测试**：`TestReloaderReplacesIndex`（已写好，使用只实现 `ListClients` 的 `fakeLister`）；另补充一个 `ListClients` 返回错误时 `Reload` 返回包装错误、且索引保持旧内容的用例。

## T04 每客户端 RPM 限流 — 状态：`DONE`

- **前置**：无
- **必读规格**：09 §3（RL-01 ~ RL-06）。注意 `Allow` 的键是字符串（`"c:<clientID>"` 或 `"ip:<addr>"`），同一实现服务两种用途
- **涉及文件**：`internal/ratelimit/ratelimit.go`、`internal/ratelimit/ratelimit_test.go`
- **测试**：`TestUnlimited`、`TestBurstThenReject`、`TestRefill`、`TestRPMChange`、`TestSweep`、`TestConcurrentAllow`（100 个 goroutine 共调用 1000 次、rpm=600 时，放行次数必须恰好 600）。
- **陷阱**：用注入的 `now`，不要在实现里调用 `time.Now()`。

## T05 内存账本 — 状态：`TODO`

- **前置**：无
- **必读规格**：06 §1–§3（LED-01 ~ LED-11）
- **涉及文件**：`internal/ledger/memory.go`、`internal/ledger/memory_test.go`
- **要做的事**：实现 `Memory` 的全部方法。账户表用 `sync.RWMutex` 保护的 `map[int64]*account`；账户内的 `spent`/`reserved` 用 `atomic.Int64`；`Reserve` 用 CAS 循环。
- **测试**：`TestReserveWithinQuota`、`TestReserveExceedsQuota`、`TestReserveUnknownClient`、`TestUnlimited`、`TestSettleOverdraft`、`TestSettleTwice`、`TestSettleNegativeCost`、`TestReleaseRestores`、`TestAdmissionBackpressure`、`TestConcurrentReserveNeverExceeds`（1000 个 goroutine 每个预留 1，额度 500，成功次数必须恰好 500）、`TestUpsertKeepsSpent`。
- **陷阱**：LED-03 失败时不能留下任何修改；`Settle` 的事件发送是阻塞的——测试中记得从 `Events()` 读取，否则测试会卡死。

## T06 计费事件刷盘 — 状态：`TODO`

- **前置**：T02、T05
- **必读规格**：06 §4（FLU-01 ~ FLU-05）、02 §5 §6
- **涉及文件**：`internal/ledger/flusher.go`、`internal/ledger/flusher_test.go`
- **测试**：`TestFlushByBatchSize`、`TestFlushByInterval`、`TestRetryKeepsBatch`（假 Store 前两次返回错误，第三次成功，最终数据库中事件不重不漏）、`TestShutdownDrains`、`TestShutdownWritesUnflushedFile`、`TestRecoverUnflushed`。
- **陷阱**：`Run` 退出前的最终写入必须用新的 `context.WithTimeout(context.Background(), 5*time.Second)`。

## T07 调度器亲和缓存 — 状态：`TODO`

- **前置**：无
- **必读规格**：07 §4（AFF-01 ~ AFF-06）
- **涉及文件**：`internal/scheduler/affinity.go`、`internal/scheduler/affinity_test.go`
- **要做的事**：16 分片，每片 `sync.Mutex` + `map[string]*list.Element` + `container/list`。
- **测试**：`TestAffinityGetPut`、`TestAffinityExpire`、`TestAffinityTouchExtends`、`TestAffinityLRUEviction`（容量 16 时每片 1 个，向同一分片写两个 key，先写的被淘汰）、`TestAffinityConcurrent`。

## T08 调度器：选择、状态机、熔断、加载 — 状态：`TODO`

- **前置**：T02、T07
- **必读规格**：07 §1–§5（SCH-01 ~ SCH-15、ST-01 ~ ST-09、§5.1）
- **涉及文件**：`internal/scheduler/scheduler.go`、`internal/scheduler/breaker.go`、`internal/scheduler/load.go`、`internal/scheduler/scheduler_test.go`、`internal/scheduler/breaker_test.go`（新建，熔断器单元测试）、`internal/scheduler/load_test.go`（新建）
- **要做的事**：
  1. `breaker.go`：按 §5.1 实现，所有方法接收 `now time.Time` 参数（不自己取时间）。窗口分桶：桶宽 = `window/10`，桶起点 = `now.Truncate(桶宽)`，桶起点早于 `now - window` 的桶视为空。
  2. `scheduler.go`：`New`、`SetKeys`（规则见 SCH-16）、`Acquire`（含 `Strict`，SCH-17）、`Lease.Finish`、`Lease.Pin`（SCH-18）、`PinnedPool`（SCH-19）、`KeyStatus`。
  3. `load.go`：`LoadKeys(ctx, st, kr, log)`：`ListUpstreamKeys` → `kr.Open(k.Sealed, crypto.UpstreamKeyAAD(k.ID, k.Pool, k.Sealed.KEKVersion))` → `NewKey(..., ParsePersistedState(k.State))`。单个 Key 解密失败：记录 `Error` 日志（不含密文与明文）并跳过该 Key（不要让整个网关起不来）。
- **测试**：`scheduler_test.go` 中已写好的全部用例（`TestRateLimitedCooldown`、`TestAcquireSkipsCooldown`、`TestAcquireRespectsMaxInFlight`、`TestAffinityPreferred`、`TestAffinityFallbackWhenBusy`、`TestExcludeTried`、`TestLeastInflightOrder`、`TestWeightedRoundRobinDistribution`、`TestFinishOnce`、`TestAuthFailedTwiceQuarantines`、`TestOverloadedNoPenalty`、`TestNoKeyRetryAfter`、`TestBreakerOpensAndHalfOpen`、`TestSetKeysPreservesRuntime`、`TestSetKeysChangedFieldsKeepCooldown`、`TestStrictPin`）；`breaker_test.go` 覆盖：窗口过期后统计清零、`MinRequests` 未达到时不打开、探测失败后打开时长翻倍且不超过上限；`load_test.go` 覆盖：解密失败的 Key 被跳过。
- **陷阱**：`OnQuarantine` 回调**不能**在持锁时调用；用 `go` 调用或在释放锁后调用。

## T09A Anthropic 适配器（入站 + 出站） — 状态：`TODO`

- **前置**：无
- **必读规格**：08 全章（重点 §2、§3、§5）；13 §1
- **涉及文件**：`internal/adapter/anthropic/ingress.go`、`internal/adapter/anthropic/egress.go`、`internal/adapter/anthropic/ingress_test.go`（新建）、`internal/adapter/anthropic/egress_test.go`、`internal/adapter/anthropic/conformance_test.go`
- **要做的事**：
  1. 入站：`Parse`（IN-G01 ~ IN-G06、IN-L01 ~ IN-L04、AFH-01 ~ AFH-04）、`StreamError`（§2.6）、`WriteCatalog`（§2.8）。`Routes`、`Claims`、`WriteError` 已提供。
  2. 出站：`prepareBody`（PT-01、PT-02、PT-04）、`newRequest`（HDR-01 ~ HDR-03、HDR-05）、`meter.observeEvent` / `observeBody`（USE-01、USE-02、USE-07 的文本字节计数）、`classifyStreamError`（STR-05）。`Prepare`、`Do`、`meter.Usage`、`meter.Fault` 已提供，不要改。
- **测试**：`conformance_test.go` 的 `TestIngressConformance`、`TestEgressConformance`（一致性套件，必须全部通过）；`egress_test.go` 中已写好的用例（含 `TestMeterEstimatesOutputWhenStreamCut`）；`ingress_test.go` 新增：亲和线索（有 `metadata.user_id` → `u`；没有 → `p`，且追加后续消息不改变 `Value`；都没有 → 零值）、`max_tokens` 为负数或字符串 → `invalid_request`、`WriteCatalog` 空列表时 `first_id` 为 `null`。

## T09B OpenAI 兼容适配器（入站 + 出站） — 状态：`TODO`

- **前置**：无
- **必读规格**：08 全章（重点 §2、§3、§5）；13 §1
- **涉及文件**：`internal/adapter/openai/ingress.go`、`internal/adapter/openai/egress.go`、`internal/adapter/openai/ingress_test.go`（新建）、`internal/adapter/openai/egress_test.go`、`internal/adapter/openai/conformance_test.go`
- **要做的事**：
  1. 入站：`Parse`（IN-G01 ~ IN-G06、IN-L01 ~ IN-L04；`max_completion_tokens` 优先于 `max_tokens`；亲和线索 `prompt_cache_key` > `user` > 前缀）、`StreamError`、`WriteCatalog`。
  2. 出站：`prepareBody`（PT-01 ~ PT-03）、`newRequest`（HDR-01、HDR-02、HDR-04、HDR-05）、`meter.observeEvent` / `observeBody`（USE-03 ~ USE-07）、`shouldForward`（FWD-02、FWD-03）。`Prepare`、`Do`、`meter.Usage` 已提供，不要改。
- **测试**：两个一致性测试；`egress_test.go` 中已写好的 `TestPrepareBodyInjectsUsage`、`TestPrepareBodyNonStreamNoInject`、`TestNewRequestHeaders`、`TestUsageCachedTokens`、`TestUsageStream`、`TestMeterEstimatesOutputWithoutUsage`、`TestShouldForwardFiltersInjectedUsageChunk`；`ingress_test.go` 新增亲和线索三种来源的用例。

## T09C OpenAI Responses 适配器（入站 + 出站 + 工具） — 状态：`DONE`

- **前置**：无
- **必读规格**：08 §1 ~ §3、§5、**§9**；07 §7.1；14 §1
- **涉及文件**：`internal/adapter/openairesp/ingress.go`、`egress.go`、`tools.go`、`ingress_test.go`（新建）、`egress_test.go`、`conformance_test.go`
- **要做的事**：
  1. 入站：`Parse`（IN-R02 ~ IN-R05）、`StreamError`（IN-R07）。`Routes`、`Claims`、`WriteError` 已提供。
  2. 出站：`prepareBody`（PT-R01 ~ PT-R03）、`newRequest`（HDR-R01 ~ HDR-R03）、`meter.observeEvent` / `observeBody`（USE-R01 ~ USE-R03、STR-R01）。`Prepare`、`Do`、`upstreamEndpoint`、`meter.Usage` 已提供，不要改。
  3. 工具：`ToolRequest`、`ToolResult`（TL-R01、TL-R02）。
- **测试**：两个一致性测试；`egress_test.go` 中的 `TestPrepareBodyStoreFlag`、`TestNewRequestHeaders`、`TestMeterFailedAndCutStreams`、`TestAskModelTool`；`ingress_test.go` 新增：亲和线索四种来源（`previous_response_id` 严格、`prompt_cache_key`、`user`、前缀）、`input` 为数组时前缀只取第一个元素、`StreamError` 的格式。

## T10 内核（core） — 状态：`TODO`

- **前置**：T05、T08、T09A、T09B
- **必读规格**：**13 全章**；08 §1、§3 开头的「Do 的返回约定」；01 §6；06 §2–§3；07 §6–§7
- **涉及文件**：`internal/core/core.go`、`internal/core/forward.go`（新建）、`internal/core/affinity.go`、`internal/core/core_test.go`、`internal/core/affinity_test.go`
- **要做的事**：
  1. `affinity.go`：按 07 §7 实现 `AffinityKey`（含 `Strict` 时忽略请求头）。
  2. `core.go`：`Handler(in, rt)` 按 13 §2 编排，同时支持透传与转译两条路径（13 §3）、三种请求体模式、任务固定（CORE-08）；收尾动作全部放在一个 `defer` 中。`bodyLimit`、`reserveUsage`、`settledUsage`、`headerTimeout` 已提供，直接使用。
  3. `forward.go`：13 §4（三种响应形态、流中异常）与 13 §5 超时。
  4. **K-01 / K-02**：内核代码中不得出现协议名、请求体字段名、`switch` 协议，也不得出现 token 专用逻辑。需要协议相关的行为时，调用 `in`（入站）、`route.Egress`（出站）或转换器的方法。
- **测试**：`core_test.go` 已提供测试环境 `newTestEnv`（假上游 + 真实 ledger/scheduler/适配器 + 测试专用的非大模型协议 `jobs`）与以下用例：`TestPassthroughNonStream`、`TestPassthroughStreamAnthropic`（含结算金额断言）、`TestRetryOn429ThenSuccess`、`TestNoRetryAfterFirstByte`、`TestClientErrorPassedThrough`、`TestProtocolMismatchRejected`、`TestTranslatorPathUsed`、`TestJobsSubmitStreamsBodyPinsAndBillsPages`、`TestJobsStreamBodyNotRetried`、`TestJobsStrictPinRoutesToSameKey`、`TestJobsRawDownload`；`TestNewRejectsUnknownProtocol`、`TestNewRejectsIncompletePrice`、`TestModelsFiltered`、`TestBodyLimit`、`TestSettledUsage`、`TestHeaderTimeout` 已可运行。`affinity_test.go` 提供 `TestAffinityKeySources`。
  你还必须**新增**：`TestPassthroughStreamOpenAIInjectedUsageFiltered`、`TestReleaseWhenAllAttemptsFail`（所有 Key 都 500 → 客户端 502，reserved 归零且无计费事件）、`TestModelNotAllowed`（403，上游未被调用）、`TestUnknownModel`（400）、`TestClientCancelStopsUpstream`、`TestInsufficientQuota`（402）、`TestTranslatorStreamPath`（假转换器把每个上游事件变成两个事件、结束时追加一个收尾事件，客户端收到双倍事件与收尾事件）、`TestRequestTooLarge`（413）、`TestStreamBodyTooLarge`（`jobs` 上传超过 `max_stream_body_bytes` → 413）、`TestEventsFirstEventErrorRetried`（`Next` 在第一个事件前返回 `*AttemptError` 时换 Key 重试，13 §4.2）、`TestRawStreamIdleTimeout`（字节流两块之间超过 `stream_idle` → 连接中止，Outcome = Timeout）、`TestDoLocalErrorNotRetried`（CORE-09）、`TestResolveRouteFromPin`（`jobs` 提交后，不带服务名的严格亲和请求路由到 `parser`，CORE-11）、`TestPinsAppendedDuringStream`（流式适配器在转发中追加的 Pins 被记录，CORE-08）。
- **陷阱**：判断「是否已写出首字节」用包装的 `http.ResponseWriter`（13 §4.5）；不要只看局部变量。流式请求体只尝试一次（CORE-07）。

## T11 数据面路由与中间件 — 状态：`TODO`

- **前置**：T03、T04、T10
- **必读规格**：09 §1–§2、§5；03 §7
- **涉及文件**：`internal/httpapi/middleware.go`、`internal/httpapi/router_test.go`、`internal/httpapi/middleware_test.go`
- **要做的事**：
  1. 实现 `Authenticate`、`RateLimit`、`AccessLog`、`IPRateLimit`、`OverloadGuard.Middleware`（`RequestID`、`ClientIP`、`Recover`、路由组装与冲突分派、`/healthz` 已提供）。
  2. 所有错误都通过传入的 `ErrorWriter` 输出，中间件自己不拼错误 JSON。
  3. `OverloadGuard` 的两种占用（请求数、请求体字节）都用 CAS 循环；第二种占用失败时必须先归还第一种。
- **测试**：`router_test.go` 中已写好的 `TestHealthzNoAuth`、`TestUnauthenticatedAnthropicFormat`、`TestUnauthenticatedOpenAIFormat`、`TestDisabledClient403`、`TestRateLimited429RetryAfter`、`TestModelsOpenAIFormat`（含白名单过滤）、`TestModelsAnthropicFormat`；`middleware_test.go` 中的 `TestOverloadGuardRejectsWhenFull`、`TestOverloadGuardBodyBudget`、`TestIPRateLimit`。另新增：
  - `TestAccessLogFields`（用 `slog.NewJSONHandler` 写到 `bytes.Buffer`，断言日志含 `request_id`、`status`、`client_id`、`client_ip`，且不含 Token）；
  - `TestOverloadGuardConcurrent`（并发 200 个请求、上限 50，任一时刻 `InFlight()` 不超过 50，结束后归零）；
  - `TestSpoofedXFFIgnored`（`Trusted` 为空时，客户端伪造 `X-Forwarded-For` 不影响 IP 限流的键）。
- **注意**：测试路由使用真实的 anthropic 与 openai 入站适配器；`/v1/models` 的输出格式依赖 T09A/T09B 的 `WriteCatalog`。`Limiter` 与 `IPLimiter` 必须是两个不同的实例。`OverloadGuard.Middleware` 的 `buffered` 参数为 false 时不占请求体预算（OVL-02）。`router_test.go` 中的 `TestMCPMountedBehindAuth` 也属于本卡。

## T12 管理面 API — 状态：`TODO`

- **前置**：T02、T03、T05、T08
- **必读规格**：05 §4、09 §4
- **涉及文件**：`internal/admin/server.go`、`internal/admin/clients.go`（新建）、`internal/admin/keys.go`（新建）、`internal/admin/usage.go`（新建）、`internal/admin/server_test.go`
- **测试**：每个端点至少一个成功用例和一个失败用例；另外 `TestCannotDisableLastAdmin`、`TestRotateTokenInvalidatesOld`、`TestNoSecretsInResponses`（对所有响应体做字符串检查，不得出现 secret 明文或 `token_hash`）、`TestAuthFailureIPLockout`。

## T13 审计日志流水线 — 状态：`DONE`

- **前置**：T02
- **必读规格**：本卡；02 §5
- **涉及文件**：`internal/audit/audit.go`、`internal/audit/audit_test.go`
- **行为**：
  - `AUD-01` `Submit(rec)` 非阻塞：队列满时丢弃并 `dropped.Add(1)`，不返回错误。
  - `AUD-02` `Writer.Run` 按 `batch_size` 或 `flush_interval` 批量调用 `InsertAuditRecords`；写失败**丢弃该批**并 `Warn`（审计可以丢，不重试）。
  - `AUD-03` `ctx` 取消后读空队列做最后一次写入（5 秒超时），然后返回。
  - `AUD-04` `Retention.Run` 启动时立即执行一次，之后每 `Interval`（默认 1 小时）执行一次 `DeleteAuditBefore(Now().AddDate(0, 0, -RetentionDays))`；失败只记 `Warn`。
  - `AUD-05` `Dropped()` 返回累计丢弃数；每次 flush 时若自上次以来有新增丢弃，输出一条 `Warn`。
- **测试**：`TestSubmitDropsWhenFull`、`TestWriterBatches`、`TestWriterDrainsOnShutdown`、`TestRetentionDeletesOld`。

## T14 进程装配与端到端测试 — 状态：`TODO`

- **前置**：T01 ~ T13 全部
- **必读规格**：03 全章（含 §6 SRV-01 ~ SRV-03、§7）、05 §5、09 §5、13 §6
- **涉及文件**：`cmd/gateway/serve.go`、`cmd/gateway/e2e_test.go`（新建）
- **要做的事**：
  1. 按 03 §5 的顺序实现 `serve`；实现 BOOT-01 ~ BOOT-03；SIGINT/SIGTERM 优雅关闭。
  2. 两个 `http.Server` 都按 03 §6 设置超时，`WriteTimeout` 为 0；先 `net.Listen` 再 `Serve`。
  3. 用 `clientip.ParseTrusted(cfg.Server.TrustedProxies)` 构造 `Trusted`，同时传给数据面路由与管理面。
  4. 用已提供的 `newRegistry()`（`adapters.go`）得到注册表，传给 `core.New` 与 `httpapi.NewDataRouter`。
  5. 构造两个独立的 `ratelimit.Limiter`（客户端、IP）与一个 `httpapi.NewOverloadGuard`；每 5 分钟对两个 Limiter 调用 `Sweep`。
  6. `cfg.MCP.Disabled` 为 false 时构造 `mcp.New(mcp.Deps{...})`，作为 `DataDeps.MCP`，`DataDeps.MCPErrorWriter = mcp.WriteHTTPError`。
- **测试**：`TestE2E`：在 `t.TempDir()` 中写配置，在 goroutine 中启动 `run(ctx, cfg, runOptions{Stderr: &buf, OnListening: ...})`（两个监听地址都设为 `127.0.0.1:0`，通过 `OnListening` 拿到实际地址；bootstrap Token 从 `buf` 中按 BOOT-02 的格式解析），用 bootstrap 管理员 Token 调管理 API 创建客户端与 Key（Key 指向 `httptest` 假上游），然后以客户端身份发流式与非流式请求，最后取消 ctx，再用 `mcp.Client` 对同一网关调用 `tools/list` 与 `ask_model`（假上游实现 `/v1/responses`），断言 `usage_events` 中有 3 条记录、客户端 `spent` > 0、进程干净退出。另新增 `TestSlowHeaderClosed`：把 `read_header_timeout` 设为 200ms，建立 TCP 连接后只发半个请求头，断言 1 秒内连接被服务端关闭。

## T15 部署示例与用户部署指南 — 状态：`TODO`

- **前置**：T14
- **必读规格**：12 全章
- **涉及文件**：12 §6 表格中列出的 5 个文件（全部新建）
- **要做的事**：按 12 §3 ~ §5 编写示例文件与 `docs/DEPLOYMENT.md`。示例中的数值必须与 `gateway.example.yaml` 的默认值一致。
- **验收**：
  - 用 `caddy validate --config deploy/Caddyfile.example --adapter caddyfile`、`nginx -t -c`（在临时目录中构造最小 nginx.conf 引入该片段）、`systemd-analyze verify` 校验示例文件；本机没有对应工具时在交付说明中写明「未验证」，不得声称已验证。
  - `install-task.ps1` 在非管理员 PowerShell 中运行时给出明确提示并以非零码退出。

## T32 MCP 服务端 — 状态：`TODO`

- **前置**：T10、T09C
- **必读规格**：**14 全章**；09 §1（RTE-05）；13 §2
- **涉及文件**：`internal/mcp/server.go`、`internal/mcp/invoke.go`、`internal/mcp/mcp_test.go`
- **要做的事**：`Server.ServeHTTP`（SRV-M01 ~ SRV-M06）、`dispatch`（SRV-M07）、`listTools`（TL-01 ~ TL-04）、`callTool`（TL-05 ~ TL-11）。`DecodeRequest`、`ResultResponse`、`ErrorResponse`、`WriteHTTPError`、`newInternalRequest`、`fillPath`、`bufferedWriter` 已提供。
- **测试**：`mcp_test.go` 中的 `TestInitialize`、`TestTransportRules`、`TestToolsListFiltered`、`TestToolsCall`、`TestToolsCallResultTooLarge`；另新增 `TestOriginRejected`（SRV-M06）、`TestToolsCallCanceledWithClient`（外层请求取消时内部请求随之取消）、`TestNoSecretsInToolOutput`（结果与错误中不出现上游 Key 与网关 Token）。
- **陷阱**：工具执行失败要放进 `isError: true` 的**结果**里，不要返回 JSON-RPC 错误——只有「工具不存在」与报文格式错误是 JSON-RPC 错误（TL-05）。内部请求不经过中间件，不要再调用 `Authenticate`。

## T33 MCP 客户端、stdio 桥与命令行 — 状态：`TODO`

- **前置**：T32
- **必读规格**：14 §3、§4
- **涉及文件**：`internal/mcp/client.go`、`internal/mcp/mcp_test.go`、`cmd/gateway/client_cmds_test.go`（新建）
- **要做的事**：`Client.Post`（CLI-01）、`ListTools`（CLI-02）、`CallTool`（CLI-03）、`Bridge`（CLI-04）。命令行解析（`client_cmds.go`）已提供。
- **测试**：`TestClientAndBridge`；`client_cmds_test.go`：用 `httptest` 网关跑 `gateway tools`、`gateway call`（含 `--args -` 与 `isError` 退出码 1）；`Bridge` 遇到网关返回 500 时对请求输出 `-32603` 错误行、对通知不输出。
- **陷阱**：stdout 是 MCP 报文通道，任何日志写到 stdout 都会让客户端解析失败；Token 不得出现在错误信息中。

---

## 扩展适配器（非大模型 API）

这两张卡验证「新增协议不修改内核」：除下列文件外不得修改任何文件。发现契约不够用时停下报告（spec/08 §6）。

## T30 Tavily 搜索适配器 — 状态：`DONE`

- **前置**：无
- **必读规格**：08 §1、§2.1、§3.1、§3.7、§5、§6、**§8.1**；06 §1–§2；厂商当时的官方 API 文档（核对端点、字段与计费）
- **涉及文件**：`internal/adapter/tavily/`（新建：`register.go`、`ingress.go`、`egress.go`、`conformance_test.go`、`ingress_test.go`、`egress_test.go`）、`cmd/gateway/adapters.go`（加一行注册）、`internal/archtest/archtest_test.go`（`layers` 加一行）、`spec/08_adapters.md`（把 §8.1 从「设计参考」改为正式规则，补充核对后的字段）
- **要做的事**：按 08 §8.1 实现；`Units() = ["credits"]`；请求体中的 `api_key` 不得转发给上游；实现 `ToolSource`，提供工具 `tavily_search`、`tavily_extract`（spec/14 §1，结果为「标题 + URL + 摘要」的文本列表），并在 14 §1 的工具表中补上。
- **测试**：一致性套件全部通过（`Valid` 覆盖全部操作；`EgressCase` 覆盖有用量字段与无用量字段两种响应）；`egress_test.go`：请求体中客户端的 `api_key` 不出现在上游请求中、`search_depth=advanced` 的估计为 2 credits。

## T31 MinerU 文档解析适配器 — 状态：`DONE`

- **前置**：无
- **必读规格**：08 §1、§2.1、§3.1、§3.7、§5、§6、**§8.2**；07 §7.1；13 §2（CORE-07、CORE-08）、§5；厂商当时的官方 API 文档
- **涉及文件**：`internal/adapter/mineru/`（新建：同上 6 个文件 + `testdata/`）、`cmd/gateway/adapters.go`、`internal/archtest/archtest_test.go`、`spec/08_adapters.md`（§8.2 改为正式规则）
- **要做的事**：按 08 §8.2 同时实现同步封装与异步透传；实现 `ToolSource`，提供工具 `mineru_parse`（参数为文档 URL，走同步封装，结果为 Markdown 文本），并在 14 §1 的工具表中补上；上传用 `BodyStream`；轮询间隔从 1 秒起、每次翻倍、上限 10 秒，用 `select` 等待（EG-02）；结果下载用 `ResponseRaw`。
- **测试**：一致性套件全部通过（`EgressCase` 至少覆盖：同步封装轮询多次后完成、提交返回 `Pins`、查询不计费、下载为字节流；流式请求体用 `NewBodyStream`）；`egress_test.go`：轮询期间 ctx 取消时 `Do` 在 100ms 内返回 `*AttemptError`、上游任务失败状态映射为 `*AttemptError`（`OutcomeClientError`，错误体包含上游原因）。

---

## M2（M1 全部 DONE 后再开始）

| 卡 | 内容 | 前置 | 必读 |
|:--|:--|:--|:--|
| T20 | OpenAI Chat 请求 → IR（10 §3） | T14 | 10 §1–§3 |
| T21 | IR → Anthropic 请求（10 §4） | T20 | 10 §4 |
| T22 | Anthropic 响应/流 → OpenAI 响应/流（10 §5，实现 `Translator.Response`：缓冲体转换 + 包装 `EventStream`）；实现 `translate.Register` 并在 `cmd/gateway/adapters.go` 中注册（内核无需修改） | T21 | 10 §0、§5 |
| T23 | Anthropic 请求 → IR → OpenAI 请求 | T22 | 10 §6（届时补充） |
| T24 | OpenAI 响应/流 → Anthropic 响应/流 | T23 | 10 §6（届时补充） |
