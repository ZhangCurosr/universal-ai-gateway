# 03 配置与进程（Config & Process）

代码位置：`internal/config`（已提供参考实现）、`cmd/gateway/main.go`。

## 1. 命令行

```
gateway serve   --config <path>      启动服务（默认 ./gateway.yaml）
gateway version                      打印版本号
gateway check-config --config <path> 只加载并校验配置，成功输出 "config ok" 并以 0 退出，失败以 1 退出
```

- 未知子命令：打印用法到 stderr，退出码 2。
- 所有日志输出到 **stderr**（JSON 格式，`slog.NewJSONHandler`）。stdout 只用于 `version` 与 `check-config` 的结果。

## 2. 配置文件结构（YAML）

完整示例见仓库根目录 `gateway.example.yaml`。字段定义以 `internal/config/config.go` 为准。

```yaml
server:
  data_listen: "0.0.0.0:8080"      # 数据面监听地址
  admin_listen: "127.0.0.1:8081"   # 管理面监听地址
  data_dir: "./data"               # SQLite 与锁文件所在目录
  master_key_file: "./secrets/master.key"
  shutdown_grace: 30s              # 优雅关闭等待在途请求的最长时间
  log_level: info                  # debug | info | warn | error
  read_header_timeout: 10s         # §6：读完请求头的最长时间（防慢速攻击）
  read_timeout: 60s                # §6：读完整个请求（头 + 体）的最长时间
  idle_timeout: 120s               # §6：keep-alive 空闲连接保留时间
  max_header_bytes: 65536          # §6：请求头总大小上限
  trusted_proxies: []              # §7：可信反向代理 CIDR，例如 ["127.0.0.1/32", "::1/128"]

limits:
  max_request_body_bytes: 33554432 # 32 MiB，单个请求体上限
  default_max_output_tokens: 8192  # 请求未给 max_tokens 时用于预留估算
  max_in_flight_requests: 256      # spec/09 §5：数据面模型请求的全局并发上限
  max_buffered_body_bytes: 536870912 # spec/09 §5：所有在途请求体合计的内存预算（512 MiB）
  preauth_ip_rpm: 600              # spec/09 §5：鉴权之前每个来源 IP 每分钟请求数；0 = 关闭

ledger:
  flush_interval: 250ms
  event_queue_capacity: 4096
  admission_high_watermark: 0.9    # 队列占用 >= 90% 时拒绝新请求

audit:
  queue_capacity: 8192
  batch_size: 256
  flush_interval: 250ms
  retention_days: 30

pools:
  - name: anthropic-main
    protocol: anthropic            # anthropic | openai_chat
    base_url: "https://api.anthropic.com"
    strategy: least_inflight       # least_inflight | weighted_round_robin
    affinity_ttl: 5m               # 应与上游 Prompt Cache TTL 一致
    max_attempts: 3                # 含首次尝试
    timeouts:
      connect: 10s
      response_header_base: 60s    # 等待上游响应头的基础超时
      response_header_per_100k_input_tokens: 30s
      stream_idle: 120s            # 流式两次事件之间的最长间隔
      non_stream_total: 10m        # 非流式请求的整体超时
    cooldown:
      default: 30s                 # 429 且无 Retry-After 时的冷却时长
      max: 10m
    breaker:
      window: 60s
      min_requests: 10
      failure_ratio: 0.5
      open_duration: 30s
      max_open_duration: 5m

models:
  - name: "claude-sonnet-4-5"      # 客户端请求中使用的模型名（唯一）
    pool: anthropic-main
    upstream_model: "claude-sonnet-4-5"   # 发往上游的模型名
    max_output_tokens: 64000
    price:                          # 单位：USD / 百万 token
      input: 3.0
      output: 15.0
      cache_read: 0.30
      cache_write: 3.75
```

## 3. 默认值（`config.Default()`）

字段缺省时取下表值。实现：先构造 `Default()`，再把 YAML 解析到其上（YAML 中出现的字段覆盖默认值）。

| 字段 | 默认值 |
|:--|:--|
| `server.data_listen` | `0.0.0.0:8080` |
| `server.admin_listen` | `127.0.0.1:8081` |
| `server.data_dir` | `./data` |
| `server.master_key_file` | `./secrets/master.key` |
| `server.shutdown_grace` | `30s` |
| `server.log_level` | `info` |
| `server.read_header_timeout` / `read_timeout` / `idle_timeout` | `10s` / `60s` / `120s` |
| `server.max_header_bytes` | `65536` |
| `server.trusted_proxies` | `[]` |
| `limits.max_request_body_bytes` | `33554432` |
| `limits.default_max_output_tokens` | `8192` |
| `limits.max_in_flight_requests` | `256` |
| `limits.max_buffered_body_bytes` | `536870912` |
| `limits.preauth_ip_rpm` | `600` |
| `ledger.flush_interval` | `250ms` |
| `ledger.event_queue_capacity` | `4096` |
| `ledger.admission_high_watermark` | `0.9` |
| `audit.queue_capacity` | `8192` |
| `audit.batch_size` | `256` |
| `audit.flush_interval` | `250ms` |
| `audit.retention_days` | `30` |
| 池内缺省字段 | `strategy: least_inflight`、`affinity_ttl: 5m`、`max_attempts: 3`、`connect: 10s`、`response_header_base: 60s`、`response_header_per_100k_input_tokens: 30s`、`stream_idle: 120s`、`non_stream_total: 10m`、`cooldown.default: 30s`、`cooldown.max: 10m`、`breaker` 同上例 |

池级默认值由 `Config.Normalize()` 填充（解析后调用）。

## 4. 校验规则（`Config.Validate()`）

`Validate` 返回第一个错误即可；错误消息格式 `config: <字段路径>: <原因>`，例如 `config: pools[1].name: duplicate "anthropic-main"`。

| 编号 | 规则 |
|:--|:--|
| CFG-01 | `data_listen`、`admin_listen` 必须能被 `net.SplitHostPort` 解析 |
| CFG-02 | `admin_listen` 的主机不是回环地址（`127.0.0.1`、`::1`、`localhost`）时报错：M1 不支持管理面对外监听 |
| CFG-03 | `master_key_file` 的绝对路径**不得**位于 `data_dir` 的绝对路径之下（防止数据目录整体备份时密钥一起泄露） |
| CFG-04 | `pools[].name` 非空且唯一；`protocol` 只能是 `anthropic` / `openai_chat`；`base_url` 必须是 `http`/`https` 的绝对 URL |
| CFG-05 | `strategy` 只能是 `least_inflight` / `weighted_round_robin`；`max_attempts` 范围 1–5 |
| CFG-06 | `models[].name` 非空且唯一；`pool` 必须引用已存在的池；`upstream_model` 非空 |
| CFG-07 | 价格各项 `>= 0`；`max_output_tokens > 0` |
| CFG-08 | `ledger.admission_high_watermark` 在 `(0, 1]`；所有容量、批量大小 `> 0`；所有时长 `> 0` |
| CFG-09 | 至少有 1 个池、1 个模型 |
| CFG-10 | `read_header_timeout`、`read_timeout`、`idle_timeout`、`max_header_bytes` 都 `> 0`，且 `read_header_timeout <= read_timeout` |
| CFG-11 | `trusted_proxies` 每一项都必须是 CIDR（`netip.ParsePrefix` 可解析）；单个 IP 要写成 `/32` 或 `/128` |
| CFG-12 | `max_in_flight_requests > 0`、`preauth_ip_rpm >= 0`、`max_buffered_body_bytes >= max_request_body_bytes` |

## 5. 启动与关闭顺序（`cmd/gateway`）

**启动**（任一步失败：记录 `Error` 日志并以退出码 1 退出）：

1. 解析参数 → `config.Load(path)`（内部 Default → YAML → Normalize → Validate）。
2. 初始化 logger（`obs.NewLogger(level)`）。
3. 创建 `data_dir`（`0700`），获取目录锁 `data_dir/gateway.lock`（见 04 §1.3）。已被占用则报错退出。
4. `crypto.LoadOrCreateMasterKey(master_key_file)` → `crypto.NewKeyring(master, 1)`。
5. `sqlite.Open(ctx, data_dir/gateway.db)`（含迁移）。
6. 管理员引导：若不存在 `role=admin` 的客户端，创建一个并把 Token **只打印一次**到 stderr（见 05 §5）。
7. 从存储加载：客户端 → `auth.Index`；用量汇总 → `ledger`；上游 Key（解密）→ `scheduler`。
8. 创建上游 `http.Client`（共享 `http.Transport`，参数见 08 §8）。
9. 启动后台 worker：`ledger.Flusher.Run`、`audit.Writer.Run`、`audit` 过期清理（每小时）。
10. 启动管理面与数据面 `http.Server`。日志输出两个监听地址。

**关闭**（收到 `SIGINT` / `SIGTERM`）：

1. 数据面 `Server.Shutdown(ctx)`，`ctx` 超时为 `shutdown_grace`：停止接受新连接，等待在途请求结束。
2. 管理面 `Server.Shutdown`。
3. 取消 worker 的 `ctx`，等待 `ledger.Flusher` 与 `audit.Writer` 完成最后一次刷盘（它们的 `Run` 返回）。
4. 关闭数据库、释放目录锁，退出码 0。

## 6. HTTP 服务端参数（`cmd/gateway` 创建 `http.Server` 时使用，任务卡 T14）

数据面与管理面的 `http.Server` 都必须设置：

```go
&http.Server{
    Addr:              addr,
    Handler:           handler,
    ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
    ReadTimeout:       cfg.Server.ReadTimeout,
    WriteTimeout:      0, // 必须为 0：流式响应可能持续数分钟，写超时会把它截断；流式超时由 spec/08 §7 控制
    IdleTimeout:       cfg.Server.IdleTimeout,
    MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
    ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
}
```

| 编号 | 规则 |
|:--|:--|
| SRV-01 | **禁止**使用 `http.ListenAndServe` 或零值 `http.Server`（它们没有任何超时，会被慢速攻击耗尽连接） |
| SRV-02 | 先 `net.Listen` 再 `Server.Serve(ln)`，以便测试中用 `127.0.0.1:0` 并通过 `ln.Addr()` 拿到实际端口 |
| SRV-03 | `ReadTimeout` 覆盖读取请求体的全过程，60 秒足以在 5 Mbps 链路上读完 32 MiB；调大 `max_request_body_bytes` 时应同步调大它 |

## 7. 客户端 IP 与可信代理（`internal/clientip`，已提供参考实现）

经过反向代理（Caddy、nginx、云负载均衡）后，`r.RemoteAddr` 恒为代理的地址。网关通过 `trusted_proxies` 判断是否信任 `X-Forwarded-For`：

```go
func clientip.ParseTrusted(cidrs []string) (clientip.Trusted, error)
func (t clientip.Trusted) From(r *http.Request) netip.Addr
```

| 编号 | 规则 |
|:--|:--|
| CIP-01 | `RemoteAddr` 不在可信列表中：直接返回 `RemoteAddr` 的 IP，**完全忽略** `X-Forwarded-For`（否则任何人都能伪造来源 IP） |
| CIP-02 | `RemoteAddr` 在可信列表中：把所有 `X-Forwarded-For` 头按逗号拆开、去空格，**从右往左**找第一个不在可信列表中的合法 IP 并返回 |
| CIP-03 | CIP-02 中所有条目都可信或都无法解析：返回最左边的合法 IP；一个合法 IP 都没有则返回 `RemoteAddr` 的 IP |
| CIP-04 | IPv4 映射的 IPv6 地址（`::ffff:1.2.3.4`）统一转换为 IPv4 后再比较与返回 |
| CIP-05 | 不读取 `X-Real-IP`、`Forwarded` 等其他头（只支持一种来源，避免多种头互相矛盾） |

客户端 IP 用于：鉴权前的按 IP 限流（spec/09 §5）、管理面鉴权失败封禁（ADM-04）、访问日志（`client_ip` 字段）。
