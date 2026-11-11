# 03 配置与进程（Config & Process）

代码位置：`internal/config`（已提供参考实现）、`cmd/gateway/main.go`。

## 1. 命令行

```
gateway serve   --config <path>      启动服务（默认 ./gateway.yaml）
gateway version                      打印版本号
gateway check-config --config <path> 只加载并校验配置，成功输出 "config ok" 并以 0 退出，失败以 1 退出
gateway reload [--dry-run] [--json]  让运行中的网关重新读取配置文件（§8，任务卡 T55）
```

| 编号 | 规则 |
|:--|:--|
| HR-01 | `gateway serve` 记住 `--config` 的路径；重载总是重新读取这个文件。 |
| HR-02 | `gateway reload [--dry-run] [--json] [--timeout 2m]`：连接信息来自环境变量 `UAIG_ADMIN_URL`（缺省 `http://127.0.0.1:8081`）与 `UAIG_ADMIN_TOKEN`（必填），不提供 `--token` 参数（同 CLI-05）。调用 `POST /admin/api/reload`。收到 202 时轮询 `GET /admin/api/reload`，直到最新一条记录不再是 `restarting`，或者超时。成功退出码 0；校验或预检失败退出码 1，把错误消息写到 stderr；`--json` 输出完整响应。 |

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
  read_timeout: 60s                # §6：读完整个请求（头 + 体）的最长时间；接入大文件上传 API 时要按带宽调大
  idle_timeout: 120s               # §6：keep-alive 空闲连接保留时间
  max_header_bytes: 65536          # §6：请求头总大小上限
  trusted_proxies: []              # §7：可信反向代理 CIDR，例如 ["127.0.0.1/32", "::1/128"]
  admin_hosts: []                  # spec/09 UI-20：管理面额外允许的 Host 主机名（本机地址总是允许）

limits:
  max_request_body_bytes: 33554432 # 32 MiB，缓冲请求体（BodyBuffered，JSON 等）上限
  max_stream_body_bytes: 536870912 # 512 MiB，流式请求体（BodyStream，文件上传）上限；不占内存预算
  max_in_flight_requests: 256      # spec/09 §5：数据面模型请求的全局并发上限
  max_buffered_body_bytes: 536870912 # spec/09 §5：所有在途请求体合计的内存预算（512 MiB）
  preauth_ip_rpm: 600              # spec/09 §5：鉴权之前每个来源 IP 每分钟请求数；0 = 关闭

mcp:                               # spec/14：/mcp 端点
  disabled: false                  # true = 不挂载 /mcp
  max_result_bytes: 4194304        # 单次工具调用的端点响应上限（4 MiB）
  tool_mode: auto                  # auto | direct | catalog（spec/14 §2.5）
  auto_threshold: 8                # auto：客户端可见工具数不超过它时直接列出，否则用目录
  pinned_tools: []                 # 目录模式下仍然直接列出的工具名

ledger:
  flush_interval: 250ms
  event_queue_capacity: 4096
  admission_high_watermark: 0.9    # 队列占用 >= 90% 时拒绝新请求

audit:
  queue_capacity: 8192
  batch_size: 256
  flush_interval: 250ms
  retention_days: 30               # 保留天数，必须 >= 1（CFG-14）

translation:
  unknown_fields: reject           # reject | drop，见 spec/10 §1 第 5 条（CFG-15）

pools:
  - name: anthropic-main
    protocol: anthropic            # 已注册的适配器协议名：anthropic | openai_chat | ……
    base_url: "https://api.anthropic.com"
    strategy: least_inflight       # least_inflight | weighted_round_robin
    affinity_ttl: 5m               # 应与上游 Prompt Cache TTL 一致
    pin_ttl: 24h                   # 异步任务 ID 固定到创建它的 Key 的时长（spec/07 §7.1）
    max_attempts: 3                # 含首次尝试
    timeouts:
      connect: 10s
      response_header_base: 60s    # 等待上游响应的基础超时（异步任务「同步封装」的池要配成分钟级）
      response_header_per_mib: 90s # 请求体每 MiB 追加的等待时长
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

models:                           # 客户端可见的「服务」：大模型、搜索、文档解析……
  - name: "claude-sonnet-4-5"      # 客户端请求中使用的服务名（唯一）
    pool: anthropic-main
    upstream_model: "claude-sonnet-4-5"   # 发往上游的服务名；缺省 = name
    price:                          # 计量单位 → "USD[/PER]"（spec/06 PRC-05）
      input_tokens: "3/1M"
      output_tokens: "15/1M"
      cache_read_tokens: "0.30/1M"
      cache_write_tokens: "3.75/1M"
    reserve:                        # 预留用量的上限兼默认值（spec/06 PRC-03）
      output_tokens: 64000

  - name: "tavily-search"          # 非大模型服务的写法（需要 tavily 适配器，spec/08 §8.1）
    pool: tavily
    price: { credits: "0.008" }
    reserve: { credits: 2 }

  - name: "bivia"                 # rest 服务可以声明接口，交给 AI 当作工具调用（CFG-17，spec/08 §8.3.5）
    pool: bivia-pool
    price: { requests: "0.001" }
    reserve: { requests: 1 }
    tools:
      - name: bivia_boolean
        description: "对一段情境回答是或否的问题，返回答案与概率"
        method: POST
        path: /boolean
        idempotent: true
        read_only: true
        input_schema:
          type: object
          properties:
            state:    { type: string, description: "需要判断的情境" }
            question: { type: string, description: "是非问题" }
          required: [state, question]
```

`price` 必须为该池协议的出站适配器声明的**每个**单位定价（免费写 `"0"`），否则启动失败（CORE-05）。各协议的单位见 spec/08（anthropic：`input_tokens`、`output_tokens`、`cache_read_tokens`、`cache_write_tokens`；openai_chat：前三个）。

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
| `server.admin_hosts` | `[]` |
| `limits.max_request_body_bytes` | `33554432` |
| `limits.max_stream_body_bytes` | `536870912` |
| `limits.max_in_flight_requests` | `256` |
| `limits.max_buffered_body_bytes` | `536870912` |
| `limits.preauth_ip_rpm` | `600` |
| `mcp.disabled` / `mcp.max_result_bytes` | `false` / `4194304` |
| `mcp.tool_mode` / `mcp.auto_threshold` / `mcp.pinned_tools` | `auto` / `8` / `[]` |
| `ledger.flush_interval` | `250ms` |
| `ledger.event_queue_capacity` | `4096` |
| `ledger.admission_high_watermark` | `0.9` |
| `audit.queue_capacity` | `8192` |
| `audit.batch_size` | `256` |
| `audit.flush_interval` | `250ms` |
| `audit.retention_days` | `30` |
| `translation.unknown_fields` | `reject` |
| 池内缺省字段 | `strategy: least_inflight`、`affinity_ttl: 5m`、`max_attempts: 3`、`pin_ttl: 24h`、`connect: 10s`、`response_header_base: 60s`、`response_header_per_mib: 90s`、`stream_idle: 120s`、`non_stream_total: 10m`、`cooldown.default: 30s`、`cooldown.max: 10m`、`breaker` 同上例 |

池级默认值与 `models[].upstream_model`（缺省 = `name`）由 `Config.Normalize()` 填充（解析后调用）。

## 4. 校验规则（`Config.Validate()`）

`Validate` 返回第一个错误即可；错误消息格式 `config: <字段路径>: <原因>`，例如 `config: pools[1].name: duplicate "anthropic-main"`。

| 编号 | 规则 |
|:--|:--|
| CFG-01 | `data_listen`、`admin_listen` 必须能被 `net.SplitHostPort` 解析 |
| CFG-02 | `admin_listen` 的主机不是回环地址（`127.0.0.1`、`::1`、`localhost`）时报错：M1 不支持管理面对外监听 |
| CFG-03 | `master_key_file` 的绝对路径**不得**位于 `data_dir` 的绝对路径之下（防止数据目录整体备份时密钥一起泄露） |
| CFG-04 | `pools[].name` 非空且唯一；`protocol` 必须匹配 `[a-z][a-z0-9_]*`（是否有对应适配器由 `core.New` 检查，CORE-01）；`base_url` 必须是 `http`/`https` 的绝对 URL |
| CFG-05 | `strategy` 只能是 `least_inflight` / `weighted_round_robin`；`max_attempts` 范围 1–5 |
| CFG-06 | `models[].name` 非空且唯一；`pool` 必须引用已存在的池；`upstream_model` 非空（Normalize 之后） |
| CFG-07 | `price` 的每个键满足 `pricing.ValidUnit`，每个值能被 `pricing.ParseRate` 解析；`reserve` 的每个键合法、值 `>= 0`。价目表是否覆盖适配器的全部单位由 `core.New` 检查（CORE-05） |
| CFG-08 | `ledger.admission_high_watermark` 在 `(0, 1]`；所有容量、批量大小 `> 0`；所有时长 `> 0` |
| CFG-09 | 至少有 1 个池、1 个模型 |
| CFG-10 | `read_header_timeout`、`read_timeout`、`idle_timeout`、`max_header_bytes` 都 `> 0`，且 `read_header_timeout <= read_timeout` |
| CFG-11 | `trusted_proxies` 每一项都必须是 CIDR（`netip.ParsePrefix` 可解析）；单个 IP 要写成 `/32` 或 `/128` |
| CFG-13 | `mcp.max_result_bytes > 0` |
| CFG-12 | `max_in_flight_requests > 0`、`preauth_ip_rpm >= 0`、`max_buffered_body_bytes >= max_request_body_bytes`、`max_stream_body_bytes > 0` |
| CFG-14 | `audit.retention_days >= 1`，否则网关拒绝启动（配置校验 fail-fast） |
| CFG-15 | `translation.unknown_fields` 只能是 `reject` 或 `drop` |
| CFG-16 | `server.admin_hosts` 每一项都必须是不带端口的主机名或 IP 地址：非空；IPv6 地址可带或不带方括号，其他取值只含字母、数字、`.`、`-`，不含通配符；比较时不区分大小写（spec/09 UI-20） |
| CFG-17 | `models[].tools` 可选，是接口声明的列表。每项字段：`name`（匹配 `^[A-Za-z0-9_.-]{1,64}$`，在整份配置内唯一）、`title`（可选）、`description`（非空）、`method`（`GET`、`DELETE`、`POST`、`PUT`、`PATCH` 之一，不区分大小写，规范化为大写）、`path`（以 `/` 开头；不含 `?`、`#`、`\`、NUL；按 `/` 拆段后没有 `.` 或 `..` 段；`{name}` 占位符的名字匹配 `^[A-Za-z_][A-Za-z0-9_]*$`，同一名字只出现一次，不允许 `{name...}`）、`input_schema`（可选；缺省为 `{"type":"object","properties":{},"additionalProperties":false}`；给出时必须是映射，`type` 为 `object`，键名都是字符串；每个占位符必须出现在 `properties` 与 `required` 中）、`idempotent`（可选，布尔，只允许用于 `POST` / `PATCH`）、`read_only`（可选，布尔；缺省 `GET` 为 true，其他为 false）。任一项不满足 → 拒绝启动，错误消息含 `models[i].tools[j]` 位置 |
| CFG-18 | `models[].tools` 非空时，该服务所在池的协议必须支持接口声明（目前只有 `rest`），否则拒绝启动。由装配代码（`cmd/gateway`）检查，配置包不判断协议名 |
| CFG-19 | `mcp.tool_mode` 取 `auto`（缺省）、`direct`、`catalog` 之一；`mcp.auto_threshold` 缺省 8，必须 `>= 1`；`mcp.pinned_tools` 可选，每一项必须是注册表中存在的工具名，且不能是目录工具名（CAT-01），否则拒绝启动（工具名的存在性在注册表封存后由装配代码检查） |

## 5. 启动与关闭顺序（`cmd/gateway`）

**启动**（任一步失败：记录 `Error` 日志并以退出码 1 退出）：

1. 解析参数 → `config.Load(path)`（内部 Default → YAML → Normalize → Validate）。
2. 初始化 logger（`obs.NewLogger(level)`）。
3. 创建 `data_dir`（`0700`），获取目录锁 `data_dir/gateway.lock`（见 04 §1.3）。已被占用则报错退出。
4. `crypto.LoadOrCreateMasterKey(master_key_file)` → `crypto.NewKeyring(master, 1)`。
5. `sqlite.Open(ctx, data_dir/gateway.db)`（含迁移）。
6. 管理员引导：若不存在 `role=admin` 的客户端，创建一个并把 Token **只打印一次**到 stderr（见 05 §5）。
7. 从存储加载：客户端 → `auth.Index`；用量汇总 → `ledger`；上游 Key（解密）→ `scheduler`。
8. 创建上游 `http.Client`（共享 `http.Transport`，参数见 13 §6）。
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
    WriteTimeout:      0, // 必须为 0：流式响应可能持续数分钟，写超时会把它截断；流式超时由 spec/13 §5 控制
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

## 8. 配置重载（任务卡 T55）

全部配置都可以在不退出进程的情况下重新加载，分两级，由网关比较新旧配置后自动选择：

| 级别 | 字段 | 做法 |
|:--|:--|:--|
| 热重载 | `pools`、`models`、`mcp`、`translation`、`server.log_level`、`server.trusted_proxies`、`server.admin_hosts` | 重建注册表、内核、MCP 服务端与数据面路由，原子替换；账本、调度器、存储、审计、限流器、并发计数器沿用。在途请求在旧内核上完成 |
| 进程内重启 | 其他全部字段（`server` 的其余字段、`limits`、`ledger`、`audit`） | 先预检；通过后按 §5 的关闭顺序排空并刷盘，再按启动顺序用新配置启动。监听地址不变时沿用同一个监听套接字 |

`master_key_file` 只在新文件中的密钥与当前相同，或 `data_dir` 同时变化时允许变更（上游 Key 的加密与 Token 哈希都依赖主密钥）。进程内重启后新实例启动失败时，用旧配置回滚；回滚也失败才以退出码 1 退出。

| 编号 | 规则 |
|:--|:--|
| HR-10 | 同一时间只有一次重载；进行中再收到请求 → 409 `reload in progress`。 |
| HR-11 | 读取并解析配置（`config.Parse`：Default → YAML → Normalize → Validate）。失败 → 400，消息为配置错误原文。旧配置继续生效。 |
| HR-12 | 比较新旧配置得到 `changes`。`server.log_level`、`server.trusted_proxies`、`server.admin_hosts`、`pools`、`models`、`mcp`、`translation` 以外的任何字段不同 → `mode = "restart"`；否则只要有差异 → `mode = "hot"`；完全相同 → `mode = "none"`，什么都不做，返回 200。 |
| HR-13 | 热重载：构造新一代（`newRegistry` → `core.New` → `mcp.New` → 数据面路由），任一步失败 → 422，旧配置继续生效。成功后依次执行：`Scheduler.SetPools`，从数据库重新加载 Key 并 `SetKeys`，`admin.SetConfig`，设置日志级别，`RestrictModels`，最后原子替换数据面处理器。返回 200。 |
| HR-14 | 旧一代：在途请求在旧内核上完成（handler 计数照常）。连接 timeout 变化时，切换后立即对旧 Transport 调用 `CloseIdleConnections`，仍在使用的连接在请求结束后由 `IdleConnTimeout` 关闭。 |
| HR-15 | 进程内重启：先按 §2.3 预检，失败 → 422，旧实例不受影响。预检通过 → 写回 202，然后执行关闭序列（与收到 SIGTERM 相同），再用新配置执行启动序列。监听套接字由常驻的 accept 协程持有、跨实例保留（地址不变时交给新实例），旧服务端只关闭自己的虚拟监听器，不打断真正的 Accept。排空顺序：关闭监听器并等 `Serve` 返回 → 关闭 keep-alive，等全部连接关闭（最多 `shutdown_grace`，超时强制关闭）→ `Shutdown` 收尾。不直接用 `Shutdown` 排空，因为 net/http 会把「关闭开始后才读完请求」的连接直接关掉而不作响应。正常关机使用同样的排空顺序。新实例从数据库重新加载 Key，Key 的冷却、熔断与亲和状态会重置（与重启进程相同） |
| HR-16 | 新实例启动失败：按待定 6 处理，并在重载记录里写明失败原因。 |
| HR-17 | `dry_run: true`：执行 HR-11、HR-12，热重载时执行 HR-13 中的构造步骤，进程内重启时执行预检（只做检查：探测新地址时绑定后立即释放，不保留目录锁），返回同样的响应，但不应用。 |
| HR-18 | 记录：每次重载按 §2.5 记 Info 日志，并追加到内存中的最近 20 条记录（`status`：`applied`、`restarting`、`failed`、`rolled_back`、`noop`）。dry-run 只记日志（`dry_run: true`），不追加记录。错误原文只放进记录，不写日志（可能引用配置中的值） |
| HR-19 | 删除或重命名池：池里的 Key 保留在数据库中，不再被调度；管理 API 的 Key 列表对池不在当前配置中的 Key 显示 `state: "orphaned"`（只是展示，不写库；库中的状态原样保留在 `stored_state` 字段）。同名的池恢复后，这些 Key 自动重新被调度。 |
| HR-30 | DEPLOYMENT.md 新增「修改配置后生效」一节：热重载与进程内重启的字段对照表、`gateway reload` 用法与 dry-run、`data_dir` 变化会生成新管理员 Token 的提示、主密钥变更的限制（待定 5）、重载失败后的行为（待定 6）、`GET /admin/api/reload` 的记录。 |
