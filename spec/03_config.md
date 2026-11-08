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

limits:
  max_request_body_bytes: 33554432 # 32 MiB
  default_max_output_tokens: 8192  # 请求未给 max_tokens 时用于预留估算

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
| `limits.max_request_body_bytes` | `33554432` |
| `limits.default_max_output_tokens` | `8192` |
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
