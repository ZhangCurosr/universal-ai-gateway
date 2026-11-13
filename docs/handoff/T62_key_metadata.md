# T62 Key 元数据：过期时间与来源标签（M4，规格草案）

- **类型**：规格 + 实现。2026-11-13 用户批准 §6 的全部建议（「全部按建议」）。
- **前置**：无。总览见 [M4_key_pool_ops.md](./M4_key_pool_ops.md)。
- **约束**：不改内核与契约的导出 API；不改分层规则与 adaptertest 套件；不加依赖；数据面不访问数据库（过期判断只用内存中的时间）。

## 1. 目标

1. 每把上游 Key 可以带过期时间 `expires_at`（可空）和来源标签 `label`（可空，如 `signup-2026-11`）。
2. `gateway keys import` 能从账号文件的字段读取过期时间，默认跳过已过期的账号。
3. 过期的 Key 不参与调度，管理面显示为 `expired`，不需要人工停用。

## 2. 存储

新增迁移 `0002_key_metadata.sql`（只加列，旧数据不变）：

```sql
ALTER TABLE upstream_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0;  -- Unix 秒；0 = 不过期
ALTER TABLE upstream_keys ADD COLUMN label      TEXT    NOT NULL DEFAULT '';
```

- `store.UpstreamKey` 增加 `ExpiresAt time.Time`（零值 = 不过期）与 `Label string`。
- `store.UpstreamKeyUpdate` 增加 `ExpiresAt *time.Time`（指向零值表示清除）与 `Label *string`。
- 以上都是新增字段，已有签名不变。

## 3. 调度

- `scheduler.Key` 增加方法 `SetExpiresAt(t)` / `ExpiresAt()`（原子存储，零值 = 不过期），由 `LoadKeys` 设置；`NewKey` 签名不变。不用导出字段：SCH-16 规定导出字段放入池后不再修改，而过期时间需要在复用旧对象时更新。
- `usable()` 增加一条：`!k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt)` → 不可用。
- 过期是**计算出来的状态**，不写回数据库（见 §6 决策 1）；`KeyStatus` 增加 `Expired bool`。
- 池里所有 Key 都不可用时，`noKey` 的错误说明里加上过期数量，便于排查（只含数量，不含 Key 信息）。
- `SetKeys` 复用旧对象时同时更新过期时间（不属于 SCH-16 的身份字段），冷却、熔断与 `inFlight` 都保留（spec/07 SCH-21）。

## 4. 管理面

| 接口 | 变化 |
|:--|:--|
| `POST /admin/api/keys` | 可选 `expires_at`（RFC 3339）与 `label` |
| `PATCH /admin/api/keys/{id}` | 同上；`expires_at: null` 清除过期时间 |
| `GET /admin/api/keys` | 每项增加 `expires_at`（没有时省略）、`label`；`state` 在过期时显示 `expired`，库中状态放在 `stored_state`（沿用 HR-19 对 `orphaned` 的做法） |
| `POST` 遇到重复 Key（409） | 响应增加已存在 Key 的 `id`，供导入命令更新元数据（见 §6 决策 2） |

- Web UI 的 Key 页增加「过期时间」列，标签显示在名称下方，有两种以上标签时可以筛选；新增与编辑表单增加这两项（spec/09 UI-33）。
- 管理面 MCP 的 `gateway_admin_list_keys` 原样返回这两个字段，工具说明已更新；没有新增工具。

## 5. 导入命令

新增参数：

| 参数 | 含义 |
|:--|:--|
| `--expires-field F` | jsonl：过期时间所在字段。接受 RFC 3339、Unix 秒、`YYYY-MM-DD HH:MM:SS`、`YYYY-MM-DD` |
| `--tz Z` | 不带时区的时间按此时区解释，默认本机时区，例如 `+08:00` |
| `--include-expired` | 默认跳过已过期账号（计入 `skipped`，原因 `expired`）；加上后照样导入 |
| `--label L` | 所有导入的 Key 打上标签 L |
| `--label-field F` | jsonl：用行内字段作为标签（与 `--label` 互斥） |
| `--update` | 遇到已存在的 Key 时更新它的过期时间与标签，计入 `updated` |

- 输出在原有计数之后追加 `, updated U, expired E`（原有前缀不变，旧脚本与测试照常匹配）；仍然只打印行号与原因，绝不打印行内容或 Key。
- `YYYY-MM-DD` 解释为当天 0 点（偏保守：宁可早一点认为过期）；Unix 时间 ≥ 10^11 视为毫秒。
- 时间字段无法解析时跳过该行（原因 `bad expires_at`），不猜测。

## 6. 决策（已批准）

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 过期 Key 怎么处理 | A. 只在内存中判断为不可用，库里状态不变；B. 后台定时把它改成 `disabled` 写回库 | **A**：不需要后台写库，延长过期时间后 Key 立即恢复 |
| 2 | 重复导入时更新元数据放在哪张卡 | A. T62 加 `--update`；B. 留给 T64 的 `keys sync` | **A**：改动小，T64 直接复用 |
| 3 | 不带时区的时间默认按什么解释 | A. 本机时区；B. 固定 UTC | **A**，并在帮助中说明可以用 `--tz` |
| 4 | 快过期提醒 | A. T62 只做过期判断，提醒放到 T64 的告警；B. T62 一并做 | **A** |

## 7. 改动文件

- `internal/store/store.go`、`internal/store/sqlite/keys.go`、`internal/store/sqlite/migrations/0002_key_metadata.sql`（新增）及测试。
- `internal/scheduler/key.go`、`internal/scheduler/scheduler.go` 及测试。
- 装配处把 `ExpiresAt` 传给 `scheduler.Key`（`cmd/gateway` 中加载 Key 的代码）。
- `internal/admin/keys.go`、`internal/admin/types.go`、`internal/admin/ui/pages/keys.js`、管理面 MCP 的 Key 工具及测试。
- `cmd/gateway/keys_import.go`、`keys_import_meta_test.go`（新增）、`admin_mcp.go`（工具说明）；`e2e_rest_test.go` 的 `keyViewItem` 增加两个字段；`.claude/skills/add-api/SKILL.md`。
- `spec/04` STO-08、STO-17 与表结构；`spec/07` SCH-21；`spec/09` ADM-13、ADM-22、UI-33 与数据结构；`spec/14` CLI-11；`docs/DEPLOYMENT.md` 导入一节。

## 8. 验收

- [x] 迁移只加带默认值的列；迁移前写入的行读出为不过期、无标签（`TestUpstreamKeyMetadataDefaults`）。
- [x] 调度：过期 Key 不被选中，到期时刻即过期；延长过期时间后立即可用；复用旧对象时冷却与 `inFlight` 保留；严格亲和不换 Key；全部过期时错误消息带数量（`expiry_test.go`）。
- [x] 导入：RFC 3339、Unix 秒与毫秒、三种本地时间格式、仅日期、`--tz`、默认跳过过期、`--include-expired`、`--label` / `--label-field`、`--update`、5 种非法参数组合各有用例；无法解析的时间跳过且不回显行内容。
- [x] `go run ./tools/check` 全绿。
- [x] 实测（2026-11-13，15/15 通过）：MinerU 账号 CSV 只做格式转换（CSV → JSONL，保留原始 `expires_at`，**不再过滤**），`--expires-field expires_at --tz +08:00` 导入 87 把，过期时间与 Python 独立解析的结果逐一相同；`--update` 更新 87 把，普通重复导入计为 87 个 duplicate；本地生成的假过期账号默认被跳过，`--include-expired` 导入后显示 `expired`；把一把真实 Key 的过期时间改到过去立即变为 `expired`，改回后立即恢复；经这批 Key 实际解析一次成功；导入输出与网关日志中没有 Key 或账号信息。

## 9. 已知限制

- 导入命令只读 JSONL 与纯文本，CSV 账号文件仍需先转换格式（不需要再过滤过期账号）。T64 的 `keys sync` 可以考虑直接支持 CSV。
- 本次实测的账号文件里没有已过期的账号，跳过逻辑用本地生成的假账号验证。
- Web UI 的改动通过了 Node 语法检查与 Go 端的 UI 测试，没有在浏览器里登录查看（登录需要在浏览器中输入管理员 Token）。
- 快过期提醒留给 T64（决策 4）。
