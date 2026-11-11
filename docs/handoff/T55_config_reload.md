# T55 配置热重载（M3）

- **类型**：规格 + 实现
- **前置**：T54（`DONE`，改动需先提交）。不改内核（`internal/core`）与契约（`internal/protocol`），不加依赖。
- **需要用户审批**：~~是~~ **已批准**（2026-11-11）：待定 1 为「全部」，2 ~ 6 采用建议方案 **a**（见 §3.6 决议列）。阶段 A 把 §3 写入 spec/03、spec/07、spec/09，之后直接进入阶段 B。
- **必读**：
  - [`spec/03_config.md`](../../spec/03_config.md) §1（命令行）、§4（校验）、§5（启动与关闭顺序）
  - [`spec/07_scheduler.md`](../../spec/07_scheduler.md) SCH-01 ~ SCH-19（尤其 `SetKeys` 的同 ID 规则 SCH-16）
  - [`spec/09_http_api.md`](../../spec/09_http_api.md) 管理 API（ADM-01 ~ ADM-17）
  - [`spec/06_billing_ledger.md`](../../spec/06_billing_ledger.md) §5（崩溃语义）、FLU-05
  - `cmd/gateway/serve.go`（`run` 的完整启动与关闭序列）、`cmd/gateway/adapters.go`、`cmd/gateway/main.go`
  - `internal/scheduler/scheduler.go`、`internal/admin/server.go`、`internal/obs/obs.go`、`internal/metrics/collector.go`

## 1. 背景与目标

现在改任何配置（加一个服务、改一个价格、声明一个接口）都要重启网关，重启会打断正在进行的请求。T56（接入 API 的 skill）的最后一步是「让新配置生效」，没有热重载这一步就断了。

本卡让网关**不退出进程**就应用新配置。用户的决定是**所有配置都可以重载**。但有些字段天生无法做到「不中断」（例如数据目录：换了就是另一个数据库），所以分两级：

| 级别 | 做法 | 对客户端的影响 |
|:--|:--|:--|
| **热重载** | 新建注册表、内核、MCP 服务端，原子替换数据面处理器；账本、调度器、存储、审计沿用 | 无：在途请求在旧内核上完成，新请求进入新内核 |
| **进程内重启** | 按正常关闭顺序排空并刷盘，再用新配置重新走启动顺序；监听地址不变时沿用同一个监听套接字 | 在途请求最多等 `shutdown_grace`；新连接在重启期间排队等待，不会被拒绝（地址变化时除外） |

网关比较新旧配置，**自动选择**需要的级别，并在响应里说明选了哪一级、改了什么。

触发方式（Windows 没有 SIGHUP）：管理 API `POST /admin/api/reload`，以及命令 `gateway reload`。两者都支持 `dry_run`：只校验并报告「会做什么」，不应用。T56 的 skill 先 dry-run，再正式应用。

## 2. 关键设计

### 2.1 哪些字段热重载，哪些需要进程内重启

| 热重载 | 进程内重启 |
|:--|:--|
| `pools`、`models`（含价格、`tools`）、`mcp`、`translation`、`server.log_level`、`server.trusted_proxies`、`server.admin_hosts` | `server` 的其他字段（监听地址、`data_dir`、`master_key_file`、超时、`max_header_bytes`、`shutdown_grace`）、`limits`、`ledger`、`audit` |

分界依据：右边的字段被启动时创建、带状态的对象持有。例如 `limits` 决定全局并发计数器；如果中途替换计数器，新旧计数器会各算各的，在途请求的总数短时间内可能突破上限。`ledger.event_queue_capacity` 决定计费事件队列的容量，替换队列有丢计费事件的风险（AGENTS §3 #4）。这些字段只能通过排空、刷盘、重建的方式安全地改变。

### 2.2 热重载：重建「无状态」的部分，沿用「有状态」的部分

每次热重载构造一代新的：注册表（`newRegistry`，含 rest 接口声明与目录检查）、`core.New`、MCP 服务端、数据面路由。用 `atomic.Pointer[http.Handler]` 原子替换数据面处理器。

沿用同一份：账本（额度、预留）、调度器（Key 的冷却、熔断、亲和、任务固定）、存储、审计管道、限流器、并发计数器、Token 索引、上游 http.Client 的响应预算。

需要补的接口都是新增方法，不改已有签名：

- **调度器 `SetPools(pools []PoolConfig)`**：增加新池；更新已有池的策略、冷却、熔断等参数（已有 Key 的状态保留）；删除的池不再调度（待定 3）。之后从数据库重新加载 Key 并 `SetKeys`：按 SCH-16，同 ID 的 Key 保留运行时状态，新池中的 Key 开始被调度。「孤立」由管理面按当前配置判断（Key 的池不在配置中），调度器不需要额外状态。
- **管理面**：`admin.Server` 读取配置改为原子指针，新增 `SetConfig`。`/admin/api/config`、可信代理、`admin_hosts` 的 Host 检查都读取当前配置。
- **日志级别**：`obs` 新增基于 `slog.LevelVar` 的构造函数，热重载时只改级别，不换 logger。
- **指标**：重新调用 `RestrictModels`（它本来就是原子替换）。
- **上游 http.Client**：所有池的 `timeouts.connect` 的最大值变化时，新一代使用新的 Transport（共享同一个响应预算），切换后立即对旧 Transport 调用 `CloseIdleConnections`；仍在使用的连接随旧一代的请求结束，之后由 `IdleConnTimeout` 关闭。

### 2.3 进程内重启：复用现有的启动与关闭序列

`run` 已经按 spec/03 §5 完整地启动和关闭。进程内重启就是：执行正常的关闭序列（数据面排空 → 管理面关闭 → worker 刷盘 → 关闭数据库 → 释放目录锁），然后用新配置再执行一次启动序列。外层在 `serve` 子命令里循环调用 `run`。

计费安全与正常关机完全相同：所有计费事件在关闭序列中刷盘，FLU-05 的恢复逻辑照常工作。

为了不让客户端在重启期间连接失败：**监听地址不变时，把监听套接字交给下一次 `run` 继续使用**。重启期间新连接在队列（以及内核的 accept 队列）里等待，新服务端开始 Serve 后就被接受。地址变化时没有办法避免，旧地址关闭、新地址开始监听。

实现中遇到的两个坑（已在 HR-15 中写明做法）：在 Windows 上用截止时间打断进行中的 `Accept` 可能丢掉刚接受的连接，所以真正的套接字由常驻的 accept 协程持有，每个服务端只拿到一个虚拟监听器；`http.Server.Shutdown` 开始后，已接受但还没读完请求的连接会被 net/http 直接关闭，所以排空改为「关闭 keep-alive，等连接自然关闭」。

先检查、再动手：进程内重启之前做**预检**，任何一项失败都不关闭旧实例，直接返回错误：

1. 新配置校验通过，注册表与内核可以用新配置构造（不连接上游）；
2. 监听地址变化时，**先**绑定新地址（与旧地址不冲突，可以同时持有）；
3. `data_dir` 变化时，新目录可创建、目录锁可获取；
4. `master_key_file` 变化时，按待定 5 检查。

预检通过之后，启动阶段仍然可能失败（例如数据库迁移出错）。此时按待定 6 回滚。

### 2.4 管理员调用 reload 的那个请求怎么办

进程内重启会关闭管理面，而触发重启的正是一个管理 API 请求。所以先写回响应（`202`，`mode: "restart"`），再开始关闭。CLI 收到 202 后轮询 `GET /admin/api/reload` 等待新实例就绪。

`data_dir` 变化意味着新的空数据库：新实例会像首次启动一样生成新的引导管理员 Token，并打印到 stderr。旧的管理员 Token 在新数据库里不存在。dry-run 的响应必须写明这一点，正式应用时同样写在响应的 `warnings` 中。

### 2.5 记录（待定 4）

当前没有管理操作审计表。本卡不加数据库表（避免迁移），做两件事：

- 每次重载（包括失败的）记一条 Info 日志 `config reload`：操作者的客户端 ID、模式、结果、增删改的池名和服务名、需要重启的字段名。**不记配置内容**，配置里可能有内网地址等信息。
- 内存中保留最近 20 次重载记录（内容同上，加时间戳），`GET /admin/api/reload` 返回，供 CLI 与管理页面查看。进程内重启时把这份记录交给新实例。

## 3. 规格草案（阶段 A：写入 spec）

### 3.1 命令行（spec/03 §1）

| 编号 | 规则 |
|:--|:--|
| HR-01 | `gateway serve` 记住 `--config` 的路径；重载总是重新读取这个文件。 |
| HR-02 | `gateway reload [--dry-run] [--json] [--timeout 2m]`：连接信息来自环境变量 `UAIG_ADMIN_URL`（缺省 `http://127.0.0.1:8081`）与 `UAIG_ADMIN_TOKEN`（必填），不提供 `--token` 参数（同 CLI-05）。调用 `POST /admin/api/reload`。收到 202 时轮询 `GET /admin/api/reload`，直到最新一条记录不再是 `restarting`，或者超时。成功退出码 0；校验或预检失败退出码 1，把错误消息写到 stderr；`--json` 输出完整响应。 |

### 3.2 重载流程（spec/03 新增 §8）

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

### 3.3 调度器（spec/07，新增 SCH-20）

| 编号 | 规则 |
|:--|:--|
| SCH-20 | `SetPools(pools)`：新池加入；已有池更新参数，Key 及其运行时状态保留（熔断参数按新配置重新设置），已发出的租约按发出时的池参数结束；不在 `pools` 中的池停止调度，`Acquire` 视为不存在，其中的 Key 从调度器移除（`KeyStatus` 不再包含）。并发安全，与 `Acquire`、`Lease.Finish`、`SetKeys` 可以同时调用；`Acquire` 不加全局锁。 |

### 3.4 管理 API（spec/09，新增 ADM-18、ADM-19）

| 编号 | 规则 |
|:--|:--|
| ADM-18 | `POST /admin/api/reload`，仅管理员角色。请求体可选 `{"dry_run": bool}`。响应 `{"mode": "none"\|"hot"\|"restart", "applied": bool, "changes": {"pools": {"added", "removed", "changed"}, "services": {"added", "removed", "changed"}, "settings": [...], "restart_fields": [...]}, "warnings": [...]}`；`settings` 列出变化的其他热重载字段（如 `mcp`、`server.log_level`），`restart_fields` 列出需要重启的字段（如 `limits.max_in_flight_requests`）。状态码：200（none 或 hot 已应用，或 dry-run）、202（restart 已开始）、400（配置错误）、409（重载进行中）、422（构造或预检失败）。错误体使用管理 API 现有的错误格式。 |
| ADM-19 | `GET /admin/api/reload`，仅管理员角色。返回 `{"records": [...]}`，最近 20 次重载，新的在前，每条 `{time, actor_client_id, mode, status, changes, error?}`。 |

### 3.5 部署文档

| 编号 | 规则 |
|:--|:--|
| HR-30 | DEPLOYMENT.md 新增「修改配置后生效」一节：热重载与进程内重启的字段对照表、`gateway reload` 用法与 dry-run、`data_dir` 变化会生成新管理员 Token 的提示、主密钥变更的限制（待定 5）、重载失败后的行为（待定 6）、`GET /admin/api/reload` 的记录。 |

### 3.6 待定项汇总

| # | 问题 | a（建议） | b | 决议 |
|:--|:--|:--|:--|:--|
| 1 | 哪些配置可以重载 | 部分 | 全部 | **全部**（2026-11-11）：按 §2.1 分为热重载与进程内重启两级 |
| 2 | 是否自动监视配置文件 | **a**：先不做，只支持显式触发 | 轮询修改时间自动重载 | **a** |
| 3 | 删除或重命名池时池里的 Key | **a**：保留、不调度、标为孤立，同名池恢复后回归 | 删除 | **a** |
| 4 | 重载是否有记录 | **a**：有（§2.5） | 无 | **a** |
| 5 | `master_key_file` 变化 | **a**：只在新文件中的密钥与当前密钥相同（例如只是移动了文件），或 `data_dir` 同时变化（新数据库）时允许；否则拒绝。理由：现有上游 Key 用主密钥加密，客户端 Token 的哈希也依赖由主密钥派生的 pepper，换了密钥，所有 Key 无法解密、所有 Token 失效 | 允许任意变更，并警告「所有凭据将失效」 | **a** |
| 6 | 进程内重启时新实例启动失败 | **a**：用旧配置再启动一次（回滚），记录 `rolled_back`；回滚也失败才以退出码 1 退出（交给第 5 节的系统服务自动重启） | 直接以退出码 1 退出 | **a** |

主密钥轮换（用新密钥重新加密全部凭据）是另一件事，不在本卡范围内。

## 4. 实现要求（阶段 B）

### 4.1 文件布局

```
cmd/gateway/
  main.go          serve 循环调用 run；reload 子命令（HR-02）
  serve.go         run 拆分：有状态部分（账本、调度器、存储……）与「一代」（注册表、内核、MCP、数据面路由）；
                   接收监听套接字与重载记录；在关闭序列后返回「重启」信号
  reload.go        新增：配置比较（HR-12）、热重载（HR-13、HR-14）、预检（§2.3）、记录（HR-18）
  reload_test.go   新增
internal/scheduler/
  scheduler.go     SetPools（SCH-20）；KeyStatus.Orphaned
internal/admin/
  server.go        配置改为原子指针、SetConfig；/admin/api/reload 两个端点（ADM-18、ADM-19），通过 Deps 中注入的函数调用 cmd 层
internal/obs/
  obs.go           基于 slog.LevelVar 的 logger 构造函数
```

管理面不能导入 `cmd/gateway`。重载逻辑在 `cmd/gateway` 中实现，通过 `admin.Deps` 中的函数字段（例如 `Reload func(ctx, actor int64, dryRun bool) (ReloadResult, error)`）注入。

### 4.2 约束

- 不改 `internal/core`、`internal/protocol`、`internal/archtest`、`internal/adapter/adaptertest`。
- 数据面热路径不加锁：处理器替换只用 `atomic.Pointer`。
- 热重载过程中不丢计费事件；进程内重启的计费保证与正常关机相同。
- 日志与重载记录中不出现配置内容、Key、Token。
- 所有等待都有上限（排空 `shutdown_grace`、CLI 的 `--timeout`）；不使用固定 sleep 等待业务状态，测试用同步信号。

## 5. 涉及文件

- `spec/03_config.md`、`spec/07_scheduler.md`、`spec/09_http_api.md`、`spec/11_tasks.md`（阶段 A）
- `cmd/gateway/main.go`、`serve.go`、`reload.go`（新增）、`reload_test.go`（新增）、`client_cmds.go`（`gateway reload`）
- `internal/scheduler/scheduler.go`、`setpools_test.go`（新增）
- `internal/admin/server.go`、`keys.go`、`clients.go`、`ui.go`、`types.go`
- `internal/obs/obs.go`、`obs_test.go`
- `docs/DEPLOYMENT.md`

## 6. 测试要求

1. **配置比较**：每个热重载字段单独变化 → `hot`；每类需要重启的字段单独变化 → `restart`，并列在 `restart_fields` 中；完全相同 → `none`；服务与池的增、删、改被正确分类。
2. **热重载**（端到端）：
   - 新增服务、修改价格、新增 rest 接口声明后，不重启即生效（`tools/list`、计费按新价格）；
   - 进行中的长请求（假上游阻塞）在重载后照常完成，并按**旧**价格结算一次；
   - Key 的冷却与熔断状态在重载后保留；
   - 删除池后其中的 Key 显示 `orphaned`，恢复同名池后重新被调度；
   - 配置错误 → 400，构造失败（例如未知协议）→ 422，两种情况下旧配置都继续生效；
   - 日志级别热切换生效。
3. **进程内重启**（端到端）：
   - 修改 `limits` 后：202，新实例使用新的限制，监听地址不变，重启期间发出的请求最终成功（不被拒绝）；
   - 重启前的计费事件全部落库；
   - 修改监听地址：新地址可用，旧地址关闭；
   - 新地址已被占用 → 预检 422，旧实例不受影响；
   - 新实例启动失败（注入数据库打开失败）→ 回滚到旧配置，记录为 `rolled_back`；
   - `master_key_file` 的三种情况（相同密钥、不同密钥、同时改 `data_dir`）按待定 5 处理。
4. **dry-run**：各模式下返回与正式应用相同的 `mode` 与 `changes`，不产生任何变化（处理器、调度器、监听、记录中的 `applied` 都不变）。
5. **并发**：两次同时重载，其中一次得到 409；重载与持续的请求流并行（race 检测在 Linux 上验证）。
6. **调度器**：SCH-20 的增、改、删，与 `Acquire`、`Finish` 并发。
7. **CLI**：`gateway reload` 的成功、校验失败、dry-run、202 后轮询直到完成、超时。
8. **安全**：重载日志与记录中没有配置内容、Key、Token；非管理员调用 → 403。

## 7. 验收标准

- [x] 待定 5、6 有用户决议（a）。
- [x] §3 写入 spec。
- [x] `go run ./tools/check` 全绿；§6 所列测试全部存在并通过（重载相关测试连续重复 10 次、进程内重启测试连续 40 次无失败）。
- [ ] Linux race 验证通过（交给 T46 一类的验证轮次）。
- [x] 不改内核、契约、架构检查和一致性套件，不新增依赖。
- [x] 实测（2026-11-11，真实 Bivia Key，13/13 通过）：运行中新增 Bivia 服务并 `gateway reload`，不重启即可调用，声明的工具可用；修改 `limits` 触发进程内重启，期间持续发出的 12 个请求全部成功，进程 PID 不变；重启前后的 14 次成功调用全部计费。
- [x] DEPLOYMENT.md 按 HR-30 更新（§5.1）。
- [x] spec/11 中 T55 状态改为 `DONE`。

## 8. 建议提交拆分

1. `docs(spec): T55 配置热重载规格`
2. `feat(scheduler): T55 SetPools 与孤立 Key`
3. `feat(gateway): T55 热重载`（reload.go、admin 端点、obs 日志级别）
4. `feat(gateway): T55 进程内重启与回滚`
5. `feat(cli): T55 gateway reload` 与 `docs: T55 部署文档`
