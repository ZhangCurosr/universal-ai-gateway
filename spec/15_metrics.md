# 15 Prometheus 指标端点（Metrics）

> 适用里程碑：M3（运维增强）
> 任务卡：T45
> 前置规格：[01_scope_and_architecture.md](./01_scope_and_architecture.md)（M3 范围与分层）、[03_config.md](./03_config.md)（CFG-02 管理面回环绑定）、[09_http_api.md](./09_http_api.md)（管理面服务）

网关提供 Prometheus 文本格式（exposition format 0.0.4）的指标端点，用于监控网关运行状态、流量、错误、耗时与资源占用。

---

## 1. 端点契约

| 编号 | 规则 |
|:--|:--|
| MTR-01 | **路径与方法**：端点路径为 `GET /admin/metrics`，挂载于管理面监听（由 CFG-02 保证只能绑定回环地址）。非 `GET` 请求返回 `405 Method Not Allowed`，并带有 `Allow: GET` 响应头。 |
| MTR-02 | **免鉴权**：由于管理面仅限本地回环（`127.0.0.1` / `::1`）访问，且指标不包含凭据与客户端身份信息，该端点**无需** Bearer Token 认证。 |
| MTR-03 | **响应头**：HTTP 状态码为 `200`，`Content-Type: text/plain; version=0.0.4; charset=utf-8`。 |
| MTR-04 | **输出稳定性**：输出时按指标家族名称字典序升序输出；同一指标的多条样本行按标签名和标签值字典序升序排序，保证输出确定可预测。 |
| MTR-05 | **标签基数有界与脱敏**：严禁在指标标签中包含客户端 ID、Token、请求 ID、Key 凭据或任何用户输入。标签值集合必须严格有界：`model` 只取配置中的模型名，其余写 `other`。 |

---

## 2. 指标清单

指标名称统一以 `uaig_` 为前缀。

### 2.1 计数器与直方图（由 AuditRecord 同步观察）

内核在每个请求结束时生成 `store.AuditRecord`。通过在 `audit.Pipeline` 设置同步观察者，在审计入队或丢弃判断之前同步更新以下指标：

| 指标名称 | 类型 | 标签 | 含义与取值规则 |
|:--|:--|:--|:--|
| `uaig_requests_total` | counter | `model`, `ingress`, `upstream`, `outcome`, `code` | 结束的请求总数。<br>- `model`: `AuditRecord.Model`；不在配置 `models[].name` 中的值（含空值，例如客户端请求了不存在的模型）一律写 `other`（MTR-05）<br>- `ingress`: `AuditRecord.IngressProtocol`<br>- `upstream`: `AuditRecord.UpstreamProtocol`<br>- `outcome`: `AuditRecord.Outcome`（若为空写 `none`）<br>- `code`: HTTP 状态码类别（`2xx`, `4xx`, `5xx`；状态码为 0 时写 `none`） |
| `uaig_request_errors_total` | counter | `model`, `error_code` | 产生业务错误的请求数。<br>- `error_code`: `AuditRecord.ErrorCode`（来自 `apperr`）。仅在 `ErrorCode != ""` 时递增。 |
| `uaig_upstream_attempts_total` | counter | `model`, `upstream` | 上游尝试总次数。<br>每次按 `AuditRecord.Attempts` 累加（若 > 0）。 |
| `uaig_request_duration_seconds` | histogram | `model`, `stream` | 请求端到端耗时（秒）。<br>取 `float64(rec.LatencyMillis)/1000.0`。<br>- `stream`: `"true"` 或 `"false"`<br>桶边界：`0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600` |
| `uaig_ttfb_seconds` | histogram | `model`, `stream` | 首字节响应时间（秒）。<br>取 `float64(rec.TTFBMillis)/1000.0`。<br>当 `TTFBMillis < 0`（即无首字节）时不计入。<br>- `stream`: `"true"` 或 `"false"`<br>桶边界：`0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30` |

### 2.2 仪表（抓取时动态读取）

以下指标在 `GET /admin/metrics` 收到请求时动态读取：

| 指标名称 | 类型 | 标签 | 来源与取值规则 |
|:--|:--|:--|:--|
| `uaig_keys_status` | gauge | `pool`, `id`, `name`, `state` | 上游 Key 状态。<br>对每把 Key，针对每个候选状态各输出一行：当前激活为 1，未激活为 0。<br>`state` 候选项包括：`ready`, `quarantined`, `disabled`, `cooldown`, `breaker_open`。 |
| `uaig_key_inflight` | gauge | `pool`, `id`, `name` | 上游 Key 当前在途请求数。<br>来源：`scheduler.KeyStatus().InFlight`。 |
| `uaig_ledger_events_queue_length` | gauge | — | 计费事件通道当前积压长度：`len(ledger.Events())`。 |
| `uaig_ledger_events_queue_capacity` | gauge | — | 计费事件通道最大容量：`cap(ledger.Events())`。 |
| `uaig_overload_inflight_requests` | gauge | — | 全局在途请求数：`OverloadGuard.InFlight()` 的 requests。 |
| `uaig_overload_inflight_body_bytes` | gauge | — | 全局缓冲的请求体在途字节数：`OverloadGuard.InFlight()` 的 bodyBytes。 |
| `uaig_audit_dropped_total` | counter | — | 审计记录累计丢弃数：`audit.Pipeline.Dropped()`。 |
| `uaig_build_info` | gauge | `version` | 网关构建版本信息，取值恒为 1。 |
| `uaig_start_time_seconds` | gauge | — | 进程启动的 Unix 时间戳（秒）。 |
| `uaig_goroutines` | gauge | — | 进程当前 Goroutine 数量：`runtime.NumGoroutine()`。 |

---

## 3. 文本格式规范（Exposition Format 0.0.4）

| 编号 | 规则 |
|:--|:--|
| MTR-06 | **HELP 与 TYPE 声明**：每个指标系列前必须先输出一行 `# HELP <name> <doc>` 和一行 `# TYPE <name> <type>`（类型为 `counter`、`gauge` 或 `histogram`）。 |
| MTR-07 | **字符转义**：标签值中的反斜杠 `\` 转义为 `\\`，双引号 `"` 转义为 `\"`，换行符 `\n` 转义为 `\n`。 |
| MTR-08 | **直方图输出**：直方图按累积桶输出 `<name>_bucket{...,le="<bucket>"}`，并包含 `<name>_bucket{...,le="+Inf"}`、`<name>_sum` 与 `<name>_count`。`+Inf` 的计数必须等于 `_count`。 |
| MTR-09 | **数值格式化**：浮点数值使用 `strconv.FormatFloat(v, 'g', -1, 64)` 格式化，整数直接格式化为整数字符串。以 LF（`\n`）换行。 |

---

## 4. 架构与依赖约束

| 编号 | 规则 |
|:--|:--|
| MTR-10 | **零第三方依赖**：禁止引入任何 Prometheus 外部 SDK（如 `client_golang`），所有文本格式化由 `internal/metrics` 纯 Go 手写实现。 |
| MTR-11 | **分层规范**：`internal/metrics` 属于第 2 层，只依赖标准库与 `internal/store`（引用 `AuditRecord`）。不得导入 `core`、`httpapi` 或 `admin`。动态仪表通过注入闭包函数解耦。 |
| MTR-12 | **无锁热路径**：`ObserveRequest` 必须为非阻塞操作，不得执行任何磁盘或网络 I/O，使用内存原子操作或细粒度互斥锁，保障极低调用开销。 |
| MTR-13 | 池级仪表（T64）：`uaig_pool_available_keys{pool}`（每个池）、`uaig_pool_quota_left{pool}`（只对声明了 `quota` 的池）、`uaig_pool_keys_expiring{pool}`（只对配置了 `expiring_within` 的池）、`uaig_alerts_firing{pool,kind}`（只输出正在告警的条目，值为 1）；口径同 spec/07 AL-01 |
