# T65 Key 视图（已批准，已完成）

## 1. 问题

T62 ~ T64 让网关知道了每把 Key 的过期时间、本期额度和池的容量，但要回答下面这些问题，仍然只能看日志或自己算：

- 「这个池这个月还够用吗？按现在的用法什么时候用完？」
- 「哪几把 Key 最近老失败？失败的是限流、鉴权还是超时？」
- 「每把 Key 最近 24 小时用了多少、花了多少钱？」

现有数据的缺口：审计记录每个请求只记最后一次尝试的 Key，被重试掉的失败尝试（换 Key 之前的那一次）没有按 Key 记下来，所以算不出每把 Key 的真实失败率。

## 2. 目标

- 每把 Key：最近 24 小时的尝试次数、各类失败次数与失败率、最近一次失败的种类与时间；所选时间段内的请求数、各计量单位用量与费用。
- 每个池：Key 按状态计数；声明了额度的池给出本期已用、剩余、最近 24 小时的消耗速度、**预计用完时间**，以及能否撑到下次重置；即将过期的 Key 数。
- 在管理页面、管理 API 和管理面 MCP 中都能看到，可以直接问 AI「tavily 池这个月还够用吗」。

不在本卡范围内：图表与历史趋势（需要另存时间序列，以后需要时再做）、按客户端拆分的 Key 用量、告警规则的扩展（T64 已有）。

## 3. 决策（建议）

| # | 问题 | 建议 | 备选 |
|:-:|:--|:--|:--|
| 1 | 失败率从哪来 | 调度器在 `Lease.Finish` 中按 Key 记录每次**尝试**的结果：24 个小时桶，只做原子加法；只在内存中，重启后从零开始 | 从审计记录统计：漏掉被重试的失败尝试，而且审计记录可能被丢弃（队列满时） |
| 2 | 什么算失败 | `rate_limited`、`auth_failed`、`permission_denied`、`server_error`、`timeout`、`network_error`。`client_error`（请求本身有问题）、`canceled`、`local_error` 不算 Key 的失败，也不计入分母 | 一切非成功都算：客户端发错请求也会拉低 Key 的成功率 |
| 3 | 用量与费用从哪来 | 计费事件 `usage_events`，按 Key 汇总（T63 加的 `(key_id, created_at)` 索引）。管理接口请求时查询，属于控制面，不在热路径上 | 在内存里累计：重启丢失，也不能选时间段 |
| 4 | 预计用完时间怎么算 | 消耗速度 = 池在额度单位上最近 24 小时的用量 ÷ 24 小时；预计用完 = 现在 + 剩余 ÷ 速度。速度为 0 时为「按当前用法不会用完」。有重置时刻的周期，用完时间晚于下次重置时显示「能撑到 {重置时间}」 | 按本期平均速度：月初几天波动太大；或用线性回归：没必要 |
| 5 | 剩余额度的口径 | 与 T64 告警相同（AL-01）：只算**可用** Key 的剩余；停用、隔离、过期、用尽的 Key 不算 | 所有 Key 都算：停用的 Key 剩余再多也用不上 |
| 6 | 页面放在哪 | Key 页每个池卡片的标题下加一行池摘要（可用、剩余额度、预计用完）；表格增加「24 小时」列（请求数与失败率，悬停显示各类失败次数）。不新增页面 | 新开「池」页：信息分散 |
| 7 | 管理面 MCP | 新增两个只读工具：`gateway_admin_pools`（池摘要，回答「还够用吗」）和 `gateway_admin_key_stats`（每把 Key 的用量与失败，可选池与时间段） | 只加一个：AI 问池时不需要逐 Key 数据，数据量大 |

## 4. 规则（拟写入规格）

### 调度器（spec/07 新增 §7.5）

| 编号 | 规则 |
|:--|:--|
| KS-01 | 每把 Key 有 24 个小时桶（按整点对齐），每桶记录尝试数与各失败种类的次数。`Lease.Finish` 只做原子加法；桶的轮换用比较并交换（桶的小时数与当前不同时先清零），不加锁 |
| KS-02 | 计入尝试的结果：`success` 与 §3 决策 2 的六种失败；其他结果不计入 |
| KS-03 | `Scheduler.KeyStats(now) map[keyID]KeyStat`：最近 24 小时（含当前小时）的尝试数、各种失败数、最近一次失败的种类与时间。`SetKeys` 以新对象替换同 ID 的 Key 时沿用统计 |

### 存储（spec/04）

| 编号 | 规则 |
|:--|:--|
| STO-22 | `SumUsageByKey(ctx, from, to time.Time) ([]KeyUsage, error)`：按 Key 汇总 `[from, to)` 内的计费事件，返回 Key ID、请求数、各单位用量之和、费用之和。为限制扫描量，`to - from` 不得超过 31 天，否则返回错误 |

### 管理 API（spec/09）

| 编号 | 规则 |
|:--|:--|
| ADM-29 | `GET /admin/api/keys/stats?pool=&from=&to=`：`from`、`to` 缺省为最近 24 小时，跨度 ≤ 31 天，否则 400。每把 Key：`{id, pool, name, requests, usage: {单位: 数量}, cost_nano_usd, attempts_24h, failures_24h: {种类: 次数}, failure_rate_24h, last_failure?: {kind, at}}`。`attempts_24h` 等始终是最近 24 小时（内存统计），不随 `from`、`to` 变化 |
| ADM-30 | `GET /admin/api/pools`：每个配置中的池 `{pool, protocol, keys: {total, available, cooling, exhausted, expired, disabled, quarantined}, expiring_7d, requests_24h, failure_rate_24h, quota?: {unit, limit_per_key, period, used, left, next_reset?, burn_per_hour, runs_out_at?, lasts_until_reset}}`。口径：`available` 同 AL-01；`quota.used` 为本期全池已用；`left` 为可用 Key 的剩余；`burn_per_hour` 为最近 24 小时额度单位的用量 ÷ 24；`runs_out_at` 在速度 > 0 时给出；`lasts_until_reset` 在有重置时刻且（速度为 0 或用完时间晚于重置）时为 true |
| ADM-31 | 管理面 MCP 增加只读工具 `gateway_admin_pools`（ADM-30）与 `gateway_admin_key_stats {pool?, from?, to?}`（ADM-29） |
| UI-37 | Key 页：每个池卡片标题下显示池摘要（可用 / 总数；有额度时：剩余、每小时消耗、「预计 {时间} 用完」或「能撑到 {重置时间}」；有即将过期时：7 天内过期 N 把）。表格增加「24 小时」列：`请求数 · 失败率`，失败率 ≥ 20% 黄色、≥ 50% 红色，`title` 列出各类失败次数与最近一次失败。数据随页面每 10 秒的刷新一起更新（统计接口单独请求，失败时只隐藏这些列） |

## 5. 测试

- **调度器**：
  - 各种结果的计数，以及哪些结果不计入；
  - 跨小时桶轮换，满 24 小时后旧数据清零；
  - 并发 Finish 时计数不丢；
  - `SetKeys` 换对象时沿用统计。
- **存储**：`SumUsageByKey` 的时间范围、多单位用量、费用，以及超过 31 天时报错。
- **管理 API**：
  - 两个接口的口径，包括可用数、剩余额度、消耗速度；
  - 预计用完时间的三种情况：会用完、能撑到重置、速度为 0；
  - 参数校验；
  - 未配置额度的池不输出 `quota`。
- **端到端**：fake 上游，让一把 Key 返回 429、另一把成功：失败率与失败种类正确；用量与费用和计费一致；池的预计用完时间符合手算结果；两个 MCP 工具可以调用。
- **实测**：用真实 Tavily Key 发少量请求，在浏览器面板查看 Key 页的池摘要与「24 小时」列，并通过管理面 MCP 的 `gateway_admin_pools` 问「这个池这个月还够用吗」。

## 6. 实现中的补充

- 失败种类多了 `overloaded`（上游繁忙，草案遗漏），共七种（KS-02）。
- `SumUsageByKey` 按 Key ID 查询（与 T63 的 `SumKeyUsage` 相同），走已有的 `(key_id, created_at)` 索引，不需要新的迁移。
- 池视图中熔断器打开的 Key 归入 `cooling`（不算可用，与 AL-01 一致）。
- `NextReset` 对 `total` 周期先返回零值再做时区换算（单元测试发现：手工构造、没有时区的额度会触发空指针）。
- **改动了一处已有测试的期望**：管理面 MCP 的工具数量由 11 变为 13、只读工具由 8 变为 10（`TestAdminMCP`），原因是本卡新增两个只读工具（ADM-31），spec/14 AM-02 同步更新。
- 「24 小时」单元格不换行（窄窗口下实测发现会折成两行）。

### 6.1 实测结果（2026-11-14，真实 Tavily Key，全部通过）

配置：原生 `tavily` 池，86 把 Key，额度 `credits` 每月 1000、北京时间每月 1 日重置。

1. 2 次基础搜索 + 1 次高级搜索全部成功。
2. `GET /admin/api/pools`：86 把全部可用；本期已用 4 点、剩余 85,996；每小时约 0.17 点（4 ÷ 24）；`lasts_until_reset: true`，下次重置 `2026-11-30T16:00:00Z`（北京时间 12 月 1 日 0 点）。
3. `GET /admin/api/keys/stats?pool=tavily`：只有 `tv-001` 有用量（drain），3 次计费请求、4 点、失败率 0。
4. 管理面 MCP `gateway_admin_pools` 返回同样的内容（`lasts_until_reset: true`），可以据此回答「这个池这个月还够用吗」。
5. 浏览器面板：池卡片下显示「可用 86 / 86 · 剩余 85,996 credits · 每小时约 0.17 credits · 能撑到 2026/11/30 11:00:00 重置」（本机时区）；`tv-001` 的「24 小时」为「3 · 0.0%」，悬停显示「24 小时尝试 3 次」；未使用的 Key 为「0 · —」。
6. 日志与接口输出中没有任何 Key、账号或 Token。

## 7. 改动文件

- `internal/scheduler`：新增 `stats.go`、`stats_test.go`；`key.go`（`stats` 指针）、`scheduler.go`（Finish 计数、SetKeys 沿用）、`quota.go`（`NextReset` 的 total 判断提前）。
- `internal/store`：`store.go`（`SumUsageByKey`、`KeyUsage`、`MaxKeyUsageRange`）、`sqlite/usage.go`、`sqlite/usage_test.go`。
- `internal/admin`：新增 `pools.go`、`pools_test.go`；`server.go`（两个路由）；UI 的 `pages/keys.js`（池摘要、24 小时列）、`i18n.js`（`fmtNum` 与文案）、`style.css`；`ui_test.go`。
- `cmd/gateway`：`admin_mcp.go`（两个工具）、`admin_mcp_test.go`（断言与工具数量）、新增 `e2e_key_views_test.go`。
- 规格：spec/04（STO-22）、spec/07（§7.5 KS-01 ~ KS-03）、spec/09（ADM-29 ~ ADM-31、UI-37）、spec/11（T65 DONE）、spec/14（AM-02）；`docs/DEPLOYMENT.md`。
