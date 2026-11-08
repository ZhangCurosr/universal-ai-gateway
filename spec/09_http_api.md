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
}
type UpdateKeyRequest struct { Name *string; Weight *int; MaxInFlight *int; State *string } // State: "ready" | "disabled"
type KeyView struct {
    ID int64; Pool, Name, SecretHint, State, StateReason string; Weight, MaxInFlight int
    InFlight int; CooldownSeconds int; Breaker string // closed | open | half_open
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
| ADM-13 | 创建 Key：`pool` 必须在配置中存在（否则 400）；`secret` 非空、≤ 4096 字节；`fingerprint = HashToken(pepper, secret)`；重复返回 409 `conflict` |
| ADM-14 | 创建/修改 Key 后，同步执行 `scheduler.LoadKeys` + `Scheduler.SetKeys`（保留同 ID Key 的运行时状态） |
| ADM-15 | `PATCH keys` 的 `state` 只接受 `ready` / `disabled`；设为 `ready` 会清除 `quarantined` 状态与 `state_reason` |
| ADM-16 | 响应中**永远不出现** `secret`、`token_hash`、`wrapped_dek`、`ciphertext` |
| ADM-17 | `usage` 查询：`from`/`to` 缺省为「最近 24 小时」；`from >= to` 返回 400 |

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

### 5.1 内存估算（写进部署文档，供用户调参）

一个在途请求在网关中大约持有 3 份请求体（原文、解析出的顶层字段、改写后的请求体），因此最坏情况下请求体相关内存约为 `3 × max_buffered_body_bytes`。默认 512 MiB 预算对应约 1.5 GiB 峰值；内存较小的机器应调小 `max_buffered_body_bytes` 与 `max_request_body_bytes`。缓冲响应另外最多缓冲 64 MiB / 请求（spec/13 §6）。流式上传（`BodyStream`）与字节流下载（`ResponseRaw`）边读边写，每个请求只占 32 KiB 级别的缓冲。
