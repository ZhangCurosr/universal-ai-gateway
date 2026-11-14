# 09 HTTP 接口（Data Plane & Admin API）

代码位置：`internal/httpapi`（路由组装、冲突分派、`/healthz`、`RequestID`、`ClientIP`、`Recover` 已提供；其余中间件 T11）、`internal/ratelimit`（T04）、`internal/admin`（T12）。

## 1. 数据面端点

**数据面的路由不写死在 httpapi 中**，而是由注册表中每个入站适配器的 `Routes()` 声明（spec/08 §2）。一个协议可以声明任意多个调用端点（大模型、搜索、文件上传、任务查询……）。当前注册的适配器产生以下端点：

| 方法 | 路径 | 声明者 | 处理者 |
|:--|:--|:--|:--|
| `POST` | `/v1/messages` | anthropic（Call，Op `messages`） | `core.Handler(anthropic 入站, 该路由)` |
| `POST` | `/v1/chat/completions` | openai_chat（Call，Op `chat.completions`） | `core.Handler(openai 入站, 该路由)` |
| `POST` | `/v1/responses`，以及 `GET`/`DELETE` `/v1/responses/{id}`、`POST /v1/responses/{id}/cancel`、`GET /v1/responses/{id}/input_items` | openai_responses（Call） | `core.Handler(responses 入站, 该路由)` |
| `GET` | `/v1/models` | anthropic 与 openai_chat（Catalog，冲突） | `Ingress.WriteCatalog(core.Models(principal))` |
| `POST`（`GET`/`DELETE` 返回 405） | `/mcp` | httpapi（由 `DataDeps.MCP` 注入，spec/14） | `mcp.Server` |
| `GET` | `/healthz` | httpapi 自身 | `200 {"status":"ok"}`，不鉴权 |

| 编号 | 规则（已实现于 `router.go`） |
|:--|:--|
| RTE-01 | 遍历 `Registry.Ingresses()`（按优先级），把每个适配器声明的每条路由注册到 `http.ServeMux`；`RouteCall` 的处理者是 `core.Handler(in, rt)`，`RouteCatalog` 的是 `in.WriteCatalog` |
| RTE-02 | 多个适配器声明同一 `(Method, Path)` 时：按优先级依次调用 `Claims(r)`，第一个返回 true 的处理；都不认领时交给兜底协议（若它声明了该路由），否则交给优先级最高者 |
| RTE-03 | 每条协议路由都套用 §1.1 的协议中间件，中间件的错误格式使用**该协议**的 `WriteError` |
| RTE-04 | 未匹配任何路由的请求返回 404，格式使用兜底协议（当前为 OpenAI） |
| RTE-05 | `DataDeps.MCP` 非 nil 时挂载 `/mcp`（所有方法），中间件与协议路由相同，错误格式用 `DataDeps.MCPErrorWriter`（`mcp.WriteHTTPError`）。httpapi 不导入 mcp 包（同层），由 `cmd/gateway` 注入（已实现） |

### 1.1 中间件顺序

```
RequestID → ClientIP → AccessLog → Recover → [协议路由: IPRateLimit → Overload → Authenticate → RateLimit] → 端点
```

顺序的理由：`IPRateLimit` 与 `Overload` 放在鉴权之前，使未鉴权的洪泛请求在计算 HMAC、读取请求体之前就被挡掉；`RateLimit`（每客户端）必须在鉴权之后，因为它需要 clientID。

协议中间件的签名都带一个 `ErrorWriter`（即 `protocol.Ingress.WriteError`），**中间件自己不拼错误 JSON**：

```go
type ErrorWriter func(w http.ResponseWriter, requestID string, err *apperr.Error)
func Authenticate(ix *auth.Index, pepper []byte, ew ErrorWriter) func(http.Handler) http.Handler
func RateLimit(l *ratelimit.Limiter, ew ErrorWriter) func(http.Handler) http.Handler
func IPRateLimit(l *ratelimit.Limiter, rpm int, ew ErrorWriter) func(http.Handler) http.Handler
func (g *OverloadGuard) Middleware(ew ErrorWriter, buffered bool) func(http.Handler) http.Handler // buffered = (rt.Body == BodyBuffered)
```

| 编号 | 中间件 | 行为 |
|:--|:--|:--|
| MW-01 | `RequestID` | 生成 `req_` + 24 位小写 base32 随机串，存入 context，写响应头 `X-Request-Id`。**忽略**客户端传来的同名头（已提供） |
| MW-02 | `AccessLog` | 请求结束后输出一条 `Info` 日志：`request_id, method, path, status, latency_ms, client_id`（未鉴权为 0）、`client_ip`（取自 MW-06）。不记录请求体与头部 |
| MW-03 | `Recover` | 捕获 panic：`Error` 日志（含堆栈）；若尚未写响应头则用兜底协议格式返回 500 `internal`（已提供） |
| MW-04 | `Authenticate` | `auth.ExtractCredential` → `HashToken` → `Index.Lookup`；失败 401；`Disabled` 为 403；成功后用 `auth.WithPrincipal(ctx, p)` 存入 context，下游用 `auth.PrincipalFrom(ctx)` 取出 |
| MW-05 | `RateLimit` | `ratelimit.Limiter.Allow("c:"+clientID, rpm)`；拒绝时 429 `rate_limited`，`Retry-After` 为秒数（向上取整） |
| MW-06 | `ClientIP` | `clientip.Trusted.From(r)` 解析真实 IP（spec/03 §7），`clientip.WithIP` 存入 context（已提供） |
| MW-07 | `IPRateLimit` | 见 §5 OVL-04、OVL-05 |
| MW-08 | `OverloadGuard.Middleware` | 见 §5 OVL-01 ~ OVL-03 |

### 1.2 目录端点

`core.Models(principal)` 返回客户端被允许使用的服务（按 ID 升序），具体格式由入站适配器的 `WriteCatalog` 决定（spec/08 §2.8）。

## 2. 错误码与错误格式

### 2.1 错误码（`internal/apperr`，已提供）

HTTP 状态码由 `apperr.Code.HTTPStatus()` 统一决定；各协议的 `error.type` 由各自入站适配器的 `errorType` 表决定（下表是当前两个适配器的汇总，供对照）：

| Code | HTTP | Anthropic `error.type` | OpenAI `error.type` |
|:--|:-:|:--|:--|
| `invalid_request` | 400 | `invalid_request_error` | `invalid_request_error` |
| `unauthenticated` | 401 | `authentication_error` | `authentication_error` |
| `insufficient_quota` | 402 | `billing_error` | `insufficient_quota` |
| `permission_denied` | 403 | `permission_error` | `permission_error` |
| `not_found` | 404 | `not_found_error` | `not_found_error` |
| `conflict` | 409 | `invalid_request_error` | `invalid_request_error` |
| `request_too_large` | 413 | `request_too_large` | `invalid_request_error` |
| `rate_limited` | 429 | `rate_limit_error` | `rate_limit_error` |
| `internal` | 500 | `api_error` | `server_error` |
| `not_implemented` | 501 | `api_error` | `server_error` |
| `upstream_error` | 502 | `api_error` | `server_error` |
| `upstream_unavailable` | 503 | `overloaded_error` | `server_error` |
| `overloaded` | 503 | `overloaded_error` | `server_error` |
| `timeout` | 504 | `timeout_error` | `server_error` |

新增错误码属于 `apperr` 的变更（第 0 层），需要架构负责人批准，并同步更新所有入站适配器的映射表。

### 2.2 写错误的约定

- 数据面：一律调用本请求所属协议的 `Ingress.WriteError`；它内部用 `apperr.WriteJSON` 写状态码与 `Retry-After`。
- 管理面：`admin.writeError`，格式 `{"error":{"code","message"}}`。
- 非业务错误先经 `apperr.Normalize` 变为 `internal`，消息固定为 `internal error`，不泄露内部错误文本。
- 已经开始写响应体后**禁止**再写错误响应，流式场景改用 `Ingress.StreamError`（spec/08 §2.6、spec/13 §4.4）。

## 3. 令牌桶限流（`internal/ratelimit`，T04）

同一实现用于两处：每客户端限流（键 `"c:<clientID>"`）与鉴权前每 IP 限流（键 `"ip:<addr>"`）。两处使用**不同的 `Limiter` 实例**。

```go
func New(now func() time.Time) *Limiter
func (l *Limiter) Allow(key string, rpm int) (ok bool, retryAfter time.Duration)
```

| 编号 | 行为 |
|:--|:--|
| RL-01 | `rpm <= 0`：永远允许 |
| RL-02 | 令牌桶：容量 `rpm`，每秒补充 `rpm/60` 个（浮点），新客户端的桶初始为满 |
| RL-03 | 每次请求消耗 1 个；不足 1 个时拒绝，`retryAfter = (1 - 当前令牌) / 每秒补充量` 秒 |
| RL-04 | 同一个键的 `rpm` 改变时：容量改为新值，当前令牌数截断到新容量 |
| RL-05 | 并发安全：每个桶一个 mutex；桶表用 `sync.Map` |
| RL-06 | 超过 10 分钟未访问的桶由 `Sweep(now)` 删除；`cmd/gateway` 每 5 分钟调用一次 |

## 4. 管理面 API（`internal/admin`，T12）

- 监听 `server.admin_listen`；所有路径以 `/admin/api/` 开头；请求与响应都是 JSON（`Content-Type: application/json`）。
- 鉴权：ADM-01 ~ ADM-04（05 §4）。
- 错误格式：`{"error":{"code":"not_found","message":"client 42 not found"}}`，HTTP 状态按 §2.1。
- 请求体上限 1 MiB；未知字段返回 400（`json.Decoder.DisallowUnknownFields`）。
- 金额字段一律为 **nano-USD 整数**，字段名以 `_nano_usd` 结尾。时间字段为 RFC 3339 字符串（UTC）。

### 4.1 端点

| 方法 | 路径 | 请求体 | 成功响应 |
|:--|:--|:--|:--|
| `GET` | `/admin/api/health` | — | `200 {"status":"ok","version":"...","uptime_seconds":123}` |
| `POST` | `/admin/api/clients` | `CreateClientRequest` | `201 {"client":ClientView,"token":"uai_live_..."}` |
| `GET` | `/admin/api/clients` | — | `200 {"clients":[ClientView]}` |
| `GET` | `/admin/api/clients/{id}` | — | `200 {"client":ClientView}` |
| `PATCH` | `/admin/api/clients/{id}` | `UpdateClientRequest` | `200 {"client":ClientView}` |
| `POST` | `/admin/api/clients/{id}/rotate-token` | — | `200 {"client":ClientView,"token":"uai_live_..."}` |
| `POST` | `/admin/api/keys` | `CreateKeyRequest` | `201 {"key":KeyView}` |
| `GET` | `/admin/api/keys` | — | `200 {"keys":[KeyView]}` |
| `PATCH` | `/admin/api/keys/{id}` | `UpdateKeyRequest` | `200 {"key":KeyView}` |
| `POST` | `/admin/api/keys/import` | `ImportKeysRequest`（T70） | `200 ImportKeysResponse` |
| `GET` | `/admin/api/usage?client_id=&from=&to=` | — | `200 {"items":[UsageItem]}` |

### 4.2 数据结构（Go 定义在 `internal/admin/types.go`，已提供）

```go
type CreateClientRequest struct {
    Name           string   `json:"name"`             // 必填，1-64 字符
    Role           string   `json:"role"`             // "user"（默认）或 "admin"
    QuotaUnlimited bool     `json:"quota_unlimited"`
    QuotaNanoUSD   int64    `json:"quota_nano_usd"`   // >= 0
    RPMLimit       *int     `json:"rpm_limit"`        // 缺省 60；0 = 不限
    AllowedModels  []string `json:"allowed_models"`   // 每项必须是已配置的模型名
}
type UpdateClientRequest struct { // 全部可选；nil 表示不修改
    Name *string; Role *string; QuotaUnlimited *bool; QuotaNanoUSD *int64
    RPMLimit *int; AllowedModels *[]string; Disabled *bool
}
type ClientView struct {
    ID int64; Name, Role, TokenDisplay string; QuotaUnlimited bool
    QuotaNanoUSD, SpentNanoUSD, ReservedNanoUSD int64; RPMLimit int
    AllowedModels []string; Disabled bool; CreatedAt, UpdatedAt string
}
type CreateKeyRequest struct {
    Pool string; Name string; Secret string; Weight *int; MaxInFlight *int
    ExpiresAt string; Label string // T62：RFC 3339 / 来源标签，均可空
}
type UpdateKeyRequest struct { Name *string; Weight *int; MaxInFlight *int; State *string // State: "ready" | "disabled"
    ExpiresAt json.RawMessage; Label *string } // T62：expires_at 省略 = 不改，null = 清除，字符串 = RFC 3339
type KeyView struct {
    ID int64; Pool, Name, SecretHint, State, StoredState, StateReason string; Weight, MaxInFlight int
    InFlight int; CooldownSeconds int; Breaker string // closed | open | half_open
    ExpiresAt string; Label string // T62：expires_at 不过期时省略
    CreatedAt, UpdatedAt string
}
type UsageItem struct {
    ClientID int64; Model string; Requests int64; Usage map[string]int64; CostNanoUSD int64
}
```

### 4.3 行为

| 编号 | 行为 |
|:--|:--|
| ADM-10 | 创建/修改客户端成功后，同步执行：`auth.Reloader.Reload` 与 `ledger.UpsertAccount`。两者任一失败返回 500，但数据库修改已生效（记录 `Error` 日志） |
| ADM-11 | `ClientView.SpentNanoUSD` / `ReservedNanoUSD` 取自 `ledger.Snapshot` |
| ADM-12 | `rotate-token`：生成新 Token，更新 `token_hash`/`token_display`，旧 Token 立即失效（Reload 之后） |
| ADM-13 | 创建 Key：`pool` 必须在配置中存在（否则 400）；`secret` 非空、≤ 4096 字节；`fingerprint = HashToken(pepper, secret)`；重复返回 409 `conflict`，错误体增加已存在 Key 的 `key_id`（T62，供导入命令更新元数据） |
| ADM-14 | 创建/修改 Key 后，同步执行 `scheduler.LoadKeys` + `Scheduler.SetKeys`（保留同 ID Key 的运行时状态） |
| ADM-15 | `PATCH keys` 的 `state` 只接受 `ready` / `disabled`；设为 `ready` 会清除 `quarantined` 状态与 `state_reason` |
| ADM-16 | 响应中**永远不出现** `secret`、`token_hash`、`wrapped_dek`、`ciphertext` |
| ADM-22 | Key 的过期时间与标签（T62）：`expires_at` 必须是 RFC 3339（不接受 Unix 纪元 0），否则 400；`label` ≤ 64 字节，否则 400。列表中库内状态为 `ready` 且已过期（以调度器的 `KeyStatus.Expired` 为准）的 Key，`state` 显示 `expired`，库中状态放进 `stored_state`（同 HR-19）；停用、隔离的 Key 照常显示原状态；孤立优先于过期（`stored_state` 保留库中状态）。过期不写库 |
| ADM-23 | `POST /admin/api/keys/import`（T70）：请求体 `{"pool", "name_prefix"?, "weight"?, "max_in_flight"?, "update_existing"?, "items": [{"secret", "expires_at"?, "label"?}]}`。1 ≤ `items` ≤ 1000，`name_prefix` ≤ 56 字节（空 = 池名），池、权重、并发上限不合法时整体 400。每条单独校验（`secret`、`expires_at`、`label` 同 ADM-13、ADM-22）、单独写入，结果为 `created` / `updated` / `duplicate` / `invalid` / `failed`（存储错误）之一；名称为 `<前缀>-NNN`，编号接在该池同前缀已有的最大编号之后。已存在（同指纹，含同一批中重复出现）的计为 `duplicate` 并带 `key_id`；`update_existing` 时把条目中**出现的** `expires_at`（空字符串 = 清除）与 `label` 写到已有 Key，计为 `updated`。处理完后只在有 `created` 或 `updated` 时执行一次 ADM-14 的重载。响应 `{"created", "updated", "duplicate", "invalid", "failed", "results": [{"index", "status", "key_id"?, "name"?, "secret_hint"?, "error"?}]}` |
| ADM-24 | 批量导入的响应、错误信息与日志不含密钥明文：只有序号、`secret_hint`（`obs.MaskSecret`）与数量；每次请求记一条 Info（池名与各项数量） |
| ADM-25 | `KeyView` 增加 `quota_used`、`quota_limit`、`quota_reset_at`（RFC 3339）（T63）：池没有额度时三项都省略，`total` 周期省略 `quota_reset_at`。库中为 `ready` 且本期额度用尽的 Key，`state` 显示 `exhausted`，库中状态放进 `stored_state`（已过期时显示 `expired`） |
| ADM-26 | `GET /admin/api/alerts`（T64）：`{"alerts": [{"pool", "kind", "level", "value", "threshold", "since", "message"}], "checked_at"?}`，按池名、种类排序；没有配置告警时 `alerts` 为空数组 |
| ADM-27 | `PATCH /admin/api/keys/{id}` 接受可选 `reason`（T64）：只能与 `state: disabled` 一起使用，去掉首尾空白后 1 ~ 200 字节，否则 400；写入 `state_reason`。缺省仍为 `disabled by admin` |
| ADM-28 | 管理面 MCP 增加只读工具 `gateway_admin_alerts`，返回 ADM-26 的内容（spec/14 AM-02） |
| ADM-29 | `GET /admin/api/keys/stats?pool=&from=&to=`（T65）：`from`、`to` 为 RFC 3339，缺省为最近 24 小时，`from` 必须早于 `to` 且跨度 ≤ 31 天；`pool` 必须是已配置的池；否则 400。响应 `{"keys": [{id, pool, name, requests, usage, cost_nano_usd, attempts_24h, failures_24h, failure_rate_24h, last_failure?: {kind, at}}], "from", "to"}`，按 ID 升序。`requests`、`usage`、`cost_nano_usd` 来自计费事件（随 `from`、`to`）；`*_24h` 与 `last_failure` 来自 KS-03，始终是最近 24 小时 |
| ADM-30 | `GET /admin/api/pools`（T65）：配置中的每个池 `{pool, protocol, keys: {total, available, cooling, exhausted, expired, disabled, quarantined}, expiring_7d, requests_24h, failure_rate_24h, quota?}`。`available` 同 AL-01（含冷却中的），`cooling` 为其中正在冷却的数量再加熔断器打开的数量；其余状态互斥，优先级为停用、隔离、过期、用尽。`requests_24h` 为最近 24 小时的计费事件数；`failure_rate_24h` 为池内 KS-03 的失败数 ÷ 尝试数。`quota`（只对声明了额度的池）：`{unit, limit_per_key, period, used（全池本期已用）, left（可用 Key 的剩余之和）, next_reset?, burn_per_hour（最近 24 小时额度单位的用量 ÷ 24）, runs_out_at?, lasts_until_reset}`；速度为 0 时没有 `runs_out_at`，有重置时刻则 `lasts_until_reset` 为 true；否则预计用完时间晚于（或等于）重置时刻时 `lasts_until_reset` 为 true，早于时给出 `runs_out_at` |
| ADM-31 | 管理面 MCP 增加只读工具 `gateway_admin_pools`（ADM-30）与 `gateway_admin_key_stats {pool?, from?, to?}`（ADM-29）（spec/14 AM-02） |
| ADM-17 | `usage` 查询：`from`/`to` 缺省为「最近 24 小时」；`from >= to` 返回 400 |
| ADM-18 | `POST /admin/api/reload`，仅管理员角色。请求体可选 `{"dry_run": bool}`。响应 `{"mode": "none"\|"hot"\|"restart", "applied": bool, "changes": {"pools": {"added", "removed", "changed"}, "services": {"added", "removed", "changed"}, "settings": [...], "restart_fields": [...]}, "warnings": [...]}`；`settings` 列出变化的其他热重载字段（如 `mcp`、`server.log_level`），`restart_fields` 列出需要重启的字段（如 `limits.max_in_flight_requests`）。状态码：200（none 或 hot 已应用，或 dry-run）、202（restart 已开始）、400（配置错误）、409（重载进行中）、422（构造或预检失败）。错误体使用管理 API 现有的错误格式。 |
| ADM-19 | `GET /admin/api/reload`，仅管理员角色。返回 `{"records": [...]}`，最近 20 次重载，新的在前，每条 `{time, actor_client_id, mode, status, changes, error?}`。 |
| ADM-20 | `GET /admin/api/tools`：`{"tool_mode": mcp.tool_mode, "tools": [{"name", "title"?, "summary", "service"?, "protocol", "read_only"}]}`，按名称升序；`summary` 为描述首行；`service` 为工具固定的服务名（空表示由参数选择）。来源是当前一代的注册表，热重载后立即更新。 |
| ADM-21 | `POST /admin/mcp`：管理面 MCP（spec/14 §6），经过管理面的全部中间件。`GET` / `DELETE` → 405。 |

## 5. 全局过载保护（`internal/httpapi`，T11）

目的：网关暴露到局域网或公网后，防止大量并发请求或超大请求体把内存打满。只作用于模型端点与 `/v1/models`；`/healthz` 与管理面不受影响。

```go
func NewOverloadGuard(maxRequests int, maxBodyBytes, maxSingleBody int64) *OverloadGuard
func (g *OverloadGuard) Middleware(next http.Handler) http.Handler
func (g *OverloadGuard) InFlight() (requests, bodyBytes int64)
func IPRateLimit(l *ratelimit.Limiter, rpm int) func(http.Handler) http.Handler
```

`cmd/gateway` 用 `limits.max_in_flight_requests`、`limits.max_buffered_body_bytes`、`limits.max_request_body_bytes`、`limits.preauth_ip_rpm` 构造它们。

| 编号 | 行为 |
|:--|:--|
| OVL-01 | 每个请求占用 1 个并发名额；当前在途数已达 `maxRequests` 时拒绝 |
| OVL-02 | `buffered` 为 true 时，每个请求按 `n = r.ContentLength` 占用请求体预算；`ContentLength < 0`（未知长度）或 `> maxSingleBody` 时按 `maxSingleBody` 占用；占用后总量会超过 `maxBodyBytes` 时拒绝。`buffered` 为 false（流式上传、无请求体的路由）时不占请求体预算，只占并发名额。两种占用都用 **CAS 循环**，任一失败时已占用的部分必须归还 |
| OVL-03 | 拒绝时返回 503 `overloaded`、`Retry-After: 1`，错误格式用传入的 `ew`，并且**不读取请求体**；放行时在 `next` 返回后（`defer`）归还全部占用。流式响应的占用持续到流结束 |
| OVL-04 | `IPRateLimit`：`rpm <= 0` 直接放行；否则以 `"ip:" + clientip.FromContext(ctx).String()` 为键调用 `Limiter.Allow`，拒绝时 429 `rate_limited` 并带 `Retry-After` |
| OVL-05 | 客户端 IP 为零值（无法解析）时以键 `"ip:unknown"` 计数，不放行也不报错 |
| OVL-06 | 装配层的共享上游 transport 为 `protocol.ReadBody` 提供内部可选的 `ReadBuffered(limit, retain)` 响应体能力，在读取前按本次读缓冲大小申请共享预算、读完归还未使用部分；耗尽返回 503 `overloaded`，不重试、不惩罚 Key。预算值复用 `max_buffered_body_bytes`，但与请求体预算**分别计数**。内核为每次尝试包装共享客户端，通过内部可选能力 `RetainResponseBuffer(release)` 保存释放回调；在尝试收尾时一并释放多步骤调用的所有缓冲结果。Body.Close 或父 context 取消都不能提前归还仍被转译/写入持有的预算。独立使用客户端、没有尝试所有者时以请求 ctx 取消作为回收边界。直接流式 Read 不使用此能力，不按累计传输量占用预算 |

### 5.1 内存估算（写进部署文档，供用户调参）

一个在途请求在网关中大约持有 3 份请求体（原文、解析出的顶层字段、改写后的请求体），因此请求体相关内存约为 `3 × max_buffered_body_bytes`。缓冲响应每份最多 64 MiB，所有尝试持有的原始响应负载另外共享一份 `max_buffered_body_bytes` 预算（默认 512 MiB），不再按并发数乘以 64 MiB。两份预算分别计数，均不等同于精确的堆上限：读取时的切片扩容、JSON 解析、转译后的输出与 MCP 结果会产生额外内存，部署时必须留出空间。流式上传（`BodyStream`）与字节流下载（`ResponseRaw`）边读边写，不按累计传输量扣除缓冲预算。

## 6. 管理 Web UI（T49）

管理页面内嵌在网关二进制中（`go:embed`），挂在管理面监听上，只是管理 API 的一个客户端：权限检查全部仍在 §4 的 API 中完成（ADM-01 ~ ADM-04）。页面是手写的 HTML、CSS 与原生 JavaScript（ES 模块）：不引入依赖，没有构建步骤，不从外部网络加载任何资源。

### 6.1 路由与响应头

| 编号 | 规则 |
|:--|:--|
| UI-01 | `GET /admin/ui/` 返回 `index.html`；`GET /admin/ui/<文件>` 返回内嵌的静态文件；`GET /admin/ui` 以 `301` 重定向到 `/admin/ui/`；内嵌文件中没有的路径返回 `404`；`GET` / `HEAD` 以外的方法返回 `405` 并带 `Allow: GET, HEAD` |
| UI-02 | 静态资源不经过 ADM-01 鉴权，也不计入 ADM-04 的失败次数。静态资源中没有任何数据或凭据 |
| UI-03 | 每个静态响应都带：`Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、`X-Frame-Options: DENY`。`index.html` 加 `Cache-Control: no-store`，其他文件加 `Cache-Control: no-cache` |
| UI-04 | `Content-Type` 按扩展名显式设置（`.html`、`.js`、`.css`、`.svg`），不依赖内容嗅探 |
| UI-05 | 页面中不得有内联脚本、内联样式（`style` 属性）、内联事件属性（`on*=`）、`eval`、`new Function` |
| UI-06 | 内嵌资源总大小不超过 300 KiB |

### 6.2 页面行为

页面包括：登录、概览、客户端、上游 Key、用量。

| 编号 | 规则 |
|:--|:--|
| UI-09 | 管理员 Token 只保存在 `sessionStorage`（关闭标签页即失效），不写 `localStorage`、Cookie 或 URL。每次调用 API 都放在 `Authorization: Bearer` 头中；页面不使用 Cookie，因此不需要 CSRF Token |
| UI-10 | 新 Token（创建客户端、轮换 Token 的响应）只在一个对话框中显示一次，提供复制按钮并提示「关闭后无法再次查看」；不写入任何存储、URL 或控制台 |
| UI-11 | 上游 Key 的 `secret` 只出现在新建表单中，使用 `type="password"`；提交后立即清空，不回显 |
| UI-12 | 来自 API 的字符串一律用 `textContent` / `setAttribute` 写入页面，禁止用 `innerHTML` 拼接数据 |
| UI-13 | 金额：显示时 nano-USD 换算为美元并保留 6 位小数；输入时美元小数用字符串运算转为 nano-USD 整数，不经过浮点数，超过 6 位小数时拒绝提交。绝对值超过 `Number.MAX_SAFE_INTEGER` 的值标注「精度可能丢失」 |
| UI-14 | 时间按浏览器本地时区显示，悬停显示原始 UTC 值；用量查询的时间输入换算为 UTC 后发出 |
| UI-15 | 禁用客户端、轮换 Token、停用 Key 前要求二次确认；API 的错误消息（如 ADM-03 的 409）原样展示 |
| UI-16 | API 返回 401 时清除 `sessionStorage` 中的 Token 并回到登录页；其他错误显示 `error.code`、`error.message` 与响应头 `X-Request-Id` |
| UI-17 | 上游 Key 页每 10 秒自动刷新一次运行时状态；页面不可见（`document.hidden`）时暂停 |
| UI-18 | 界面提供中文与英文，按 `navigator.language` 选择（以 `zh` 开头用中文，否则英文），可在页面中手动切换；手动选择保存在 `sessionStorage` 的 `uaig.lang` 键。页面不使用 `localStorage` |
| UI-19 | 每个 `fetch` 都有 15 秒超时（`AbortController`） |

### 6.3 DNS 重绑定防护

| 编号 | 规则 |
|:--|:--|
| UI-20 | 管理面对**所有**请求（含 `/admin/api/*`、`/admin/metrics`、`/admin/ui/*`）检查 `Host` 头：去掉端口与 IPv6 方括号后，主机名（不区分大小写）必须是 `127.0.0.1`、`::1`、`localhost`，或 `server.admin_hosts`（spec/03）中列出的值，否则返回 `421`，错误码 `invalid_request`，消息 `host not allowed`。该检查在 ADM-04 的 IP 计数之前进行，不计入失败次数 |
| UI-21 | `admin.Deps.HostCheck` 为 false 时不做 UI-20 的检查（库的零值，供单元测试使用）；`cmd/gateway` 总是设为 true，并把 `server.admin_hosts` 传入 `admin.Deps.AdminHosts` |

管理面仍然只监听回环地址（CFG-02）。远程使用时用 SSH 端口转发（`ssh -L 8081:127.0.0.1:8081 <host>`），不要通过反向代理把管理面暴露到公网。

### 6.4 为页面补充的只读 API

| 方法 | 路径 | 成功响应 |
|:--|:--|:--|
| `GET` | `/admin/api/config` | `200 {"pools":[{"name","protocol"}],"models":[{"name","pool"}]}`：均按名称升序。**不含** `base_url`、价格以及任何其他配置字段 |
| `GET` | `/admin/api/health` | 在 §4.1 的字段之外**增加**：`ledger_queue: {"length","capacity"}`、`audit_dropped`（与 spec/15 的对应仪表同源）。取数函数未注入时这两个字段不输出 |

两个端点都按 ADM-01 ~ ADM-04 鉴权。

### 6.5 T57 补充（孤立 Key、配置页）

| 编号 | 规则 |
|:--|:--|
| UI-30 | Key 的 `state` 为 `orphaned` 时显示「孤立」（灰色），原因「池已从配置中删除」，不提供启用 / 禁用按钮；概览把它计入已停用。 |
| UI-33 | Key 页（T62）：表格增加「过期时间」列（本地时间，`title` 为原始值；没有时显示「不过期」）；名称下方显示标签；`expired` 显示「已过期」（灰色），原因「已于 … 过期」，仍提供「停用」按钮；有两种以上标签时页头显示标签筛选；新增与编辑表单增加过期时间（`datetime-local`，本机时区，留空 = 不过期）与标签。 |
| UI-34 | Key 页「批量导入」（T70）：粘贴文本或选择本地文件（≤ 8 MiB），在浏览器中解析为每行一把 Key、JSONL 或 CSV / TSV（自动识别，可手动指定）；JSONL 与 CSV 可选择密钥、过期时间、标签字段与「字段 = 值」过滤；可设置时区（过期时间格式同 `gateway keys import`）、统一标签、密钥前缀、名称前缀，以及「也导入已过期的」「更新已有 Key」。预览只显示数量与无效行的行号和原因，不显示任何密钥。提交时每批 ≤ 500 条且请求体 ≤ 900 KiB，依次调用 ADM-23 并显示进度；完成后显示各项数量与无效、失败行的行号。导入完成或对话框关闭后清空文本与内存中的条目 |
| UI-35 | Key 页（T63）：声明了额度的池增加「本期用量」列（`已用 / 上限` 与进度条，≥ 80% 黄色、用满红色；`title` 为重置时刻）；`exhausted` 显示「额度用尽」（黄色），原因「{time} 重置后恢复」，`total` 周期为「额度已用完，不会重置」。概览把 `exhausted` 计入冷却中、`expired` 计入已停用 |
| UI-36 | 概览页（T64）：有池级告警时在最上面显示「Key 池告警」卡片（`critical` 红色、`warning` 黄色；池名与种类、数值与阈值、开始时间）；没有告警时不显示这一块 |
| UI-37 | Key 页（T65）：与 Key 列表一起（每 10 秒）请求 ADM-29 与 ADM-30，失败时只隐藏以下内容。每个池卡片标题下一行摘要：「可用 a / n」；有额度时加「剩余 N 单位」「每小时约 N 单位」（速度 > 0 时）以及「预计 {时间} 用完」（黄色）或「能撑到 {时间} 重置」或「按当前用法不会用完」；有 7 天内过期的 Key 时加「7 天内过期 N 把」；没有可用 Key 时整行红色。表格增加「24 小时」列：`请求数 · 失败率`（失败率 ≥ 20% 黄色、≥ 50% 红色；没有尝试时为「—」），`title` 列出尝试数、各类失败次数与最近一次失败 |
| UI-31 | 「配置」页：按钮「预览重载」（`dry_run: true`）与「应用重载」（二次确认）；显示模式、增删改的池与服务、变化的设置、需要重启的字段、警告；收到 202 时显示「正在重启」，每秒查询 `GET reload` 直到该记录不再是 `restarting`（最多 2 分钟；重启期间的请求失败视为尚未就绪）；错误按 UI-16 显示。下方是最近 20 次重载记录。 |
| UI-32 | 「配置」页的服务与工具：按 `GET config` 列出池与服务，按 `GET tools` 列出工具（名称、所属服务、只读与否、说明首行）与当前的 `tool_mode`。全部用 `textContent` 写入（UI-12）。 |
