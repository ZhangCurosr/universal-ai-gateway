# T61 基线、备份恢复最终验收报告（2026-11-14）

T61：**DONE**。完整检查、历史固定版本的两组各 30 次 Linux race、24 小时基线及落库核对、备份恢复演练均满足。收尾时尚未提交；用户于 2026-11-15 要求完成后自动提交并推送，本次据此整理为独立 T61 提交。

## 版本与授权

- 本机 git pull：`8162506` → `2267732c129c`（T72 池与服务编辑）；本地已有工作由 autostash 恢复。收尾分支 `task/t61-finish`。
- T61 原实现位于独立提交 `08d7d07`，此次恢复到最新工作区，未合并其他分支或改写历史；迁移支持更新为 4。
- 2026-11-13 用户已批准备份方案、256 MiB 上限、恢复后手工调整路径及文件范围。本轮另明确批准 T66 卡外测试时钟数据修复，断言未改。
- 两组 30 次 race 与全天负载固定在 `71f32a37a3334599f7739b390ddd97b9438aed03` + T55 修复补丁；补丁 SHA-256 `f822f53367007fb18ef4cc08cb65390ad46b64bc3dd57612f6f7aa1e27eb2d1c`，负载二进制 SHA-256 `7ce41273fd72282354168e1e3a8b4a67a2cdba2e05ca52bfe5a71fc8f44c2d0e`。
- 当前完整检查与重启覆盖 `2267732` + 恢复的 T61 + 已有本地改动。不能把历史全天结果描述为最新所有功能都已全天验证。
- MCP / YouTube、调度器已有源文件的收尾前后哈希一致；原有部署文档和任务表中的本地内容也保留。

## 环境

WSL2 Linux x86_64，Go 1.27.3，GCC 13.3，race 实际启用；常规构建与 Windows 交叉构建均为零 CGO。固定版本复测使用 `GOMAXPROCS=2`、`GOFLAGS='-p=2 -buildvcs=false'`（归档没有 Git 元数据）；当前完整检查使用正常 `go run ./tools/check`。

## 24 小时稳定性基线

| 项目 | 实测 |
|:--|:--|
| 开始 / 结束（北京时间） | 2026-11-13T16:10:44+08:00 / 2026-11-14T17:21:29+08:00 |
| 实际主动负载（monotonic） | 86400.529 秒，达到 86400 秒 |
| 墙钟跨度 | 90644.662 秒；与主动负载时钟不同，均原样保留 |
| 总请求 / 错误 | 172788 / 0 |
| 各 worker 请求 | 缓冲透传、SSE、转译、REST 各 43197 |
| 热重载 | 287 次 |
| 采样 | 8638 行，最大相邻采样间隔 10.186 秒（monotonic） |
| RSS 首次 / 最后 / 峰值 | 16.613 / 26.406 / 32.738 MiB |
| goroutine 首次 / 最后 / 峰值 | 31 / 25 / 31 |
| 延迟桶 P50 / P95 / P99 上界 | 2 / 50 / 50 ms；不是精确分位值 |
| 计费队列峰值 / 审计丢弃 | 2 / 0 |
| 网关 Warn / Error | 空日志 |
| 优雅退出后的 usage_events | 172788，与成功请求数一致 |
| 独立 SQLite 核对 | COUNT=172788；integrity_check=ok |

内存与 goroutine 分窗中位数：

| 窗口 | RSS | goroutine |
|:--|--:|--:|
| 0–1 小时 | 22.939 MiB | 25 |
| 1–2 小时 | 24.363 MiB | 25 |
| 12–13 小时 | 27.855 MiB | 25 |
| 23–24 小时 | 26.422 MiB | 25 |

负载为本机模拟上游、4 个固定 worker、合计目标 2 请求/秒，覆盖缓冲透传、SSE、Chat → Anthropic 转译与 REST；每 5 分钟热重载调度策略。没有真实上游调用或真实费用；所有凭据为合成值。前期有开发检查并行，机器并非独占压测机；这些数据不是生产吞吐承诺。

原始文件：`/tmp/uai-t61-results/soak-24h/summary.json`、`samples.csv`、`gateway_warnings.jsonl`、`data/gateway.db`。仓库保留本报告的汇总和摘要，避免收录数据库、主密钥或账号文件。

```json
{
  "summary.json": "b7e25d66a646b60c2eac274685869b5d8c34424721c13f49d388040ffd288768",
  "samples.csv": "af2ab7484bc536eb5c918cb8374c2e5ef88dba3a45d057d9be6f0acb32cd0850",
  "gateway_warnings.jsonl": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
}
```

## Linux 竞态与完整检查

| 步骤 | 结果 | DATA RACE | 耗时 |
|:--|:--|--:|--:|
| 固定版本完整检查 | 通过 | 0 | 54.029s |
| 5 个包，各 30 次 | 通过 | 0 | 474.471s |
| 15 个包，各 30 次 | 通过 | 0 | 83.058s |
| 当前工作区完整检查 | 通过，含技能同步、race、零 CGO 与架构检查 | 0 | 见日志 |
| T66 时间数据修复的定向 race | 30 次通过 | 0 | 见日志 |

### 01_check

实际命令：`go run ./tools/check`

```text
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/archtest	1.080s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	1.098s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.038s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/clientip	1.018s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/config	1.111s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	2.156s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/crypto	1.017s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	1.057s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ir	1.020s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx	1.020s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	1.327s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	1.256s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.075s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/obs	1.017s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/pricing	1.018s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/protocol	1.030s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.020s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	1.151s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/sse	1.017s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/store	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	2.478s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	1.368s
?   	github.com/ZhangCurosr/universal-ai-gateway/tools/check	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/tools/release	1.018s
ALL CHECKS PASSED
```
### 02_race_new

实际命令：`go test -race -count=30 -timeout=60m ./internal/metrics/... ./internal/audit/... ./internal/translate/... ./internal/admin/... ./cmd/gateway/...`

```text
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.886s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	3.463s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	8.615s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	48.688s
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	424.782s
```
### 03_race_core

实际命令：`go test -race -count=30 -timeout=60m ./internal/core/... ./internal/ledger/... ./internal/scheduler/... ./internal/auth/... ./internal/ratelimit/... ./internal/httpapi/... ./internal/mcp/... ./internal/store/sqlite/... ./internal/adapter/...`

```text
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	34.613s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	10.137s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	4.972s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.682s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.144s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	1.984s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	7.441s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	46.384s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.745s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/gemini	1.997s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	8.628s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	1.914s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	1.970s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/rest	6.222s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.673s
```

当前完整检查最后 10 行：

```text
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.041s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	1.174s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/sse	1.031s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/store	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	2.876s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	3.049s
?   	github.com/ZhangCurosr/universal-ai-gateway/tools/check	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/tools/release	1.028s
ok  	github.com/ZhangCurosr/universal-ai-gateway/tools/skillsync	1.027s
ALL CHECKS PASSED
```

定向回归：`go test -race -count=30 -run '^TestE2E_REST_DeclaredAPI$' ./cmd/gateway`。

```text
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	7.191s
```

## 备份恢复验收

- Linux 完整检查执行全部新增备份恢复测试：真实仓储的客户端 Token 哈希、Key 信封解密、持久化与待刷盘计费、运行时冷却 / 任务固定、WAL 快照、来源文件不变；以及篡改、异常头 / manifest / 成员、错口令、超限、已有文件、在线锁、同目标并发、失败回滚与日志脱敏。
- 当前已有迁移 `0004_usage_key_index.sql`，支持上限更新到 schema 4，迁移守卫仍比对实际迁移数。修复 Windows 盘符 URI，避免 `D:` 被解析为 URI authority。
- 正式 Windows gateway.exe 版本命令实际返回 `2267732-t61-local`。
- 用正式 exe 在本机 NTFS 临时目录执行原生 CLI 演练：合成 schema 4 数据库 → 加密备份 → 新目录恢复；master.key 与原配置字节一致，1 条计费事件金额 7 nano-USD 保留、数据库完整性为 ok；已有归档和错口令均拒绝，错口令不创建目标。测试数据已清理，没有读取真实 Key 明文或调用真实上游。
- Windows 测试程序的完整测试集曾被应用程序控制策略拦截（有一次 PowerShell 误返回 0，按日志未记通过）；本机没有找到预期的 Windows Go 安装。因此 Windows 仅声明正式 CLI 演练与版本检查通过，不宣称 ACL 或符号链接语义全部验证；没有修改安全策略。

```json
{
  "status": "passed",
  "platform": "Windows native gateway.exe",
  "schema": 4,
  "backup_restore_roundtrip": true,
  "master_and_config_preserved": true,
  "usage_events": 1,
  "cost_nano_usd": 7,
  "integrity_check": "ok",
  "existing_archive_rejected": true,
  "wrong_password_rejected_without_destination": true,
  "synthetic_data_only": true
}
```

## 编译与重启

已重新编译根目录 `gateway.exe`（Windows amd64、CGO_ENABLED=0）；当前实际服务为 WSL Linux 进程，所以另编译相同源码的 Linux 二进制后对既有启动器发送 SIGTERM，优雅停止并重新启动。沿用原配置、数据目录、Token 与主密钥。新启动失败时部署脚本会恢复旧 Linux 二进制，本次新版本验证成功。

| 项目 | 结果 |
|:--|:--|
| 运行版本 | 2267732-t61-local |
| 原 / 新 PID | 888188 / 1146849 |
| Web UI / 管理员健康检查 | 200 / 200 |
| 新配置编辑资源 | HTTP 返回字节与新源码哈希一致 |
| Key / 客户端数 | 272 / 1，重启前后一致 |
| 计费记录数 | 34323，重启前后一致 |
| 配置 / Token / 主密钥 | 重启前后文件哈希一致 |
| Windows exe SHA-256 | `191744751739b2478e6acc089ec31253e9ce0adbaa89a41bbab67e9bff4b6db4` |
| Linux 二进制 SHA-256 | `ea6fc9caef3f0a1fdec820ad7bbd9c777d248b27827d5fecf85a47b8765fad60` |

## 失败记录与修复

1. 既有 T66 测试固定凌晨 00:01 为重置时刻，却要求剩余冷却 >3600 秒；临近午夜时实际只剩约 3455 秒，导致完整检查失败。用户批准仅将测试重置时间设为当前 +12 小时；两把 Key 与 >3600 秒断言保持不变，30 次 race 回归及完整检查通过。文件：`cmd/gateway/e2e_rest_api_test.go`。
2. 原 T61 Windows 文件 URI 把盘符当 authority，首次原生测试出现 invalid uri authority: D:。修复 drive 路径前导斜杠后，正式 Windows CLI 备份恢复已通过。
3. 应用程序控制策略阻止部分测试程序启动。未把 shell 返回码当测试通过依据，没有关闭策略或伪造测试结果；最终通过的是正式 exe 的 CLI 演练。

## 验收逐项

| 验收项 | 状态 |
|:--|:--|
| 两组 Linux race 各满 30 次及固定版本证据 | 满足，20 个包均 ok |
| 24 小时内存、goroutine、延迟、热重载与落库核对 | 满足 |
| 已批准的离线加密、大小限制、拒绝覆盖和配置路径约定 | 满足 |
| 恢复保留凭据、账目、运行时状态及待刷盘事件 | 满足，Linux 测试 |
| 异常归档、错口令、已有目标、在线锁、并发和失败清理 | 满足，Linux 测试；Windows 正式 CLI 验证核心往返与两项负面行为 |
| 最新代码完整检查、零 CGO、分层、技能同步 | 满足 |
| 本机 git pull、重编译 gateway.exe、重启与页面核对 | 满足 |
| Windows ACL / 符号链接完整语义、断电注入 | 未验证，不属于首版承诺；限制已说明 |
| 本卡 TODO / SPEC-GAP | 无 |
| 卡片状态 | DONE |

多目录发布不承诺断电原子性；口令与备份应分开保管。归档上限 256 MiB，恢复后手工调整配置路径。源文件、依赖、公开接口与架构边界遵循已批准范围；唯一新增卡外测试修改已单独获用户批准。

## 最终源文件摘要

```json
{
  "cmd/gateway/main.go": "645941f2e07974b357572796ff981bde2a4d54ebc19ea0a5fa2212a780aa4e6b",
  "cmd/gateway/backup.go": "c7291bc4acf33af8f4c40d4d7a6c25f28929785e17fd984cc67be19ac98ed7e6",
  "cmd/gateway/restore.go": "d72e0061677552467e1eadaa095fc57d73814760faa8a76ca0d684de916c7517",
  "cmd/gateway/backup_test.go": "130c13526b9d4739f5aadc748f6201763de5d9ee71a4649e1613d3828404299a",
  "cmd/gateway/restore_test.go": "33906d1a8d85be6dc0a2400ea328d90d38ba5c524a208c692eec113943f713d9",
  "cmd/gateway/e2e_rest_api_test.go": "03dcc178d5e6902bc1dedbbd8974d4c280f1411b56ed1ee73b3194975497d5dc",
  "tools/soak/soak.py": "f68406c3421d31457baa5e53efcdac1b300f84f388d2b9dc8b08466f43b375c7"
}
```
