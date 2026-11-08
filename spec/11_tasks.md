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
  T04、T05、T07、T09A、T09B 无前置，可并行
  T10 需要 T05、T08、T09A、T09B
  T11 需要 T03、T04、T10
  T12 需要 T02、T03、T05、T08
  T14 需要以上全部
M2：T20 → T21 → T22，T23 → T24（全部需要 T14）
```

可以同时开工的第一批：**T01、T04、T05、T07、T09A、T09B**（互不修改同一文件）。

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
| `provider` | 接口、`OutcomeKind.Retryable`、`ClassifyHTTP` / `ClassifyNetError` / `ParseRetryAfter` |
| `scheduler` | `Key`、`NewKey`、`tryAcquire`、`PoolsFromConfig` |
| `httpapi` | `RequestID`、`Recover`、`FormatForPath`、`trackingWriter`、`chain` |
| `cmd/gateway` | `version`、`check-config` 子命令 |
| `archtest`、`tools/check` | 分层检查与一键检查 |

查看还剩哪些待办：`git grep -n "TODO(T"`。

---

## T01 SQLite 仓储：客户端与设置 — 状态：`TODO`

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

## T02 SQLite 仓储：上游 Key、计费事件、审计 — 状态：`TODO`

- **前置**：T01
- **必读规格**：04 §3–§4（STO-06 ~ STO-14）、05 §2.3
- **涉及文件**：`internal/store/sqlite/keys.go`、`internal/store/sqlite/usage.go`、`internal/store/sqlite/audit.go`、`internal/store/sqlite/keys_test.go`、`internal/store/sqlite/usage_test.go`
- **要做的事**：
  1. `InsertUpstreamKey` 严格按 STO-06 的四步在**一个事务**中完成；`seal` 返回错误时回滚并返回该错误。
  2. `InsertUsageEvents` 使用 `INSERT OR IGNORE`；`InsertAuditRecords` 普通 `INSERT`；两者都复用一条 prepared statement。
  3. `QueryUsage` 用一条 `GROUP BY client_id, model` 的 SQL 完成，返回按 `client_id, model` 排序。
- **测试**：`TestInsertUpstreamKeySealUsesID`、`TestInsertUpstreamKeySealErrorRollsBack`、`TestUpstreamKeyUpdates`（`keys_test.go`）；`TestUsageEventsIdempotent`、`TestSumCostByClient`、`TestQueryUsage`、`TestAuditInsertAndDelete`（`usage_test.go`）。
- **陷阱**：`seal` 必须拿到**真实的自增 id**（`res.LastInsertId()`），这是 AAD 绑定行主键的前提。

## T03 客户端索引重载 — 状态：`TODO`

- **前置**：T01
- **必读规格**：05 §3.3–§3.4
- **涉及文件**：`internal/auth/reloader.go`、`internal/auth/reloader_test.go`
- **要做的事**：实现 `Reloader.Reload(ctx)`：`ListClients` → `Index.Replace`。错误用 `fmt.Errorf("reload clients: %w", err)` 包装。
- **测试**：`TestReloaderReplacesIndex`（已写好，使用只实现 `ListClients` 的 `fakeLister`）；另补充一个 `ListClients` 返回错误时 `Reload` 返回包装错误、且索引保持旧内容的用例。

## T04 每客户端 RPM 限流 — 状态：`TODO`

- **前置**：无
- **必读规格**：09 §3（RL-01 ~ RL-06）
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
  2. `scheduler.go`：`New`、`SetKeys`（规则见 SCH-16）、`Acquire`、`Lease.Finish`、`KeyStatus`。
  3. `load.go`：`LoadKeys(ctx, st, kr, log)`：`ListUpstreamKeys` → `kr.Open(k.Sealed, crypto.UpstreamKeyAAD(k.ID, k.Pool, k.Sealed.KEKVersion))` → `NewKey(..., ParsePersistedState(k.State))`。单个 Key 解密失败：记录 `Error` 日志（不含密文与明文）并跳过该 Key（不要让整个网关起不来）。
- **测试**：`scheduler_test.go` 中已写好的全部用例（`TestRateLimitedCooldown`、`TestAcquireSkipsCooldown`、`TestAcquireRespectsMaxInFlight`、`TestAffinityPreferred`、`TestAffinityFallbackWhenBusy`、`TestExcludeTried`、`TestLeastInflightOrder`、`TestWeightedRoundRobinDistribution`、`TestFinishOnce`、`TestAuthFailedTwiceQuarantines`、`TestOverloadedNoPenalty`、`TestNoKeyRetryAfter`、`TestBreakerOpensAndHalfOpen`、`TestSetKeysPreservesRuntime`、`TestSetKeysChangedFieldsKeepCooldown`）；`breaker_test.go` 覆盖：窗口过期后统计清零、`MinRequests` 未达到时不打开、探测失败后打开时长翻倍且不超过上限；`load_test.go` 覆盖：解密失败的 Key 被跳过。
- **陷阱**：`OnQuarantine` 回调**不能**在持锁时调用；用 `go` 调用或在释放锁后调用。

## T09A Anthropic 适配器 — 状态：`TODO`

- **前置**：无
- **必读规格**：08 §1–§6（PT-01、PT-02、PT-04、HDR-01 ~ HDR-03、HDR-05、USE-01、USE-02、FWD-01、STR-05）
- **涉及文件**：`internal/provider/anthropic/adapter.go`、`internal/provider/anthropic/adapter_test.go`
- **测试**：`TestPrepareBodyRewritesModelOnly`（断言 `messages`、`system`、`tools` 的原始字节不变）、`TestNewRequestHeaders`（客户端 `Authorization` 不被转发；`anthropic-beta` 多值全部复制；缺省 `anthropic-version`）、`TestUsageTapStream`（用 `testdata/stream_basic.sse`）、`TestUsageTapBody`、`TestClassifyStreamError`。

## T09B OpenAI 兼容适配器 — 状态：`TODO`

- **前置**：无
- **必读规格**：08 §1–§6（PT-01 ~ PT-03、HDR-01、HDR-02、HDR-04、HDR-05、USE-03 ~ USE-06、FWD-02、FWD-03）
- **涉及文件**：`internal/provider/openai/adapter.go`、`internal/provider/openai/adapter_test.go`
- **测试**：`TestPrepareBodyInjectsUsage`（三种情况：无 `stream_options`、有但为 false、已为 true）、`TestPrepareBodyNonStreamNoInject`、`TestNewRequestHeaders`、`TestUsageCachedTokens`（OpenAI 与 DeepSeek 两种字段）、`TestUsageStream`（含 `[DONE]` 被忽略）、`TestShouldForwardFiltersInjectedUsageChunk`。

## T10 数据面编排（proxy） — 状态：`TODO`

- **前置**：T05、T08、T09A、T09B
- **必读规格**：01 §6（LIFE-07 ~ LIFE-16）、06 §2–§3、07 §6–§7、08 §6–§7
- **涉及文件**：`internal/proxy/handler.go`、`internal/proxy/forward.go`（新建）、`internal/proxy/affinity.go`、`internal/proxy/handler_test.go`、`internal/proxy/affinity_test.go`
- **要做的事**：
  1. `affinity.go`：按 07 §7 实现 `AffinityKey`。
  2. `handler.go`：`ServeHTTP` 按 LIFE-07 起的步骤编排。所有收尾动作（Finish、Settle/Release、audit.Submit）放在一个 `defer` 函数中，保证任何 return 路径都会执行。
  3. `forward.go`：流式与非流式转发、超时定时器、错误事件。
- **测试**：`handler_test.go` 已提供测试环境 `newTestEnv`（假上游 + 真实 ledger/scheduler/adapter）和以下用例：`TestPassthroughNonStream`、`TestPassthroughStreamAnthropic`（含结算金额断言）、`TestRetryOn429ThenSuccess`、`TestNoRetryAfterFirstByte`、`TestClientErrorPassedThrough`、`TestProtocolMismatchRejected`；`affinity_test.go` 提供 `TestAffinityKeySources`。
  你还必须用同一环境**新增**：`TestPassthroughStreamOpenAIInjectedUsageFiltered`（客户端收不到空 choices 的 usage chunk，但结算用到了 usage）、`TestReleaseWhenAllAttemptsFail`（所有 Key 都 500 → 客户端 502，账户 reserved 归零且无计费事件）、`TestModelNotAllowed`（Principal 白名单不含该模型 → 403，上游未被调用）、`TestUnknownModel`（→ 400）、`TestClientCancelStopsUpstream`（客户端 ctx 取消后上游请求的 ctx 也被取消）、`TestInsufficientQuota`（→ 402）。
- **陷阱**：判断「是否已写出首字节」用一个包装 `http.ResponseWriter` 记录 `wroteHeader`；不要只看局部变量。

## T11 数据面路由与中间件 — 状态：`TODO`

- **前置**：T03、T04、T10
- **必读规格**：09 §1–§2
- **涉及文件**：`internal/httpapi/router.go`、`internal/httpapi/middleware.go`、`internal/httpapi/models.go`（新建）、`internal/httpapi/router_test.go`
- **要做的事**：实现 `Authenticate`、`RateLimit`、`AccessLog` 中间件（`RequestID`、`Recover` 已提供）与 `NewDataRouter`；实现 `/v1/models`、`/healthz`。
- **测试**：`router_test.go` 中已写好的 `TestHealthzNoAuth`、`TestUnauthenticatedAnthropicFormat`、`TestUnauthenticatedOpenAIFormat`、`TestDisabledClient403`、`TestRateLimited429RetryAfter`、`TestModelsOpenAIFormat`（含白名单过滤）、`TestModelsAnthropicFormat`；另新增 `TestAccessLogFields`（用 `slog.NewJSONHandler` 写到 `bytes.Buffer`，断言日志含 `request_id`、`status`、`client_id`，且不含 Token）。
- **注意**：`DataDeps.Proxy` 在测试中为 nil；`/v1/models` 需要模型列表时从 `DataDeps.Config.Models` 读取，不要依赖 `Proxy`。

## T12 管理面 API — 状态：`TODO`

- **前置**：T02、T03、T05、T08
- **必读规格**：05 §4、09 §4
- **涉及文件**：`internal/admin/server.go`、`internal/admin/clients.go`（新建）、`internal/admin/keys.go`（新建）、`internal/admin/usage.go`（新建）、`internal/admin/server_test.go`
- **测试**：每个端点至少一个成功用例和一个失败用例；另外 `TestCannotDisableLastAdmin`、`TestRotateTokenInvalidatesOld`、`TestNoSecretsInResponses`（对所有响应体做字符串检查，不得出现 secret 明文或 `token_hash`）、`TestAuthFailureIPLockout`。

## T13 审计日志流水线 — 状态：`TODO`

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
- **必读规格**：03 全章、05 §5
- **涉及文件**：`cmd/gateway/serve.go`、`cmd/gateway/e2e_test.go`（新建）
- **要做的事**：按 03 §5 的顺序实现 `serve`；实现 BOOT-01 ~ BOOT-03；SIGINT/SIGTERM 优雅关闭。
- **测试**：`TestE2E`：在 `t.TempDir()` 中写配置，在 goroutine 中启动 `run(ctx, cfg, runOptions{Stderr: &buf, OnListening: ...})`（两个监听地址都设为 `127.0.0.1:0`，通过 `OnListening` 拿到实际地址；bootstrap Token 从 `buf` 中按 BOOT-02 的格式解析），用 bootstrap 管理员 Token 调管理 API 创建客户端与 Key（Key 指向 `httptest` 假上游），然后以客户端身份发流式与非流式请求，最后取消 ctx，断言 `usage_events` 中有 2 条记录、客户端 `spent` > 0、进程干净退出。

---

## M2（M1 全部 DONE 后再开始）

| 卡 | 内容 | 前置 | 必读 |
|:--|:--|:--|:--|
| T20 | OpenAI Chat 请求 → IR（10 §3） | T14 | 10 §1–§3 |
| T21 | IR → Anthropic 请求（10 §4） | T20 | 10 §4 |
| T22 | Anthropic 响应/流 → OpenAI 响应/流（10 §5），并在 `proxy` 中接入转译路径 | T21 | 10 §5 |
| T23 | Anthropic 请求 → IR → OpenAI 请求 | T22 | 10 §6（届时补充） |
| T24 | OpenAI 响应/流 → Anthropic 响应/流 | T23 | 10 §6（届时补充） |
