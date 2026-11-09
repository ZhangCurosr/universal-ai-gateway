# T45 Prometheus 指标端点（M3 第一项）

- **类型**：规格 + 实现
- **前置**：T40（指标代码全是并发计数，需要 CI 上的竞态检测兜底）
- **需要用户审批**：**是**。spec/01 规定「M3 的规格在 M2 完成后补充」。本卡的阶段 A 就是补这部分规格，批准后才能写代码。
- **必读**：
  - [`spec/01_scope_and_architecture.md`](../../spec/01_scope_and_architecture.md) §2（M3 范围）、模块地图与分层
  - [`spec/09_http_api.md`](../../spec/09_http_api.md)（管理面监听与鉴权）、[`spec/03_config.md`](../../spec/03_config.md) CFG-02（管理面只能监听回环地址）
  - [`internal/audit/audit.go`](../../internal/audit/audit.go)（`Pipeline.Submit`：每个请求结束时内核提交一条 `store.AuditRecord`）
  - [`internal/store/store.go`](../../internal/store/store.go) 中的 `AuditRecord`
  - [`internal/scheduler/scheduler.go`](../../internal/scheduler/scheduler.go) 中的 `KeyStatus()`
  - [`internal/ledger/memory.go`](../../internal/ledger/memory.go) 中的 `Events()`
  - [`internal/httpapi/middleware.go`](../../internal/httpapi/middleware.go) 中的 `OverloadGuard.InFlight()`
  - [`cmd/gateway/serve.go`](../../cmd/gateway/serve.go)：管理面 handler 的装配
  - [`internal/archtest/archtest_test.go`](../../internal/archtest/archtest_test.go)：新包必须在 `layers` 中登记

## 1. 背景与约束

现在网关只能靠日志和审计表观察运行状态，可观测性很弱。本卡新增一个 Prometheus 文本格式的指标端点。

约束：

1. **不新增依赖**。spec/02 §3 的白名单里没有 `prometheus/client_golang`，文本格式（exposition format 0.0.4）要手写。
2. **不改内核**。`internal/core` 是冻结的。数据来源是内核已经产出的东西：每个请求结束时提交的 `AuditRecord`，以及调度器、账本、过载保护已有的查询方法。
3. **热路径开销极小**。记录指标只能是原子操作或很短的加锁，不能分配大对象，不能做 I/O。
4. **标签基数有界**。标签里不能出现客户端 ID、Token、请求 ID、用户输入。

## 2. 关键设计：数据从哪里来

内核在每个请求的收尾阶段调用 `audit.Pipeline.Submit(rec)`，见 `core.go` 的 `submitAudit`。`Submit` 是非阻塞的，**队列满时会丢弃**（AUD-01）。
指标不能跟着审计一起丢，所以要在**丢弃判断之前**观察记录：

```go
// internal/audit：新增
// SetObserver 设置一个同步观察者：每条 Submit 的记录先交给 fn，再按 AUD-01 入队或丢弃。
// 必须在开始接收请求之前调用；fn 必须是非阻塞、并发安全的。
func (p *Pipeline) SetObserver(fn func(store.AuditRecord))
```

`audit` 不是冻结包，新增方法不违反 spec/13 §7。字段的读写要考虑并发：设置之后只读，可以用 `atomic.Pointer` 保存。

队列长度、Key 状态这类「瞬时值」不需要在热路径上记录，**在抓取时现算**：

| 指标 | 来源 |
|:--|:--|
| Key 状态、在途数、熔断状态 | `scheduler.KeyStatus()` |
| 计费事件队列长度与容量 | `len(ledger.Events())`、`cap(ledger.Events())` |
| 全局在途请求数、在途请求体字节数 | `OverloadGuard.InFlight()` |
| 审计丢弃数 | `audit.Pipeline.Dropped()` |

## 3. 规格草案（阶段 A：写进 spec 后提交审批）

> 建议新增 `spec/15_metrics.md`，并在 spec/01 §2 的 M3 小节、spec/README 的目录中加上引用。标了「待定」的地方请用户回答。

### 3.1 端点

| 项 | 规定 |
|:--|:--|
| 路径 | `GET /admin/metrics`，挂在**管理面监听**上（CFG-02 保证只能是回环地址） |
| 鉴权 | *待定 1*：a. 不需要鉴权（依赖管理面只监听回环地址）；b. 需要管理员 Token（`Authorization: Bearer`）。建议 a：Prometheus 配置里就不用放权限很大的管理员 Token。指标里没有凭据，也没有客户端身份 |
| 响应 | `200`，`Content-Type: text/plain; version=0.0.4; charset=utf-8` |
| 输出顺序 | 按指标名、再按标签值排序，保证输出稳定，方便测试 |
| 方法 | 非 GET 返回 `405` |

### 3.2 指标清单

名称前缀统一为 `uaig_`。

**计数器与直方图**（由 `AuditRecord` 观察者更新）：

| 名称 | 类型 | 标签 | 含义 |
|:--|:--|:--|:--|
| `uaig_requests_total` | counter | `model`、`ingress`、`upstream`、`outcome`、`code` | 结束的请求数。`outcome` 取 `AuditRecord.Outcome`，空值写 `none`；`code` 是 HTTP 状态码的类别（`2xx`、`4xx`、`5xx`，状态码为 0 时写 `none`） |
| `uaig_request_errors_total` | counter | `model`、`error_code` | 带错误码的请求数。`error_code` 取 `AuditRecord.ErrorCode`，即 apperr 的错误码，取值集合有限；为空时不计 |
| `uaig_upstream_attempts_total` | counter | `model`、`upstream` | 上游尝试次数，即 `AuditRecord.Attempts` 的累加 |
| `uaig_request_duration_seconds` | histogram | `model`、`stream` | 端到端耗时，取 `LatencyMillis`。桶：`0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600` |
| `uaig_ttfb_seconds` | histogram | `model`、`stream` | 首字节时间，取 `TTFBMillis`；值为 `-1`（没有首字节）时不计。桶：`0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30` |

**仪表**（抓取时现算）：

| 名称 | 类型 | 标签 | 来源 |
|:--|:--|:--|:--|
| `uaig_upstream_key_inflight` | gauge | `pool`、`key_id` | `KeyStatus().InFlight` |
| `uaig_upstream_key_state` | gauge | `pool`、`key_id`、`state` | 每把 Key 对每个状态各输出一行，当前状态为 1，其余为 0。状态包括持久状态（ready / quarantined / disabled）和是否冷却中 *（待定 2：确切的状态取值以 `scheduler.PersistedState` 的 String 为准，执行者在阶段 A 列出）* |
| `uaig_upstream_key_breaker_open` | gauge | `pool`、`key_id` | 熔断打开为 1，否则为 0 |
| `uaig_ledger_queue_length` / `uaig_ledger_queue_capacity` | gauge | — | 计费事件队列 |
| `uaig_inflight_requests` / `uaig_inflight_body_bytes` | gauge | — | `OverloadGuard.InFlight()` |
| `uaig_audit_dropped_total` | counter | — | `Pipeline.Dropped()` |
| `uaig_build_info` | gauge | `version` | 恒为 1 |
| `uaig_start_time_seconds` | gauge | — | 进程启动时间（Unix 秒） |
| `uaig_goroutines` | gauge | — | `runtime.NumGoroutine()` |

*（待定 3：要不要输出计费相关的计数器，例如按模型和计量单位累计的用量、累计成本？建议本卡不做。计费以数据库为准，可以通过管理 API 查询，指标端点不重复提供。）*

**明确不做**：

- 不按客户端 ID 打标签：基数没有上界，而且会泄露客户端分布。
- 不使用 Key 名称作为标签：名称可能包含运维人员写的敏感信息。只用 `key_id`。

### 3.3 文本格式要求

- 每个指标先输出 `# HELP <name> <说明>` 和 `# TYPE <name> <counter|gauge|histogram>`，再输出样本行。
- 标签值转义：`\` → `\\`，`"` → `\"`，换行 → `\n`。
- 直方图输出累积桶 `<name>_bucket{...,le="<上界>"}`、`le="+Inf"`、`<name>_sum`、`<name>_count`。
- 浮点数用 `strconv.FormatFloat(v, 'g', -1, 64)` 格式化。

### 3.4 分层

新包 `internal/metrics`：

- 放在第 2 层，与 `audit`、`scheduler` 同层；只依赖标准库与 `internal/store`（`AuditRecord` 类型）。
- 不得导入 `core`、`httpapi`、`admin`。
- 仪表的数据源由 `cmd/gateway` 以函数的形式注入，见 §4.3，避免 `metrics` 依赖上层包。

在 `internal/archtest` 的 `layers` 表中登记这个包。这是「登记新包」，不是修改分层规则，但仍然在审批请求里写明。

## 4. 实现要求（阶段 B：用户批准后）

### 4.1 `internal/metrics`

```go
// Collector 汇总指标。并发安全。
type Collector struct { /* 计数器与直方图：map + sync.Mutex，或按标签组合缓存 *atomic 计数 */ }

func NewCollector() *Collector
func (c *Collector) ObserveRequest(rec store.AuditRecord) // 交给 audit.Pipeline.SetObserver

// Gauges 是抓取时现算的数据源；任何字段为 nil 时跳过对应指标。
type Gauges struct {
	Keys          func() []KeyGauge   // 由 cmd/gateway 从 scheduler.KeyStatus() 转换
	LedgerQueue   func() (length, capacity int)
	InFlight      func() (requests, bodyBytes int64)
	AuditDropped  func() int64
	Version       string
	StartedAt     time.Time
}

// Handler 返回 GET /admin/metrics 的处理器。
func (c *Collector) Handler(g Gauges) http.Handler
```

- 热路径（`ObserveRequest`）的实现可以是：标签组合拼成 key 字符串，`sync.Mutex` 保护 `map[string]*series`。抓取时加同一把锁做快照，然后在锁外格式化输出。
- 直方图的桶边界是常量切片，观察时线性查找即可（桶数 ≤ 12）。

### 4.2 `internal/audit`

按 §2 新增 `SetObserver`，并写测试：

- 设置观察者后，`Submit` 的每条记录都会到达观察者，**包括队列满被丢弃的记录**。
- 没有设置观察者时，行为与现在完全一致。
- 并发 `Submit` 下没有竞态（T40 合入后由 CI 的 `-race` 验证）。

### 4.3 装配（`cmd/gateway/serve.go`）

- 创建 `Collector`，调用 `auditPipeline.SetObserver(collector.ObserveRequest)`。**必须在数据面开始监听之前完成。**
- 管理面 handler 外面再包一层 `http.ServeMux`：`/admin/metrics` 交给 `collector.Handler(gauges)`，其余路径交给原来的管理面 handler。`internal/admin` 包本身不需要改。
  待定 1 选 b（需要鉴权）时，就需要改 `internal/admin`：把指标路由放进它的鉴权链。
- `gauges` 用闭包从已经构造好的 scheduler、ledger、overload guard、audit pipeline 中取数据。

### 4.4 文档

- `docs/DEPLOYMENT.md` 新增一节「监控」：给出 Prometheus 抓取配置示例，以及建议的告警规则，例如：
  - `uaig_ledger_queue_length / uaig_ledger_queue_capacity > 0.8` 持续 1 分钟；
  - `rate(uaig_requests_total{code="5xx"}[5m])` 偏高；
  - 某个池里 `uaig_upstream_key_breaker_open == 1` 的 Key 数等于该池 Key 总数。
- `gateway.example.yaml` 不需要改，因为没有新增配置项。如果审批时决定加开关，再补。

## 5. 涉及文件

- spec：新增 `spec/15_metrics.md`；修改 `spec/01_scope_and_architecture.md`（M3 小节）、`spec/README.md`（目录）、`spec/11_tasks.md`（登记本卡）
- 新增：`internal/metrics/*.go` 及测试
- 修改：`internal/audit/audit.go` 及测试、`cmd/gateway/serve.go`、`cmd/gateway/e2e_test.go`（加一个指标断言）、`internal/archtest/archtest_test.go`（只在 `layers` 中登记新包）、`docs/DEPLOYMENT.md`

## 6. 测试要求

1. **格式黄金测试**：构造一个固定状态的 `Collector`（几条 `AuditRecord`）和固定的 `Gauges`，断言 `/admin/metrics` 的完整输出与 `internal/metrics/testdata/expected.txt` 逐字节一致。
2. **转义**：标签值含 `\`、`"`、换行时输出正确。
3. **直方图**：桶是累积的；`+Inf` 等于 `_count`；`_sum` 正确；`TTFBMillis = -1` 不计入。
4. **基数**：输出中不出现客户端 ID 和 Key 名称。可以用包含特殊名称的输入做反向断言。
5. **并发**：100 个 goroutine 同时 `ObserveRequest`，同时并发抓取，结束后计数精确。
6. **端到端**：`cmd/gateway` 的 `TestE2E` 结束前请求一次 `GET /admin/metrics`，断言 `uaig_requests_total{model="claude-test",...}` 的值等于测试发出的请求数。为了不改动现有断言，也可以新写一个独立的端到端测试。

## 7. 验收标准

- [ ] 阶段 A 的规格已获用户批准，3 个待定项都有答案。
- [ ] §6 的测试全部通过；T40 的 CI 上 `-race` 没有告警。
- [ ] 没有新增 `go.mod` 依赖，没有修改 `internal/core`、`internal/protocol`。
- [ ] 用一个真实的 Prometheus（或 `promtool check metrics`）验证输出格式能被解析。交付说明中贴出命令和结果。
- [ ] `docs/DEPLOYMENT.md` 的监控一节写完。
- [ ] `go run ./tools/check` 全绿。

## 8. 建议提交拆分

1. 阶段 A：`docs(spec): T45 新增 spec/15 指标端点（待审批）`
2. `feat(audit): T45 Pipeline 支持同步观察者`
3. `feat(metrics): T45 新增指标收集与 Prometheus 文本输出`
4. `feat(gateway): T45 在管理面挂载 /admin/metrics`
5. `docs(deploy): T45 监控与告警示例`
