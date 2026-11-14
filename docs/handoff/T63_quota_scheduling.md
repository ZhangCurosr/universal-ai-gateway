# T63 按周期额度调度（已批准，已完成）

## 1. 问题

很多 API 的免费或低价账号带有**周期额度**，例如「每月 1000 次」「每天 N 页」「试用共 N 点，用完为止」。现在网关只能被动应对：一把 Key 用满之后，下一次请求先失败一次（429 或业务错误码），按 T66 的规则冷却，才换下一把。这有三个问题：

1. **每把 Key 用满时都要失败一次**：客户端会看到一次重试的延迟。如果上游用满时返回的不是可重试的错误，客户端还会直接看到失败。
2. **无法选择消耗方式**：有时希望一把用完再换下一把（便于淘汰旧账号，未用的账号保持完整），有时希望平均分摊（每把都留有余量，日额度场景下不会某一把先用满）。
3. **重置时刻靠上游告诉我们**：上游不说什么时候重置，冷却时长就只能靠猜。

## 2. 目标

- 在池上声明额度：计量单位、上限、周期、重置时刻、消耗策略。
- 网关按每把 Key 统计本周期用量，到上限就**主动**不再选它，直到下一个重置时刻。
- 两种策略：`drain`（耗尽优先）和 `spread`（平均分摊）。
- 用量跨重启准确；在已经运行的池上新增额度声明时，本周期已经用掉的部分也能算进去。
- 管理页面和 API 显示每把 Key 的本期用量、上限和重置时间；用满的 Key 显示「额度用尽」。

不在本卡范围内：
- 告警与补充 Key（T64）。
- 用量图表、预计耗尽时间、按池汇总（T65）。
- 每把 Key 各自不同的上限（以后在 Key 元数据上加，见决策 6）。
- 主动查询上游余额（各家没有统一接口）。

## 3. 配置示例

```yaml
pools:
  - name: tavily
    protocol: tavily
    quota:
      unit: credits            # 计量单位：必须是这个池的服务会计费的单位
      limit: 1000              # 每把 Key 每周期的上限
      period: month            # day | week | month | total（total = 不重置，用完为止）
      reset: { day: 1, time: "00:00", tz: "+08:00" }   # 每月 1 日 0 点（北京时间）
      strategy: drain          # drain（默认）| spread

  - name: mineru
    protocol: mineru
    quota:
      unit: pages
      limit: 2000
      period: day
      reset: { time: "00:00", tz: "+08:00" }
      strategy: spread
```

`reset` 中的字段按周期使用：
- `day` 只用 `time`。
- `week` 用 `weekday`（`mon` ~ `sun`）和 `time`。
- `month` 用 `day`（1 ~ 31，超过当月天数时取月末）和 `time`。
- `total` 不需要 `reset`。
- `tz` 缺省为 UTC，写法与 T66 的 `cooldown_until` 相同（实现时改为与 T66 一致；草案写的是本机时区）。

上面的数字只是示例，实际额度以各家账号说明为准。

## 4. 决策（建议）

| # | 问题 | 建议 | 备选 |
|:-:|:--|:--|:--|
| 1 | 用量从哪里来 | 在装配代码里包一层 `ledger.Ledger`。每次 `Settle` 后，把这把 Key 在额度单位上的用量加到调度器的内存计数上。**不改内核**（内核与契约冻结），热路径只做原子加法 | 改内核在 `settle` 里直接调用调度器：违反冻结规则 |
| 2 | 跨重启 | **启动时从计费事件重算**本周期用量（`usage_events` 已经记录了 Key ID 和各单位用量，而且从不清理）；配置热重载改了额度的单位、周期或重置时刻时也重算一次。不另存一份用量 | 按 T68 的方式定期快照用量：崩溃时会丢最近 5 秒；在已有的池上新加额度声明时，不知道本周期已经用了多少 |
| 3 | 重算的成本 | 新增索引 `usage_events(key_id, created_at)`，用 SQLite 的 `json_extract` 在数据库里求和。只在启动和改配置时执行，在控制面完成 | 读出全部事件在 Go 里求和：慢 |
| 4 | 用满后怎么处理 | 新增一种只在内存中的「额度用尽」判断（和 T62 的过期一样，不写库），**不借用冷却**：冷却有 `cooldown.max` 上限（默认 10 分钟，T68 恢复时也按它截断），表达不了「到下个月 1 日」。`total` 周期用满后一直不可用，直到增加上限或换 Key | 冷却到重置时刻：受 `cooldown.max` 限制 |
| 5 | 异步任务的查询 | 严格亲和（任务固定，SCH-17）**不检查额度**：任务已经在那把 Key 上创建，查询结果必须继续落在它上面，而且查询本身通常不计费。普通会话亲和照常检查，用满就换 Key | 一律检查：用满的 Key 上进行中的任务会查不到结果 |
| 6 | 上限按池还是按 Key | **按池**：同一个池里的账号通常来自同一套注册流程，额度相同 | 每把 Key 单独设置：要改 Key 元数据、导入与界面，放到以后需要时再做 |
| 7 | 策略的含义 | 先按原有规则取出可用的 Key（启用、未过期、未冷却、熔断器允许、未满载），再按剩余额度排序：`drain` 剩余最少的优先，`spread` 剩余最多的优先；剩余相同时按 ID。声明了额度的池忽略 `strategy`（最少在途 / 加权轮询） | 两种排序叠加：语义不清 |
| 8 | 并发时超出上限 | 软上限：「已用 < 上限」就可以选，所以同时在途的请求可能让用量略超上限，超出量不超过「在途数 × 单次用量」。上游的拒绝仍由 T66 的规则兜底 | 按预估用量预扣：每个服务都要能预估，复杂且不准 |
| 9 | 无 Key 可用时 | 错误信息附上「（N 把额度用尽）」；`Retry-After` 取最早的重置时刻（`total` 周期不参与） | — |
| 10 | 上游先于我们判定用满 | 不做特殊处理。T66 规则中的 `cooldown_until` 会让 Key 冷却到重置时刻。这种情况通常是网关以外也在用这把 Key，计数对不上 | 把上游判定写进本地计数：要区分「额度用尽」和「普通限流」，规则会变复杂 |

## 5. 规则（拟写入规格）

### spec/03 配置

| 编号 | 规则 |
|:--|:--|
| CFG-21 | `pools[].quota` 可选。<br>`unit` 必须是该池至少一个服务在 `price` 中声明的单位。<br>`limit` ≥ 1。<br>`period` ∈ `day` / `week` / `month` / `total`。<br>`reset.time` 为 `HH:MM`（默认 `00:00`）；`reset.weekday` 只用于 `week`（必填）；`reset.day` 只用于 `month`（1 ~ 31，必填）；`reset.tz` 写法同 T66（默认本机时区）。`total` 不允许写 `reset`。<br>`strategy` ∈ `drain` / `spread`，默认 `drain`。<br>违反任一条时加载失败。 |

### spec/07 调度器（新增 §7.3）

| 编号 | 规则 |
|:--|:--|
| QT-01 | `PoolConfig.Quota` 为空时行为与现在完全相同 |
| QT-02 | 每把 Key 在内存中保存「本周期开始时刻」和「已用量」。读取或累加时，如果当前时刻已经进入新周期，先清零再计算；周期边界由 `period` 和 `reset` 算出，按 `tz` 处理夏令时与月末 |
| QT-03 | `Scheduler.RecordUsage(keyID, usage map[string]int64)`：只累加额度单位上的数量；Key 不存在、池没有额度或数量 ≤ 0 时忽略。只用原子操作，不加池锁 |
| QT-04 | 非严格选择时，`已用 ≥ 上限` 的 Key 不可用（与过期同级，在 SCH-01 ~ SCH-05 之后判断）。严格亲和（SCH-17）不检查额度 |
| QT-05 | 声明了额度的池，候选集按剩余额度排序（`drain` 升序、`spread` 降序，相同时按 ID 升序），依次尝试占用；普通会话亲和命中且可用时仍优先 |
| QT-06 | `Scheduler.SetQuotaUsage(map[keyID]int64, periodStart)`：用重算结果覆盖内存计数（只覆盖周期开始时刻相同的 Key） |
| QT-07 | `KeyStatus` 增加 `QuotaUsed`、`QuotaLimit`、`QuotaResetAt`（`total` 为零值）、`QuotaExhausted` |
| QT-08 | §3.1 的无 Key 错误：信息附「（N quota exhausted）」，`Retry-After` 取冷却截止时刻与最早重置时刻中较早的一个 |

### spec/04 存储

| 编号 | 规则 |
|:--|:--|
| STO-20 | 迁移 `0004_usage_key_index.sql`：`CREATE INDEX idx_usage_key_time ON usage_events(key_id, created_at)` |
| STO-21 | `SumKeyUsage(ctx, unit string, since time.Time, keyIDs []int64) (map[int64]int64, error)`：对 `created_at >= since` 的事件按 Key 求 `usage` 中 `unit` 的和。`unit` 必须匹配 `^[a-z][a-z0-9_]*$`，否则返回错误；JSON 路径作为 `?` 参数传入，不拼接 SQL |

### 装配与管理面

| 编号 | 规则 |
|:--|:--|
| QT-10 | 启动时在 `SetKeys` 和 T68 恢复之后、开始服务之前，对每个声明了额度的池执行 STO-21，结果交给 QT-06。失败时记 Warn 并从 0 开始计数（不阻止启动） |
| QT-11 | 配置热重载（含进程内重启）改变了某池额度的 `unit`、`period` 或 `reset` 时，对该池重算；只改 `limit` 或 `strategy` 时保留计数 |
| QT-12 | 包装后的 `Ledger.Settle`：先调用原 `Settle`，再调用 `RecordUsage`（无论原 `Settle` 是否出错，因为上游已经产生了用量） |
| ADM-25 | `KeyView` 增加 `quota_used`、`quota_limit`、`quota_reset_at`（RFC 3339，没有时省略）。库中为 `ready`、额度用尽的 Key，`state` 显示为 `exhausted`，库中状态放进 `stored_state`（与 T62 的 `expired` 相同） |
| UI-35 | Key 页：声明了额度的池，表格增加「本期用量」列（`已用 / 上限` 加进度条）；`exhausted` 显示「额度用尽」，原因为「{time} 重置」，`total` 周期显示「额度已用完」 |

## 6. 测试

- **配置**：CFG-21 的每个分支，包括单位不在任何服务的价目表中、`total` 带 `reset`、`week` 缺 `weekday`。
- **周期计算**：`day` / `week` / `month` 的边界；月末取值（31 日在 2 月）；时区；`total`；跨周期自动清零。
- **调度器**：
  - 用满后不再选；
  - `drain` 和 `spread` 的选择顺序；
  - 严格亲和不受额度影响；
  - 无 Key 时的错误信息和 `Retry-After`；
  - 重算结果覆盖计数；
  - 不同 ID 的 `SetKeys` 不丢计数（沿用 SCH-16）。
- **存储**：STO-21 求和（多种单位、时间范围、非法单位名）。
- **装配**：
  - 包装后的 Ledger 在 `Settle` 后累加；
  - 启动重算；
  - 热重载只在需要时重算。
- **端到端**：fake 上游、两把 Key、`limit: 3`：
  - `drain` 时第一把用满 3 次后换第二把，**全程没有一次失败的上游请求**；
  - 重启后计数保留；
  - 两把都用满后返回 503 和 `Retry-After`；
  - 把时钟拨到重置时刻后恢复可用。
- **实测**：用真实的 Tavily Key 配一个很小的 `limit`，验证用满后主动换 Key，且网关计数与 Tavily 后台的用量一致。真实 Key 只通过 `gateway keys import` 按路径导入，不读出内容。

## 7. 实现中的补充

- `reset.tz` 缺省为 UTC（与 T66 的 `cooldown_until` 一致），草案写的是本机时区。
- 每把 Key 的额度计数用一把小锁保护（`quotaState`），保证跨周期清零与并发累加不会互相覆盖；只锁这一把 Key，不持池锁。
- 规则编号：配置 CFG-21；调度器 QT-01 ~ QT-12（QT-09 为 `SetKeys` 换新对象时沿用计数）；存储 STO-20、STO-21；管理面 ADM-25、UI-35。
- 概览页顺带把 `expired`（T62）计入已停用、`exhausted` 计入冷却中；之前已过期的 Key 在概览中被算作可用。
- 进度条增加 `.bar-bad` 样式（用满时红色）。

### 7.1 实测结果（2026-11-13，真实 Tavily Key，全部通过）

配置：原生 `tavily` 池，86 把 Key（`gateway keys import --where status=ok`），额度 `credits`、每把每天 2 点、`drain`、北京时间 0 点重置。

1. 5 次基础搜索全部成功：`tv-001`、`tv-002` 各用 2 点后显示「额度用尽」，第 5 次落在 `tv-003`。没有任何 Key 冷却或隔离，也就是用满前已经主动换 Key，没有一次上游失败。重置时刻为 `2026-11-14T16:00:00Z`（北京时间次日 0 点）。
2. 一次高级搜索（2 点）落在剩余最少的 `tv-003`，用量变为 3（软上限）。
3. 强制结束进程后重启：日志 `quota usage restored`（86 把，3 把有用量，3 把用尽），用量恢复为 2 / 2 / 3；下一次搜索落在 `tv-004`。
4. 网关日志中没有任何 Key、账号或 Token。
5. **未能核对**：Tavily 自己的 `/usage` 接口在测试期间（约 10 分钟）没有变化（搜索前后都是同样的数字），推测是统计有延迟，无法用它核对网关计数。T66 已验证过网关的 Tavily 计费与专用适配器一致。

## 8. 改动文件

- `internal/config`：`quota.go`（新增：类型、`Schedule`、CFG-21 校验）、`config.go`（`PoolConfig.Quota`、缺省策略）、`load.go`，以及 `quota_test.go`（新增）。
- `internal/scheduler`：`quota.go`（新增：周期计算、计数、`RecordUsage`、`SetQuotaUsage`、`QuotaPools`、`PoolKeyIDs`）、`key.go`、`scheduler.go`（过滤、排序、无 Key 错误、`KeyStatus`），以及 `quota_test.go`（新增）。
- `internal/store`：`store.go`（`SumKeyUsage`）、`sqlite/usage.go`、`sqlite/migrations/0004_usage_key_index.sql`（新增）、`sqlite/usage_test.go`。
- `cmd/gateway`：`quota.go`（新增：Ledger 包装、重算、重载判定）、`serve.go`、`reload.go`，以及 `quota_test.go`、`e2e_quota_test.go`（新增）、`e2e_rest_test.go`（测试用结构增加两个字段）。
- `internal/admin`：`keys.go`、`types.go`、`ui/pages/keys.js`、`ui/pages/overview.js`、`ui/i18n.js`、`ui/style.css`，以及 `quota_view_test.go`（新增）、`ui_test.go`。
- 规格：spec/03（CFG-21）、spec/04（STO-20、STO-21）、spec/07（§7.3）、spec/09（ADM-25、UI-35）、spec/11（T63 DONE）；`docs/DEPLOYMENT.md`（周期额度）、`gateway.example.yaml`（示例）。
