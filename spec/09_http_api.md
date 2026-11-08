# 09 HTTP 接口（Data Plane & Admin API）

代码位置：`internal/httpapi`（错误格式与中间件已提供；路由与 handler 装配 T11）、`internal/ratelimit`（T04）、`internal/admin`（T12）。

## 1. 数据面端点

| 方法 | 路径 | 鉴权 | 处理者 |
|:--|:--|:--|:--|
| `POST` | `/v1/messages` | 是 | `proxy.Handler`，入站协议 `anthropic` |
| `POST` | `/v1/chat/completions` | 是 | `proxy.Handler`，入站协议 `openai_chat` |
| `GET` | `/v1/models` | 是 | §1.2 |
| `GET` | `/healthz` | 否 | 返回 `200 {"status":"ok"}` |

其他路径返回 404，错误格式按 §2.3（OpenAI 格式）。

### 1.1 中间件顺序

```
RequestID → AccessLog → Recover → [模型端点: Authenticate → RateLimit] → handler
```

| 编号 | 中间件 | 行为 |
|:--|:--|:--|
| MW-01 | `RequestID` | 生成 `req_` + 24 位小写 base32 随机串，存入 context，写响应头 `X-Request-Id`。**忽略**客户端传来的同名头 |
| MW-02 | `AccessLog` | 请求结束后输出一条 `Info` 日志：`request_id, method, path, status, latency_ms, client_id`（未鉴权为 0）。不记录请求体与头部 |
| MW-03 | `Recover` | 捕获 panic：`Error` 日志（含堆栈）；若尚未写响应头则返回 500 `internal` |
| MW-04 | `Authenticate` | `auth.ExtractCredential` → `HashToken` → `Index.Lookup`；失败 401；`Disabled` 为 403；成功后用 `auth.WithPrincipal(ctx, p)` 存入 context，下游用 `auth.PrincipalFrom(ctx)` 取出。请求 ID 用 `obs.WithRequestID` / `obs.RequestIDFrom` |
| MW-05 | `RateLimit` | `ratelimit.Limiter.Allow(clientID, rpm)`；拒绝时 429 `rate_limited`，`Retry-After` 为秒数（向上取整） |

错误格式由**路径决定**：`/v1/messages` 用 Anthropic 格式，其余用 OpenAI 格式（MW-04/05 也遵守这一点）。

### 1.2 `GET /v1/models`

只列出客户端被允许使用的模型（`Principal.ModelAllowed`），按 `name` 升序。

- 请求带 `anthropic-version` 头 → Anthropic 格式：

  ```json
  {"data":[{"type":"model","id":"claude-sonnet-4-5","display_name":"claude-sonnet-4-5","created_at":"1970-01-01T00:00:00Z"}],
   "has_more":false,"first_id":"claude-sonnet-4-5","last_id":"claude-sonnet-4-5"}
  ```
  列表为空时 `first_id`、`last_id` 为 `null`。

- 否则 → OpenAI 格式：

  ```json
  {"object":"list","data":[{"id":"claude-sonnet-4-5","object":"model","created":0,"owned_by":"universal-ai-gateway"}]}
  ```

## 2. 错误码与错误格式

### 2.1 错误码（`internal/apperr`，已提供）

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

### 2.2 Anthropic 格式

```json
{"type":"error","error":{"type":"rate_limit_error","message":"client rate limit exceeded"},"request_id":"req_..."}
```

### 2.3 OpenAI 格式

```json
{"error":{"message":"client rate limit exceeded","type":"rate_limit_error","param":null,"code":"rate_limited"}}
```

### 2.4 写错误的统一入口（已提供）

```go
// internal/apperr/http.go（放在第 0 层，httpapi、proxy、admin 都可以使用）
func WriteHTTP(w http.ResponseWriter, format apperr.Format, requestID string, err error)
// format 取 apperr.FormatAnthropic / apperr.FormatOpenAI / apperr.FormatAdmin；
// httpapi.FormatForPath(path) 按路径给出数据面应使用的格式。
```

- `err` 不是 `*apperr.Error` 时按 `internal` 处理，消息固定为 `internal error`（不泄露内部错误文本）。
- `RetryAfter > 0` 时写 `Retry-After` 头（秒，向上取整）。
- 已经开始写响应体后**禁止**再调用 `WriteError`（流式场景改用 08 §6.4 的错误事件）。

## 3. 每客户端限流（`internal/ratelimit`，T04）

```go
func New(now func() time.Time) *Limiter
func (l *Limiter) Allow(clientID int64, rpm int) (ok bool, retryAfter time.Duration)
```

| 编号 | 行为 |
|:--|:--|
| RL-01 | `rpm <= 0`：永远允许 |
| RL-02 | 令牌桶：容量 `rpm`，每秒补充 `rpm/60` 个（浮点），新客户端的桶初始为满 |
| RL-03 | 每次请求消耗 1 个；不足 1 个时拒绝，`retryAfter = (1 - 当前令牌) / 每秒补充量` 秒 |
| RL-04 | 同一客户端的 `rpm` 改变时：容量改为新值，当前令牌数截断到新容量 |
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
    ClientID int64; Model string; Requests, InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens, CostNanoUSD int64
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
