# T68 运行时状态持久化（M4，规格草案）

- **类型**：规格 + 实现。2026-11-13 用户批准 §7 的全部建议（「全部按建议」）。
- **前置**：无（T66、T67 已完成，本卡对它们的任务固定同样生效）。总览见 [M4_generalization.md](./M4_generalization.md)。
- **约束**：不改内核与契约的导出 API；不改分层规则与 adaptertest 套件；不加依赖。**数据面热路径不访问数据库**（AGENTS §3 #2）：写库只在控制面的后台 worker 与启动、关闭流程中进行。

## 1. 要解决的问题

调度器的运行时状态只在内存里，网关一重启（包括 T55 的进程内重启、升级、机器重启）就全部丢失：

| 状态 | 丢失后的后果 | 例子 |
|:--|:--|:--|
| Key 的冷却截止时间 | 日额度已用完的 Key 重新参与调度，每把都要再失败一次才冷却 | MinerU 87 把 Key 用完当天额度后重启，下一批请求先撞 87 次 -60018 |
| 连续鉴权失败计数 | 第一次 401 的「冷却 60 秒」记录丢失，坏 Key 要再失败两次才隔离 | 影响小，但与上一条同一机制 |
| 异步任务的固定记录 | 重启前创建的任务，查询返回 404（`unknown or expired task; resubmit`），只能重新提交（重新计费） | MinerU、T66 声明的异步任务、OpenAI Responses 的后台任务 |

T63（按周期额度调度）要记录每把 Key 在当前周期的用量，同样必须跨重启。本卡建立这套机制，T63 往里加一类数据即可。

**不在范围**：隔离状态（已由 `OnQuarantine` 写库，ST-06）；熔断器状态（窗口只有几十秒，见决策 1）；普通会话亲和（只影响缓存命中率）。

## 2. 设计

```
数据面（内存）                         控制面 worker（每 5 秒）               SQLite
Scheduler ── RuntimeSnapshot() ──────► 有变化才写 ── 单个事务替换 ──────► key_runtime / task_pins
          ◄─ RestoreRuntime() ──────── 启动时：SetKeys 之后、开始服务之前读取并恢复
                                       关闭时：数据面排空之后写最后一次
```

### 2.1 存储（迁移 `0003_runtime_state.sql`）

```sql
CREATE TABLE key_runtime (
    key_id         INTEGER PRIMARY KEY,
    cooldown_until INTEGER NOT NULL,   -- Unix 毫秒；0 = 没有冷却
    auth_failures  INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE TABLE task_pins (
    affinity_key TEXT    PRIMARY KEY,  -- 内核算出的亲和键：c<客户端ID>:j:<sha256 前 16 位>，不含任务 ID 明文
    key_id       INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL      -- Unix 毫秒
);
```

- `store.Store` 增加 `SaveRuntimeState(ctx, keys []KeyRuntime, pins []TaskPin) error`（单个事务：清空两张表后批量插入，整体替换）与 `LoadRuntimeState(ctx, now time.Time) ([]KeyRuntime, []TaskPin, error)`（只返回未过期的冷却与固定记录）。都是新增方法，已有签名不变。
- 数据量有上限：固定记录不超过亲和缓存容量（65536），Key 只写有冷却或有失败计数的。

### 2.2 调度器

| 编号 | 规则 |
|:--|:--|
| RT-01 | 亲和缓存的条目增加 `pinned` 标记：`Lease.Pin`（任务固定，SCH-18）写入的条目为 true，普通会话亲和为 false。普通亲和命中时不改变这个标记 |
| RT-02 | `Scheduler.RuntimeSnapshot() RuntimeState`（新增导出方法）：返回所有「冷却截止时间晚于现在或失败计数 > 0」的 Key，以及所有未过期的 `pinned` 条目（亲和键、Key ID、到期时间）。只读内存、逐个分片短暂加锁，不阻塞 `Acquire` |
| RT-03 | `Scheduler.RestoreRuntime(RuntimeState)`（新增导出方法）：只恢复当前存在的 Key ID；冷却截止时间已过的忽略；恢复的冷却截止时间不晚于「现在 + 池当前的 `cooldown.max`」（配置可能在重启期间改小，见决策 5）；失败计数原样恢复；固定记录按剩余有效期写回亲和缓存并标记 `pinned`。对已有的冷却只延长不缩短（与 `coolDown` 相同） |
| RT-04 | 快照与恢复都不碰数据库；数据库读写由装配代码负责 |

### 2.3 装配（`cmd/gateway`）

| 编号 | 规则 |
|:--|:--|
| RT-10 | 启动：`SetKeys` 之后、监听端口开始服务之前，`LoadRuntimeState` → `RestoreRuntime`，记一条 Info（恢复了几把冷却中的 Key、几条固定记录；只有数量）。读取失败只记 Warn 并继续启动（状态丢失不比现在更差） |
| RT-11 | 后台 worker `runtime_snapshot`：每 5 秒取一次快照，与上次写入的内容不同才写库（大部分时候不写）。写库失败记 Warn，下一次重试，不影响服务 |
| RT-12 | 关闭：数据面排空之后（关闭 3，与计费刷盘同一阶段）写最后一次快照。T55 的进程内重启同样走这个流程，新实例启动时读到旧实例最后写入的状态 |
| RT-13 | 进程崩溃（没有正常关闭）时，最多丢失最近 5 秒的变化 |
| RT-14 | 日志与快照中不出现任务 ID、Key 明文或客户端 Token；亲和键本身是哈希 |

## 3. 实现要点

- `internal/store`：类型 `KeyRuntime{KeyID, CooldownUntil, AuthFailures}`、`TaskPin{AffinityKey, KeyID, ExpiresAt}`；`internal/store/sqlite/runtime.go` 实现；迁移 `0003`。
- `internal/scheduler`：`affinityEntry.pinned`；`RuntimeState{Keys []KeyRuntime; Pins []PinRecord}`（调度器自己的类型，不依赖 store，装配代码负责转换）；`RuntimeSnapshot`、`RestoreRuntime`。
- `cmd/gateway/serve.go`：启动恢复、worker、关闭时的最后一次写入。
- T63 之后在 `RuntimeState` 中增加周期用量，复用同一个 worker 与事务。

## 4. 测试

- 存储：保存后读取；整体替换（第二次保存覆盖第一次）；过期记录不返回；空快照清空表；迁移在旧库上执行。
- 调度器：快照只含冷却中、有失败计数的 Key 与 `pinned` 条目（不含普通亲和、已过期条目）；恢复后 Key 继续冷却、严格亲和命中同一 Key；不存在的 Key ID 忽略；冷却按当前 `cooldown.max` 截断；只延长不缩短；与 `Acquire`、`Finish` 并发（Linux 下 `-race`）。
- 端到端：
  1. 两把 Key，一把冷却 1 小时 → 正常关闭 → 用同一数据目录启动 → 请求只落到另一把 Key，管理面显示冷却剩余约 1 小时。
  2. 创建异步任务（T66 声明式与 `mineru` 适配器各一个）→ 重启 → 查询仍落在创建任务的 Key 上。
  3. T55 进程内重启后同样保留。
  4. 快照没有变化时不写库（计数写入次数）。

## 5. 实测（验收）

真实 MinerU Key：提交异步任务 → 重启网关 → 查询直到完成，始终落在创建任务的 Key 上，没有 404；人为让一把 Key 进入冷却 → 重启 → 仍在冷却。日志中没有任务 ID、Key 或账号信息。

### 5.1 实测结果（2026-11-13，全部通过）

真实 MinerU Key（`mineru` 专用池与声明式 `rest` 池各 87 把），外加一把假 Token 的池：

1. 提交两个异步任务（`mineru` 适配器 `/v1/tasks` 与 T67 声明式上传 `/rest/mineru-api/tasks/file`）；假 Token 请求一次，第一次 401 后冷却 60 秒、失败计数 1。等 7 秒（周期快照已执行）。
2. **强制结束进程**（不经正常关闭，模拟崩溃）后重新启动：日志 `runtime state restored`，`cooling_keys=1`、`task_pins=2`、`keys_with_auth_failures=1`；假 Key 仍在冷却（剩余 32 秒）；两个任务的查询都落在创建它们的 Key 上，直到 `done`，没有 404。
3. 修改一个需要重启的字段（`limits.max_in_flight_requests`）并调用 `reload`，触发 T55 的**进程内重启**（RT-12 的关闭时写入）：重启成功后两个任务仍可查询。
4. 每次重启都记录了恢复；与运行时状态相关的日志中没有任务 ID；174 把真实 Key 全部保持可用；日志中没有 Key、Token 或账号信息。

### 5.2 已知限制

- 熔断器状态与普通会话亲和不保留（决策 1）。
- 进程崩溃时最多丢最近 5 秒的变化（RT-13）。
- 时间以本机时钟为准：重启期间系统时间被大幅调整时，冷却与固定记录的剩余时间会随之偏移。
- 请求日志（`path` 字段）本来就包含异步任务的查询路径，其中含任务 ID；这是已有行为，不属于本卡（RT-14 只约束运行时状态的快照与日志）。

## 6. 改动文件

- `internal/store/store.go`、`internal/store/sqlite/runtime.go`（新增）、`migrations/0003_runtime_state.sql`（新增）、`keys_test.go`（`TestRuntimeState`）。
- `internal/scheduler/affinity.go`（`pinned` 标记、`pin`、`pinnedEntries`）、`scheduler.go`（`Lease.Pin` 改用 `pin`）、新增 `runtime.go`、`runtime_test.go`。
- `cmd/gateway/serve.go`（启动恢复、worker）、新增 `runtime_state.go`、`runtime_state_test.go`、`e2e_runtime_state_test.go`（正常关闭后重启，冷却与 3 个任务固定都保留）。
- `spec/04`（表结构与 STO 规则）、`spec/07`（新增 RT 规则）、`docs/DEPLOYMENT.md`（重启后保留哪些状态）、`spec/11`。

## 7. 决策（已批准）

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 保存哪些状态 | A. 冷却、失败计数、任务固定；B. 再加熔断器状态与普通会话亲和 | **A**：熔断窗口只有几十秒，重启后重新统计无害；会话亲和只影响缓存命中，数量大，不值得写库 |
| 2 | 写库方式 | A. 每 5 秒取快照，有变化才写，关闭时再写一次；B. 每次变化都写（像隔离那样经通道写库） | **A**：写库次数与请求量无关；崩溃最多丢 5 秒，代价是个别 Key 重启后再失败一次 |
| 3 | 快照间隔能否配置 | A. 固定 5 秒；B. 配置项，缺省 5 秒 | **A**：少一个旋钮；需要时再加 |
| 4 | 启动时读取失败 | A. 记 Warn 继续启动；B. 拒绝启动 | **A**：状态丢失只是回到现在的行为，不应因此停服 |
| 5 | 恢复的冷却超过当前配置的 `cooldown.max` 时 | A. 截断到「现在 + 当前上限」；B. 原样恢复 | **A**：管理员重启前调小上限，就是希望 Key 早点回来 |
