# T79 用量统计（Token 与其他计量单位）：趋势、多维分组、与协议无关（已批准）

- **类型**：规格 + 实现。2026-11-14 用户批准全部建议（「全部按建议」）。
- **前置**：T06（计费事件刷盘）、T65（Key 视图）。
- **约束**：微内核（AGENTS §2.1）：内核、路由、管理面后端**不出现 token 专用逻辑**，统计只认计量单位；不加依赖；数据面不访问数据库；不改 `internal/core` / `internal/protocol` 的导出 API。

## 1. 现状

- 每个请求结算时写一条 `usage_events`：客户端、Key、服务名（model）、操作、**用量 JSON（计量单位 → 数量）**、成本、用量来源（上游报告 / 估算）、时间。Token 数据其实已经完整记录，缺的是「看」。
- 现有查询只有两种：`GET /admin/api/usage` 按 (客户端, 服务) 汇总一段时间；Key 视图（T65）按 Key 汇总。
- 「用量」页面把四个 token 单位**写死在前端**（`TOKEN_UNITS`），其他单位挤在「其他」一栏。将来接入的适配器若报告新单位（例如 `audio_input_tokens`），页面不会单独显示。

缺少的：按天 / 小时的趋势、按 Key / 池 / 服务 / 客户端任意分组、token 合计、估算用量占比、导出。

## 2. 设计原则：统计与协议无关（回答「多适配器接入后也能用」）

1. **后端只聚合「单位 → 数量」映射，从不枚举单位。** SQL 用 SQLite 的 `json_each(usage)` 展开后 `GROUP BY` 单位名：适配器报告什么单位就统计什么单位，新增适配器不需要改统计代码。
2. **单位的语义由命名约定统一**（写进 spec/06 §1，新增 PRC-06 / PRC-07）：
   - **PRC-06**：表示 token 的单位名必须以 `_tokens` 结尾；同一个名字在所有适配器中含义相同（如 `input_tokens` 一律不含缓存读写，PRC-02 已规定）。适配器负责把上游的字段换算成这些名字。
   - **PRC-07**：同一请求的各单位**互不包含**（例如推理 token 若单独报告，`output_tokens` 中就不能再计入），所以「token 合计」= 所有 `*_tokens` 单位之和，不会重复计算。
3. **「是不是 token」只在一个地方判断**：`pricing.UnitKind(unit)`（`tokens` / `count` / `other`，按 PRC-06 的后缀），与单位常量放在一起；管理面把它作为数据下发，前端按它分组显示，不再写死单位列表。
4. **单位目录**：`GET /admin/api/usage/stats` 的响应附带 `units`：当前配置中各池协议的 `Egress.Units()` ∪ 接口声明（`models[].tools`）中的单位 ∪ 查询结果中实际出现的单位（已删除的协议或池的历史数据也能显示）。每个单位带 `kind` 和报告它的协议列表。
5. **守护测试**（`cmd/gateway`，组装适配器的地方）：遍历所有注册的出站适配器，`Units()` 中每个单位都满足 `pricing.ValidUnit`；看上去是 token 的单位（含 `token`）必须以 `_tokens` 结尾。新适配器违反约定时测试失败，而不是统计悄悄变错。

## 3. 接口

### 3.1 存储（`store.Store` 新增方法，STO-23）

```go
// AggregateUsage 按 q.GroupBy 汇总 [From, To) 内的计费事件；用量按单位求和（json_each），与协议无关。
AggregateUsage(ctx context.Context, q UsageAggQuery) ([]UsageBucket, error)

type UsageAggQuery struct {
    From, To time.Time
    GroupBy  []string       // 取值：time、client、model、key、pool；可组合，最多 3 个
    Interval string         // time 分组时：hour | day
    TZ       *time.Location // 按天分组的日界（缺省 UTC）
    ClientID int64          // 过滤，0 = 全部
    Model    string         // 过滤，空 = 全部
    Pool     string         // 过滤，空 = 全部
}
type UsageBucket struct {
    Time              time.Time // time 分组时为桶的起点
    ClientID, KeyID   int64
    Model, Pool       string    // pool 取 upstream_keys.pool；Key 已删除时为 ""
    Requests          int64
    EstimatedRequests int64     // usage_source = estimated 的请求数
    CostNanoUSD       int64
    Usage             map[string]int64
}
```

- 时间桶在 SQL 中计算：`(created_at + 时区偏移) / 桶长 * 桶长`（偏移按 `TZ` 在区间起点的偏移；跨夏令时的区间按起点偏移，SPEC-GAP 记录）。
- 新增迁移 `0005_usage_time_index.sql`：`CREATE INDEX idx_usage_time ON usage_events(created_at)`（现有索引都以 client_id / key_id 开头，全体客户端按时间查询用不上）。
- 上限：`hour` 分组区间不超过 7 天，`day` 不超过 92 天（与现有 `/usage` 一致）；结果行数不超过 10,000（超过 → 400，提示缩小区间或减少分组）。

### 3.2 管理面（ADM-32 ~ ADM-34）

| 编号 | 接口 | 说明 |
|:--|:--|:--|
| ADM-32 | `GET /admin/api/usage/stats?from&to&group=time,model&interval=day&tz=+08:00&client_id&model&pool` | 返回 `{"items": [UsageBucket...], "units": [{"name", "kind", "protocols": [...]}], "totals": {"requests", "estimated_requests", "cost_nano_usd", "usage", "tokens"}}`；`totals.tokens` = `kind == tokens` 的单位之和（PRC-07） |
| ADM-33 | 同上加 `format=csv` | CSV 导出：分组列 + 请求数 + 估算请求数 + 成本（USD）+ 每个单位一列（列按 `units` 顺序：tokens 在前，其余按名称） |
| ADM-34 | 管理面 MCP 工具 `gateway_admin_usage_stats`（只读，spec/14 AM-02 增加一项） | 参数同 ADM-32；命令行短名 `gateway admin call usage_stats --args '{"group":"time,model","interval":"day"}'` |

现有 `GET /admin/api/usage` 保持不变（兼容）。

### 3.3 网页（UI-41）

「用量」页面改为：

- 顶部：时间范围（今天 / 7 天 / 30 天 / 自定义）、分组（服务 / 客户端 / 池 / Key）、过滤；
- 统计卡：请求数、**Token 合计**、输入 / 输出 / 缓存 token、成本、估算用量占比；
- **趋势图**：按天（7 天以内可切换按小时）的 token 堆叠柱状图（每个 token 单位一种颜色），非 token 单位（`credits`、`pages`、`requests`）在下拉中切换显示；用内联 SVG 绘制，不加前端依赖；
- 明细表：列由 `units` 动态生成（token 单位在前），合计行；「导出 CSV」按钮。

## 4. 改动文件

- `internal/pricing/pricing.go`：`UnitKind` 与常量 `KindTokens` / `KindCount` / `KindOther`（+ 测试）。
- `internal/store/store.go`（接口与类型）、`internal/store/sqlite/usage.go`、迁移 `0005`（+ 测试）。
- `internal/admin/usage.go`（ADM-32、ADM-33）、`internal/admin/server.go`（路由）、管理面 MCP 工具注册处（ADM-34）、`internal/admin/ui/pages/usage.js` 与 i18n 文案。
- `cmd/gateway`：把单位目录（协议 → 单位、接口声明的单位）注入管理面依赖；守护测试（§2.5）。
- 规格：spec/06 §1（PRC-06、PRC-07）、spec/04（STO-23）、spec/09（ADM-32 ~ 34、UI-41）、spec/14 AM-02（新工具）、spec/11 T79；`operate-gateway` 技能加「查看 token 用量」场景。

## 5. 测试

- `UnitKind` 各分支；守护测试覆盖全部已注册适配器。
- `AggregateUsage`：各分组组合、按天 / 按小时、`+08:00` 日界、过滤、Key 已删除时 pool 为空、估算请求数、未知单位（模拟未来适配器报告 `audio_input_tokens`）原样出现在结果与 `units` 中、行数上限。
- ADM-32 / 33：参数校验（区间上限、非法分组、时区）、`totals.tokens`、CSV 列顺序与转义。
- 前端：静态规则检查（现有 UI 测试）；实测中在浏览器里看趋势图与明细表。

## 6. 实测（验收）

用 `tools/devgw` 与假上游：openai / anthropic / responses 三种协议各发若干请求（含流式中断产生的估算用量），再加一条 Tavily 真实请求（`credits`），检查：按天与按服务的统计与发出的请求一致；token 合计等于各 token 单位之和；新单位不用改代码就出现；CSV 能用表格软件打开。

## 7. 待决策

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 「是不是 token」怎么判断 | A. 单位命名约定 `*_tokens` + `pricing.UnitKind` 一处判断；B. 给 `protocol.EgressAdapter` 增加单位描述方法 | **A**：`internal/protocol` 是冻结的契约，B 要改所有适配器；A 只需要约定加守护测试，现有单位已全部符合 |
| 2 | 聚合在哪里做 | A. SQL `json_each` 展开后 GROUP BY；B. 读出 JSON 在 Go 里累加（现有 `/usage` 的做法） | **A**：行数多时快得多，且同样与单位无关 |
| 3 | 要不要预先汇总表（按小时滚动写入） | A. 先不做，直接查明细 + 新时间索引；B. 现在就加汇总表 | **A**：单机 SQLite 在 92 天、百万行级别直接聚合可接受；实测慢再加 |
| 4 | 普通客户端能否查自己的用量（另一个项目想知道自己用了多少 token） | A. 本卡只做管理面；B. 本卡同时加数据面 `GET /v1/usage`（需要读数据库，属于数据面访问数据库的例外，同 T69） | **A**，另开一张卡：涉及数据面读库的例外与限流，单独审批更清楚 |
| 5 | 趋势图怎么画 | A. 内联 SVG 手写；B. 引入图表库 | **A**：前端无构建、无依赖是现有约定 |
| 6 | 旧的 `GET /admin/api/usage` | A. 保留不动；B. 改为调用新接口 | **A**：已有脚本与测试依赖它 |

## 8. 实现说明（2026-11-15）

- 文件：`internal/pricing`（`UnitKind`、`SortUnits`）；`internal/store`（`AggregateUsage`、迁移 0005）；`internal/admin/usage_stats.go`（ADM-32、ADM-33）；`cmd/gateway/usage_units.go`（单位目录）与 `admin_mcp.go`（ADM-34）；`internal/admin/ui/pages/usage.js`、`style.css`、`i18n.js`（UI-41）。
- 已有测试的期望值变更（规格批准的新增工具所致）：`TestAdminMCP` 的工具数 13 → 14、只读工具数 10 → 11。
- 页面的单位中文名（输入、缓存读……）只是显示用的翻译，没有的单位直接显示原名；页面不判断单位种类，只读接口给出的 `kind`。
- SPEC-GAP：时区偏移取区间起点的偏移，区间跨夏令时切换时，切换后的日界偏一小时（中国不实行夏令时，不受影响）。

## 9. 实测结果（2026-11-15，本机 Windows，`tools/devgw` + 假上游 + 真实 Tavily）

- 流量：openai 非流式 3 次、流式 2 次（`echo:abcdefgh`），anthropic 2 次，responses 2 次，Tavily 搜索 1 次（MCP `tavily_search`，86 把 Key 由 `keys import` 导入，只输出计数）。
- 脚本 10/10 通过：请求数 10；Token 合计 128 = 各 `*_tokens` 单位之和；按服务 fake-llm 72、fake-claude 28、fake-resp 28（与假上游报告的用量逐一相符）；Tavily 记 `credits` = 1、不计入 Token；单位目录顺序正确，`credits` 的 kind 为 other、协议 tavily；按小时 + `+08:00` 分桶；按池与 Key 分组 4 行；CSV 带 BOM、表头正确、4 行数据；非法分组返回 400。
- `gateway admin call usage_stats --args '{"group":"model","tz":"+08:00"}'` 返回 4 行、Token 128。
- 未实测：流中断产生的估算用量（假上游总是完整返回；估算请求数的统计由单元测试覆盖）。
- 未在浏览器中检查新的「用量」页面（用户选择跳过）；页面的静态规则与文案由 `TestUIUsageStatsPage` 检查，数据由上面的接口实测验证。
