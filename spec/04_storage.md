# 04 存储（Storage）

代码位置：接口与记录类型 `internal/store/store.go`；实现 `internal/store/sqlite/`；迁移脚本 `internal/store/sqlite/migrations/*.sql`。

## 1. SQLite 连接

### 1.1 打开数据库（已提供参考实现 `sqlite.Open`）

- 驱动名 `sqlite`（`modernc.org/sqlite`）。DSN：`file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate`。
- 连接池：`SetMaxOpenConns(4)`、`SetMaxIdleConns(4)`、`SetConnMaxLifetime(0)`。
- **写操作必须在事务中**，且事务用 `_txlock=immediate`（开始即拿写锁，避免死锁升级）。
- 打开后立即执行迁移（§2）。

### 1.2 只有控制面访问数据库

调用方只有：`cmd/gateway`（启动加载）、`admin`（管理 API）、`ledger.Flusher`、`audit.Writer`、`auth` 索引重载。数据面请求路径**禁止**调用 `store` 的任何方法（ARCH-01）。

### 1.3 数据目录锁（已提供参考实现 `sqlite.AcquireDirLock`）

- 在 `data_dir/gateway.lock` 打开一个**独立的** SQLite 文件，设置 `PRAGMA locking_mode=EXCLUSIVE` 并执行一次写入，保持该连接直到进程退出。
- 第二个进程尝试获取时会得到 `database is locked`，`AcquireDirLock` 返回 `ErrDataDirLocked`。
- 进程崩溃时操作系统自动释放文件锁，无需清理残留锁文件。

## 2. 迁移

- 迁移脚本以 `embed.FS` 嵌入，文件名 `NNNN_描述.sql`（4 位数字，从 `0001` 开始，连续不跳号）。
- `schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)` 记录已执行版本。
- 启动时按版本升序执行未执行的脚本，**每个脚本一个事务**，执行成功后插入版本号。
- **已发布的迁移脚本禁止修改**。改表结构只能新增脚本。
- 数据库中的版本号大于程序已知的最大版本时，`Open` 返回错误（防止旧程序打开新库）。

## 3. 表结构（`0001_init.sql`，已提供）

时间一律存 **Unix 毫秒**（`INTEGER`），Go 侧用 `time.UnixMilli` / `t.UnixMilli()` 转换；金额一律 nano-USD（`INTEGER`）。

```sql
CREATE TABLE clients (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL,
    role            TEXT    NOT NULL CHECK (role IN ('user', 'admin')),
    token_hash      TEXT    NOT NULL UNIQUE,   -- hex(HMAC-SHA256(pepper, token))
    token_display   TEXT    NOT NULL,          -- "uai_live_ab12cd…"
    quota_unlimited INTEGER NOT NULL DEFAULT 0 CHECK (quota_unlimited IN (0, 1)),
    quota_nano_usd  INTEGER NOT NULL DEFAULT 0 CHECK (quota_nano_usd >= 0),
    rpm_limit       INTEGER NOT NULL DEFAULT 60 CHECK (rpm_limit >= 0),  -- 0 = 不限
    allowed_models  TEXT    NOT NULL DEFAULT '[]',                       -- JSON 数组；空数组 = 全部允许
    disabled        INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE upstream_keys (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    pool          TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    fingerprint   TEXT    NOT NULL UNIQUE,   -- hex(HMAC-SHA256(pepper, secret))，用于去重
    secret_hint   TEXT    NOT NULL,          -- obs.MaskSecret(secret)
    kek_version   INTEGER NOT NULL,
    wrapped_dek   BLOB    NOT NULL,
    ciphertext    BLOB    NOT NULL,
    weight        INTEGER NOT NULL DEFAULT 1 CHECK (weight >= 1),
    max_in_flight INTEGER NOT NULL DEFAULT 16 CHECK (max_in_flight >= 1),
    state         TEXT    NOT NULL DEFAULT 'ready' CHECK (state IN ('ready', 'quarantined', 'disabled')),
    state_reason  TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX idx_upstream_keys_pool ON upstream_keys(pool);

-- 0002_key_metadata.sql（T62）：只加列，旧数据不变
ALTER TABLE upstream_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0; -- Unix 秒；0 = 不过期
ALTER TABLE upstream_keys ADD COLUMN label      TEXT    NOT NULL DEFAULT '';  -- 来源标签，可空

-- 0004_usage_key_index.sql（T63）：按 Key 与时间重算周期额度用量
-- CREATE INDEX idx_usage_key_time ON usage_events(key_id, created_at);

-- 0003_runtime_state.sql（T68）：调度器运行时状态的快照，由控制面整体替换
CREATE TABLE key_runtime (
    key_id         INTEGER PRIMARY KEY,
    cooldown_until INTEGER NOT NULL,   -- Unix 毫秒；0 = 没有冷却
    auth_failures  INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE TABLE task_pins (
    affinity_key TEXT    PRIMARY KEY,  -- 内核算出的亲和键（哈希），不含任务 ID 明文
    key_id       INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL      -- Unix 毫秒
);

CREATE TABLE usage_events (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id         TEXT    NOT NULL UNIQUE,  -- 幂等键
    client_id          INTEGER NOT NULL,
    key_id             INTEGER NOT NULL,          -- 0 = 未选中 Key
    model              TEXT    NOT NULL,
    op                 TEXT    NOT NULL,          -- 操作名（protocol.Envelope.Op）
    usage              TEXT    NOT NULL,          -- JSON 对象：计量单位 → 数量，只含 > 0 的单位，键按字母序
    cost_nano_usd      INTEGER NOT NULL,
    usage_source       TEXT    NOT NULL CHECK (usage_source IN ('upstream', 'estimated')),
    created_at         INTEGER NOT NULL
);
CREATE INDEX idx_usage_client_time ON usage_events(client_id, created_at);

CREATE TABLE audit_records (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id        TEXT    NOT NULL,
    client_id         INTEGER NOT NULL,
    key_id            INTEGER NOT NULL,
    model             TEXT    NOT NULL,
    op                TEXT    NOT NULL,   -- 操作名（Envelope.Op）
    ingress_protocol  TEXT    NOT NULL,
    upstream_protocol TEXT    NOT NULL,
    status            INTEGER NOT NULL,
    error_code        TEXT    NOT NULL,
    outcome           TEXT    NOT NULL,   -- 最后一次尝试的 OutcomeKind.String()；未尝试为空串
    attempts          INTEGER NOT NULL,
    stream            INTEGER NOT NULL,
    ttfb_ms           INTEGER NOT NULL,   -- 上游首字节耗时；无则 -1
    latency_ms        INTEGER NOT NULL,
    created_at        INTEGER NOT NULL
);
CREATE INDEX idx_audit_time ON audit_records(created_at);

CREATE TABLE system_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
```

说明：
- 上游 Key 的 `cooldown`、`open`、`half_open` 等**瞬时状态只在内存**；数据库 `state` 只记录需要跨重启保留的状态：`ready`、`quarantined`（自动隔离，需人工恢复）、`disabled`（管理员停用）。
- `allowed_models` 用 JSON 文本存储；Go 侧是 `[]string`，`nil` 与空切片都写为 `[]`。

## 4. 仓储接口（`internal/store/store.go`，已提供签名）

```go
type Store interface {
    // 客户端
    CreateClient(ctx context.Context, c *Client) (int64, error)
    GetClient(ctx context.Context, id int64) (*Client, error)
    ListClients(ctx context.Context) ([]Client, error)
    UpdateClient(ctx context.Context, c *Client) error
    CountAdmins(ctx context.Context) (int, error)

    // 上游 Key
    InsertUpstreamKey(ctx context.Context, k *UpstreamKey, seal SealFunc) (int64, error)
    ListUpstreamKeys(ctx context.Context) ([]UpstreamKey, error)
    GetUpstreamKey(ctx context.Context, id int64) (*UpstreamKey, error)
    UpdateUpstreamKeyMeta(ctx context.Context, id int64, upd UpstreamKeyUpdate) error
    SetUpstreamKeyState(ctx context.Context, id int64, state, reason string) error

    // 计费
    InsertUsageEvents(ctx context.Context, events []UsageEvent) error
    SumCostByClient(ctx context.Context) (map[int64]int64, error)
    QueryUsage(ctx context.Context, q UsageQuery) ([]UsageSummary, error)

    // 审计
    InsertAuditRecords(ctx context.Context, recs []AuditRecord) error
    DeleteAuditBefore(ctx context.Context, before time.Time) (int64, error)

    // 设置
    GetSetting(ctx context.Context, key string) (string, bool, error)
    PutSetting(ctx context.Context, key, value string) error

    Close() error
}

// SealFunc 在插入事务中、拿到自增 id 之后被调用，返回该行的密文。
type SealFunc func(id int64) (crypto.Sealed, error)
```

### 4.1 方法语义（逐条实现，逐条测试）

| 编号 | 方法 | 语义 |
|:--|:--|:--|
| STO-01 | `CreateClient` | 插入一行，忽略 `c.ID`，设置 `created_at = updated_at = now`，返回新 id。`token_hash` 重复返回 `ErrConflict` |
| STO-02 | `GetClient` | 不存在返回 `ErrNotFound` |
| STO-03 | `ListClients` | 按 `id` 升序返回全部（含禁用的） |
| STO-04 | `UpdateClient` | 按 `c.ID` 更新 `name, role, token_hash, token_display, quota_unlimited, quota_nano_usd, rpm_limit, allowed_models, disabled`，刷新 `updated_at`；不存在返回 `ErrNotFound` |
| STO-05 | `CountAdmins` | 返回 `role='admin' AND disabled=0` 的行数 |
| STO-06 | `InsertUpstreamKey` | **单个事务**内：① 插入一行，`wrapped_dek`/`ciphertext` 先写空 BLOB `x''`；② 用新 id 调用 `seal(id)`；③ `UPDATE` 写入 `kek_version, wrapped_dek, ciphertext`；④ 提交。`seal` 返回错误则回滚并原样返回该错误。`fingerprint` 重复返回 `ErrConflict` |
| STO-07 | `ListUpstreamKeys` | 按 `id` 升序返回全部，包含密文字段 |
| STO-08 | `UpdateUpstreamKeyMeta` | 只更新 `UpstreamKeyUpdate` 中非 nil 的字段（`Name`、`Weight`、`MaxInFlight`、`ExpiresAt`、`Label`；`ExpiresAt` 指向零值表示清除过期时间）；不存在返回 `ErrNotFound` |
| STO-17 | 过期时间与标签（T62） | `UpstreamKey.ExpiresAt` 零值表示不过期，库中存 0；非零值按 Unix 秒存取（读出为 UTC，精度到秒）。`Label` 原样存取。过期只是元数据：仓储不据此修改 `state` |
| STO-18 | `SaveRuntimeState`（T68） | 单个事务内清空 `key_runtime`、`task_pins` 后批量插入（整体替换快照）；任一步失败回滚并返回错误。空输入表示清空 |
| STO-19 | `LoadRuntimeState(now)`（T68） | 返回 `cooldown_until > now` 或 `auth_failures > 0` 的行（冷却已结束的 `CooldownUntil` 为零值）与 `expires_at > now` 的固定记录；不修改数据 |
| STO-20 | 迁移 `0004_usage_key_index.sql`（T63） | 新增索引 `idx_usage_key_time ON usage_events(key_id, created_at)` |
| STO-21 | `SumKeyUsage(unit, since, keyIDs)`（T63） | 对 `created_at >= since` 且 `key_id` 在 `keyIDs` 中的事件，按 Key 求 `json_extract(usage, '$.<unit>')` 之和；没有用量的 Key 不出现。`unit` 必须匹配 `^[a-z][a-z0-9_]*$`，否则返回错误；JSON 路径作为 `?` 参数传入；每次最多 500 个 Key 的占位符，超出分批 |
| STO-22 | `SumUsageByKey(from, to, keyIDs)`（T65） | 按 Key 汇总 `[from, to)` 内、`key_id` 在 `keyIDs` 中的计费事件：请求数（事件数）、`usage` 各单位之和、费用之和；没有事件的 Key 不出现，按 Key ID 升序。`from` 必须早于 `to` 且跨度 ≤ 31 天，否则返回错误。用 `idx_usage_key_time`，每次最多 500 个 Key 的占位符 |
| STO-09 | `SetUpstreamKeyState` | `state` 不在 `ready/quarantined/disabled` 中返回 `ErrInvalidArgument`；不存在返回 `ErrNotFound` |
| STO-10 | `InsertUsageEvents` | 单个事务批量插入；使用 `INSERT OR IGNORE`，重复 `request_id` 静默跳过（幂等）；空切片直接返回 nil |
| STO-11 | `SumCostByClient` | `SELECT client_id, SUM(cost_nano_usd) ... GROUP BY client_id` |
| STO-12 | `QueryUsage` | 按 `UsageQuery{ClientID(0=全部), From, To}` 过滤 `created_at ∈ [From, To)`，按 `client_id, model` 分组（按此顺序排序）汇总请求数、成本与**各单位用量之和**（`Usage` map；可用 SQLite 的 `json_each(usage)` 在 SQL 中汇总，也可逐行解析后在 Go 中累加）；没有用量的组 `Usage` 为空 map |
| STO-16 | `InsertUsageEvents` | `usage` 列写 `json.Marshal(ev.Usage)`（Go 的 map 序列化按键排序，满足「键按字母序」）；`ev.Usage` 为 nil 时写 `{}` |
| STO-13 | `InsertAuditRecords` | 单个事务批量插入；空切片返回 nil |
| STO-14 | `DeleteAuditBefore` | 删除 `created_at < before` 的行，返回删除行数 |
| STO-15 | `GetSetting` / `PutSetting` | `PutSetting` 为 upsert；`GetSetting` 不存在时返回 `("", false, nil)` |

- 哨兵错误定义在 `store` 包：`ErrNotFound`、`ErrConflict`、`ErrInvalidArgument`。SQLite 唯一约束冲突（错误信息含 `UNIQUE constraint failed`，扩展错误码 2067）需转换为 `ErrConflict`——这是本仓库**唯一允许**检查驱动错误信息的地方，封装在 `sqlite.isUniqueViolation(err)` 中。
- 批量插入：每条记录一条 `INSERT`，复用同一个 `tx.PrepareContext` 语句。不要拼接多行 `VALUES`（参数个数上限与可读性问题）。
