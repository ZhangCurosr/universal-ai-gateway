# T64 Key 补充与告警（已批准，已完成）

## 1. 问题

现在 Key 池能自己判断「用满」「过期」「失效」（T62、T63、ST-06），但池子什么时候快空了，只能靠人去管理页面看：

1. **新账号要手工进池**：注册程序不断往账号文件里追加新账号，每次都要手工运行一次 `gateway keys import`。账号文件里状态变成 `banned` 的账号，网关也不知道，要等调用失败两次才隔离。
2. **池子快空了没人知道**：可用 Key 只剩一两把、本月额度快用完、一批账号明天到期，都不会有任何提醒，直到客户端开始收到 503。
3. **CSV 账号文件要先转换**：MinerU 的账号文件是 CSV，导入前要用脚本转成 JSONL。

## 2. 目标

- `gateway keys sync`：让池与账号文件保持一致，可以一直运行。
  - 新账号进池；
  - 已有账号更新过期时间与标签；
  - 文件里已经没有或状态不再符合条件的账号停用；
  - 重新符合条件时恢复。
- `keys import` 和 `keys sync` 支持 CSV。
- 池级告警：可用 Key 太少、剩余额度太少、一批 Key 即将过期。
  - 告警显示在管理页面概览、管理 API、管理面 MCP 和 Prometheus 指标中；
  - 可选推送到 Webhook（通用 JSON、Slack、飞书、企业微信、钉钉）。

不在本卡范围内：
- 用量图表、失败率、预计耗尽时间（T65）。
- 自动注册新账号（属于注册程序，不属于网关）。
- 邮件、短信通知。

## 3. 用法示例

```bash
# 一次性同步：新增、更新，并停用文件中已经没有或 status 不再是 ok 的账号（只动带同一标签的 Key）
gateway keys sync --pool tavily --file tavily_accounts.jsonl --where status=ok --label signup-tavily

# 一直运行：文件有变化就同步（每 10 分钟检查一次修改时间），只在有变化时输出一行
gateway keys sync --pool mineru --file mineru_accounts.csv --format csv --expires-field expires_at --tz +08:00 \
  --label signup-mineru --watch 10m
```

```yaml
alerts:
  check_interval: 1m           # 每分钟评估一次
  repeat: 24h                  # 持续告警时每 24 小时再推送一次
  webhook:
    url_file: ./secrets/alert_webhook.url   # 地址里常带令牌，放文件里，不写进配置
    format: feishu             # generic | slack | feishu | wecom | dingtalk

pools:
  - name: tavily
    alerts:
      min_available_keys: 5    # 可用 Key 少于 5 把
      min_quota_left: 500      # 全池剩余额度少于 500（需要 T63 的 quota）
      expiring_within: 72h     # 有 Key 将在 72 小时内过期
```

## 4. 决策（建议）

| # | 问题 | 建议 | 备选 |
|:-:|:--|:--|:--|
| 1 | 同步在哪里运行 | **命令行** `gateway keys sync`，复用 `keys import` 的解析与管理 API。加 `--watch` 后常驻运行，也可以交给计划任务定期执行。账号文件只由本机的命令读取，网关进程不碰它 | 在网关配置里声明账号文件，由网关后台读取：网关要读含密码的文件，而且要在网关内部重复实现一遍导入逻辑 |
| 2 | 停用的范围 | 只停用**同一池中、标签与 `--label` 相同**的 Key（`--disable-missing` 必须配合 `--label`，`sync` 默认开启）。手工添加的 Key、其他来源的 Key 不受影响 | 停用池中所有不在文件里的 Key：会误伤手工添加的 Key |
| 3 | 停用与恢复 | 同步停用时把原因写成 `sync: not in source`。之后账号重新符合条件时**只恢复**原因是这一条的 Key；管理员手工停用或因 401 隔离的 Key，同步不会动 | 一律按文件恢复：会推翻管理员的操作 |
| 4 | 管理 API 的改动 | `PATCH /admin/api/keys/{id}` 增加可选的 `reason`（只在 `state: disabled` 时有效，≤ 200 字节） | 新开一个同步专用接口：重复 |
| 5 | 安全阈值 | 一次同步要停用的 Key 超过已有数量的 50% 时，不停用，报错退出（文件可能写坏了或读到了半截）。加 `--force` 才执行 | 不设阈值：文件损坏时会一次停掉整个池 |
| 6 | CSV | `--format csv`（第一行是表头，RFC 4180 引号规则；分隔符自动识别逗号或制表符）；字段参数与 JSONL 相同（`--field`、`--where`、`--expires-field`、`--label-field`） | 继续要求先转换：麻烦 |
| 7 | 告警规则 | 按池配置三条：<br>`min_available_keys`：当前可用的 Key 数（`ready`、未过期、额度未用尽、熔断器未打开；短暂冷却的算可用）；<br>`min_quota_left`：全池剩余额度之和，只对声明了 `quota` 的池有效；<br>`expiring_within`：有 Key 在这段时间内过期。<br>都不配就不告警 | 全局统一阈值：各池规模差别很大 |
| 8 | 评估方式 | 控制面后台每分钟只读一次调度器的内存状态，不访问数据库。条件成立时进入「告警中」，不成立时「已恢复」。状态变化时记一条日志并推送；持续告警时每 `repeat` 再推送一次 | 每次请求时检查：会增加热路径开销 |
| 9 | 推送 | 可选 Webhook，固定几种消息格式，不做模板。每次推送超时 10 秒，失败重试 2 次后记 Warn 放弃；推送失败不影响告警状态。地址从 `url_file` 读取，日志中打码 | 自定义模板：等于在配置里写代码 |
| 10 | 重启后 | 告警状态只在内存中。重启后仍然成立的告警会再推送一次（便于确认网关又在运行） | 持久化告警状态：意义不大 |
| 11 | 告警在哪里看 | 新增 `GET /admin/api/alerts`；管理页面概览顶部显示；管理面 MCP 增加只读工具 `gateway_admin_alerts`（可以问 AI「Key 池有没有问题」）；Prometheus 新增池级指标 | 只推送：错过就看不到 |

## 5. 规则（拟写入规格）

### 命令行（spec/03 或 DEPLOYMENT）

| 编号 | 规则 |
|:--|:--|
| CLI-12 | `keys import` 与 `keys sync` 支持 `--format csv`：第一行为表头；按 RFC 4180 处理引号；第一行含制表符且不含逗号时以制表符分隔；表头去掉 BOM 与首尾空白；字段参数按列名取值，与 JSONL 相同；数据行的列数与表头不同时跳过该行（只输出行号） |
| CLI-13 | `keys sync` 等于 `keys import --update`，再加上停用与恢复：要求 `--label`；文件中符合条件的 Key 记为「在源中」。同一池、标签等于 `--label`、不在源中、库中状态为 `ready` 的 Key 停用，原因为 `sync: not in source`；在源中、库中为 `disabled` 且原因正是这一条的 Key 恢复为 `ready`。已过期的账号（未加 `--include-expired`）视为不在源中 |
| CLI-14 | 将要停用的数量大于同标签已有 Key 的 50% 且至少 2 把时，不做任何停用，退出码 1，提示使用 `--force`；`--dry-run` 只输出各项数量 |
| CLI-15 | `--watch <间隔>`（≥ 1 分钟）：每个间隔检查一次文件修改时间与大小，有变化才同步，并输出一行带时间的汇总；单次失败只输出错误并继续；收到中断信号后退出。不加 `--watch` 时同步一次就退出 |
| CLI-16 | 同步输出：`synced: imported N, updated U, disabled D, restored R, unchanged C, skipped S, expired E, failed F`。任何输出都不包含密钥或文件中的其他字段 |

### 配置（spec/03）

| 编号 | 规则 |
|:--|:--|
| CFG-22 | `alerts` 可选：<br>`check_interval` 缺省 1m，≥ 10s；<br>`repeat` 缺省 24h，≥ 1m；<br>`webhook.url_file` 存在时 `webhook.format` ∈ `generic` / `slack` / `feishu` / `wecom` / `dingtalk`（缺省 `generic`），文件内容去掉首尾空白后必须是 http(s) URL（启动时读取，读不到则启动失败）。<br>`pools[].alerts` 可选：`min_available_keys` ≥ 1；`min_quota_left` ≥ 1，且该池必须声明了 `quota`；`expiring_within` ≥ 1h。 |

### 告警（spec/07 新增 §7.4，或独立一章）

| 编号 | 规则 |
|:--|:--|
| AL-01 | 后台 worker `pool_alerts` 每 `check_interval` 用 `Scheduler.KeyStatus()` 计算每个配置了告警的池：`available`（库中 `ready`、未过期、额度未用尽、熔断器不是 `open`）、`quota_left`（`available` 的 Key 的 `max(上限 − 已用, 0)` 之和）、`expiring`（未过期、在 `expiring_within` 内过期的 Key 数）。不访问数据库 |
| AL-02 | 告警种类 `low_keys`（`available < min_available_keys`）、`low_quota`（`quota_left < min_quota_left`）、`expiring`（`expiring > 0`）。每池每种至多一条，带 `level`（`low_keys` 在 `available == 0` 时为 `critical`，其余为 `warning`）、`value`、`threshold`、`since`、`message`（英文一句话） |
| AL-03 | 状态变化（新告警、已恢复、级别变化）时记 Warn 或 Info 日志并推送；持续告警时每 `repeat` 推送一次 |
| AL-04 | Webhook：POST JSON，超时 10 秒，非 2xx 或网络错误时间隔 2 秒、4 秒各重试一次，之后记 Warn 放弃。一次评估中有多条变化时合并为一条消息。日志中的地址用 `obs.MaskSecret` |
| AL-05 | 消息格式：<br>`generic` 为 `{"status", "alerts": [...]}`；<br>`slack` 为 `{"text"}`；<br>`feishu` 为 `{"msg_type": "text", "content": {"text"}}`；<br>`wecom` 与 `dingtalk` 为 `{"msgtype": "text", "text": {"content"}}`。<br>文本每条告警一行：`[级别] 池: 说明`。 |
| AL-06 | 配置热重载后按新阈值重新评估；被删除的池或规则对应的告警直接移除（不推送「已恢复」） |
| ADM-26 | `GET /admin/api/alerts`：`{"alerts": [...], "checked_at"}`；没有配置告警时返回空数组 |
| ADM-27 | `PATCH /admin/api/keys/{id}` 接受可选 `reason`：只在 `state` 为 `disabled` 时有效（否则 400），≤ 200 字节；缺省仍为 `disabled by admin` |
| UI-36 | 概览页顶部显示当前告警（级别颜色、池名、说明、开始时间）；没有告警时不显示这一块 |
| MTR-13 | 新增指标 `uaig_pool_available_keys{pool}`、`uaig_pool_quota_left{pool}`（只对有额度的池）、`uaig_pool_keys_expiring{pool}`、`uaig_alerts_firing{pool,kind}` |
| ADM-28 | 管理面 MCP 增加只读工具 `gateway_admin_alerts`（返回 ADM-26 的内容） |

## 6. 测试

- **CSV 解析**：引号、引号里的逗号和换行、制表符、BOM、列数不一致。
- **sync**（fake 管理面或端到端）：
  - 新增、更新；
  - 停用不在源中的 Key；
  - 恢复由同步停用的 Key，不恢复手工停用和隔离的 Key；
  - 不碰其他标签和手工添加的 Key；
  - 超过 50% 拒绝，`--force` 后执行；
  - `--dry-run`；
  - `--watch` 只在文件变化后同步。
- **管理 API**：`reason` 的校验与写入。
- **告警评估**：
  - 三种规则与级别；
  - 只在状态变化时推送；
  - 持续告警按 `repeat` 重复推送；
  - 热重载后移除；
  - 冷却中的 Key 算可用。
- **Webhook**：五种格式的请求体；失败重试；地址不出现在日志中。
- **配置**：CFG-22 的每个分支。
- **端到端**：fake 上游与 fake Webhook，Key 用满后收到告警推送，恢复后收到恢复推送；管理页面概览显示告警。
- **实测**：用 MinerU 的 CSV 账号文件直接 `sync`（不再转换），确认数量与上次导入一致；Webhook 推送只发到本机 fake 接收端，**不推送到任何外部服务**。

## 7. 实现中的补充

- 规则最终编号：CLI-12 ~ CLI-17（spec/14）、CFG-22（spec/03）、AL-01 ~ AL-06（spec/07 §7.4）、ADM-26 ~ ADM-28 与 UI-36（spec/09）、MTR-13（spec/15）。
- **CLI-17（新增）**：`keys import` 与 `keys sync` 的名称编号接在池中同前缀已有的最大编号之后。端到端测试发现，第二次同步新增的 Key 会从 `-001` 重新编号，与已有 Key 撞名；现在与网页导入（ADM-23）一致。`import` 也因此在写入前先读取一次 Key 列表。
- `--dry-run` 时 `sync` 只输出源中的数量：停用与恢复要按指纹比对，只能由网关判断。
- 一轮中任一行导入失败时，这一轮不停用也不恢复（无法确定失败的行是否在源中）。
- CSV 用标准库 `encoding/csv` 解析，每行转换为等价的 JSON 对象后走原来的 JSONL 路径，已有的导入测试不变。
- 告警的过期窗口在消息中写成 `72h`、`90 days`。
- `alerts` 段加入热重载的段落列表；池上的 `alerts` 属于池的变化，同样热重载。
- **改动了一处已有测试的期望**：管理面 MCP 的工具数量由 10 变为 11、只读工具由 7 变为 8（`TestAdminMCP`），原因是本卡新增了只读工具 `gateway_admin_alerts`（ADM-28），spec/14 AM-02 同步更新。
- 概览页的告警卡片在没有告警时不插入（`replaceChildren` 会把 `null` 渲染为文字）。

### 7.1 实测结果（2026-11-14）

1. **MinerU CSV 直接同步**（真实账号文件，87 行有 Key）：`--dry-run` 显示源中 87 把；第一次同步 `imported 87`，与之前先转 JSONL 再导入的数量一致，Key 都带有标签与过期时间；第二次同步 `imported 0, updated 87, disabled 0`；名称 `mu-001 .. mu-087` 无重复。输出与网关日志中没有任何 Key、邮箱或密码。
2. **告警推送**（只推送到本机的假接收端 `127.0.0.1`，没有发到任何外部服务）：池 `mineru` 设置 `min_available_keys: 100`、`expiring_within: 2160h`，启动后第一次评估就推送了一条飞书格式的消息（两条 warning：87 把可用低于 100；87 把将在 90 天内过期）；`GET /admin/api/alerts` 列出这两条；网关日志中没有 Webhook 地址中的令牌。

## 8. 改动文件

- `cmd/gateway`：`keys_import.go`（参数解析拆分、CSV、编号接续）、新增 `keys_sync.go`、新增 `alerts.go`、`serve.go`（启动检查地址文件、worker、指标与管理面接线）、`reload.go`（`alerts` 热重载）、`admin_mcp.go`（新工具）、`main.go`（帮助文字）；新增测试 `keys_sync_test.go`、`alerts_test.go`、`e2e_alerts_test.go`，`admin_mcp_test.go` 增加断言并改工具数量。
- `internal/config`：新增 `alerts.go`、`alerts_test.go`；`config.go`、`load.go`。
- `internal/admin`：`keys.go`（`reason`）、`types.go`（`Reason`、告警视图）、`server.go`（`GET alerts`）；UI 的 `overview.js`、`i18n.js`；测试 `key_metadata_test.go`、`quota_view_test.go`、`ui_test.go`。
- `internal/metrics`：`types.go`、`format.go`、`metrics_test.go`。
- 规格：spec/03、spec/07、spec/09、spec/11、spec/14、spec/15；`docs/DEPLOYMENT.md`；`gateway.example.yaml`。
