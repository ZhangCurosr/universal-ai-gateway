# T61 基线、备份恢复交付报告（2026-11-13，进行中）

## 状态与版本

T61 **DOING**：实现及短期检查通过，24 小时基线仍在后台运行，没有把启动或短测当作全天通过。

- 分支：`task/t61-baseline-and-backup`，尚未提交/推送。
- 基础提交：`71f32a37a3334599f7739b390ddd97b9438aed03`（T68）。
- 远端原始版本曾在 `TestSetPoolsConcurrent` 报告竞态；前一轮报告：`/tmp/T61_baseline_report_20261113.md`，原始日志 `/tmp/uai-t61-check.log`、`/tmp/uai-t61-repro.log`。
- 验证快照 = 基础提交 + 上次已授权的 T55 修复，补丁 SHA-256：`f822f53367007fb18ef4cc08cb65390ad46b64bc3dd57612f6f7aa1e27eb2d1c`。
- 快照归档 SHA-256：`8da75d9a444dbf8bf2012826e1f481df922701553923399c12563848d0a6aaae`。`/tmp/uai-t61-source.tar` 与 `/tmp/uai-t55-pending.patch` 可复现。
- 最终工作区的 T61 新命令及测试另外通过完整检查；源文件校验和见下文。不能把这些结果描述为远端未经修复的版本已通过。

## 环境与命令

- `Linux-6.18.54.1-microsoft-standard-WSL2-x86_64-with-glibc2.39`；`go version go1.27.3 linux/amd64`；GCC 13.3；CGO_ENABLED=1。
- 固定快照：`/tmp/uai-t61-fixed-20261113`。两组复测并行运行，每组 `GOMAXPROCS=2`、`GOFLAGS='-p=2 -buildvcs=false'`；缓存 `/tmp/go-build`。归档不含 Git 元数据，故关闭 VCS 信息嵌入。初次归档检查因 `/tmp/.git` 导致 VCS 查询失败，环境诊断日志保存在 `00_snapshot_vcs.log`；未改源码规避测试。
- 最终工作区：正常 `go run ./tools/check`（没有 -short），实际启用 race。
- Windows：`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /tmp/uai-t61-results/gateway-windows.exe ./cmd/gateway` 通过；没有在 Windows 运行或验证 ACL / 硬链接行为。

## 竞态验证

| 步骤 | 结果 | DATA RACE 段数 | 墙钟耗时 |
|:--|:--|--:|--:|
| 固定快照完整检查 | 通过 | 0 | 54.029s |
| 5 个新并发包，各 30 次 | 通过 | 0 | 474.471s |
| 15 个核心/适配器包，各 30 次 | 通过 | 0 | 83.058s |

### 01_check

命令：`go run ./tools/check`

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

命令：`go test -race -count=30 -timeout=60m ./internal/metrics/... ./internal/audit/... ./internal/translate/... ./internal/admin/... ./cmd/gateway/...`

```text
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.886s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	3.463s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	8.615s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	48.688s
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	424.782s
```
### 03_race_core

命令：`go test -race -count=30 -timeout=60m ./internal/core/... ./internal/ledger/... ./internal/scheduler/... ./internal/auth/... ./internal/ratelimit/... ./internal/httpapi/... ./internal/mcp/... ./internal/store/sqlite/... ./internal/adapter/...`

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

## 备份与恢复

用户已批准离线加密方案、256 MiB 上限、手工调整配置路径及文件范围。主要变化：

- `cmd/gateway/backup.go`、`restore.go`：离线目录锁；只读 SQLite + 参数化 VACUUM INTO 一致性快照；PBKDF2-HMAC-SHA256 + AES-256-GCM 认证；固定 USTAR 白名单、manifest 大小和哈希校验；完整校验后发布到新路径；拒绝覆盖、符号链接和路径别名；普通错误清理本次文件。
- `backup_test.go`、`restore_test.go`：14 个测试函数（含表驱动子用例），覆盖真实仓储的迁移、WAL、一致快照、Token 哈希、Key 解密、账目、运行时冷却/任务固定及待刷盘计费的恢复；错误口令、篡改、截断、异常头、manifest、重复/恶意成员、损坏数据库、超限文件、在线锁、同目标并发、发布失败回滚与输出脱敏。
- `main.go`：注册两个子命令；规格与部署说明按批准方案更新。
- `tools/soak/`：可复跑的外部负载工具与说明，无新依赖。语法检查通过；同一驱动的临时版本已做实际短测。
- 原有 Grafana、Prometheus 文件未改动；架构测试、一致性套件、公开接口、依赖均未修改。

专项 race 命令：`go test -race -count=1 -run '^(TestBackup|TestRestore)' ./cmd/gateway`，结果：

```text
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	38.956s
```

最终完整检查最后 10 行（包括后续归档尾标记及取消清理细化）：

```text
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/protocol	1.032s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.025s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	1.179s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/sse	1.019s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/store	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	2.601s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	2.833s
?   	github.com/ZhangCurosr/universal-ai-gateway/tools/check	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/tools/release	1.024s
ALL CHECKS PASSED
```

## 稳定性基线

- 驱动：本机模拟上游与独立网关，4 个固定 worker、合计目标 2 请求/秒；缓冲透传、SSE、Chat → Anthropic 转译、REST；每 300 秒热重载一次调度策略。
- 无真实 Key 或真实 API 调用。所有凭据为临时合成值，输出不记录凭据；每次请求超时 5 秒、响应上限 1 MiB。
- 60 秒短测：120 次请求、0 次错误、3 次热重载、120 条计费事件一致；RSS 峰值 20.94 MiB，goroutine 峰值 30。
- 24 小时进程开始于 `2026-11-13T16:10:44+08:00`；计划负载 86400 秒，预计约 `2026-11-14T16:10:44+08:00` 后结束（还需关闭和落库核对）。PID `2988305`，网关 PID `2988309`。
- 本报告快照：实际负载约 670 秒，1344 次请求、0 次错误，RSS 22.46 MiB，goroutine 25，计费队列 0.0，审计丢弃 0.0。这只代表当前观测，不是 24 小时结论。
- 路径：`/tmp/uai-t61-results/soak-24h/status.json`、`latest.json`、`samples.csv`。结束时自动生成 `summary.json`，只有该文件 status=passed、实际负载达到 86400 秒且落库数一致，才能验收全天测试。当前没有全天 summary。
- 二进制来自前述固定运行时快照（包含 T55 修复）；本卡后加的离线 backup / restore 不在该二进制中，已由专项恢复演练及最终完整检查另验。
- 前期同时有备份开发与检查，环境不是独占压测机；曲线分析需注明这段活动。负载为稳定性基线，不用于宣称生产吞吐。
- RSS >768 MiB、goroutine >512、数据文件 >1 GiB 或任何异常响应都会终止并标为失败。机器休眠/断电、进程退出都会影响持续时间；后续必须依据数据确认，不补写时间。

## 源文件校验和

```json
{
  "cmd/gateway/main.go": "ae07e58843e0262fa9ac8d355a67726feedde7b859c1e468354c670ef8e391d5",
  "cmd/gateway/backup.go": "1eb596b687153470515f0b060ef7e91c2c02a75ebec18c636c73d54e6fd55dce",
  "cmd/gateway/restore.go": "d72e0061677552467e1eadaa095fc57d73814760faa8a76ca0d684de916c7517",
  "cmd/gateway/backup_test.go": "16a54a6df432bafe81a33d063e72763f28be99ac658cd12f40ab035399bce7bb",
  "cmd/gateway/restore_test.go": "33906d1a8d85be6dc0a2400ea328d90d38ba5c524a208c692eec113943f713d9",
  "internal/scheduler/scheduler.go": "0c1ff31fd66e40e65900b25f10c107d0f5d9848baa235aa510382271271e368a",
  "internal/scheduler/setpools_test.go": "a8458f434b25f9acfc797dc01fefdc2fb547cb53e469c6f5ede7ded5ab2ded4a",
  "tools/soak/soak.py": "f68406c3421d31457baa5e53efcdac1b300f84f388d2b9dc8b08466f43b375c7"
}
```

## 验收标准逐项

| 验收项 | 状态 |
|:--|:--|
| 最新基线的已知竞态定位与既有修复复验 | 满足；未经修复的远端仍不通过，结果有区分 |
| 两组 Linux race 各满 30 次 | 满足，20 个包均 ok，无 DATA RACE / FAIL |
| 备份命令审批、规格、实现、恢复演练与专项负面测试 | 满足 |
| 最终完整检查：格式、vet、构建、零 CGO、race、分层 | 满足 |
| 24 小时内存 / goroutine / 延迟数据与退出后的计费一致性 | **未满足，进行中** |
| Windows 权限与发布实测、断电故障注入 | 未验证；没有声称已验证 |
| 新增 SPEC-GAP / TODO(T61) | 无 |
| 整卡 DONE | 未满足，继续保持 DOING |

多目录发布不提供断电原子性；强制结束可能留下新目录或 `.restore-lock`。不自动清理不明文件；恢复到新位置并人工检查。首版限制及操作命令见 [部署指南](../../DEPLOYMENT.md)。详细授权与文件范围见 [任务卡](../T61_baseline_and_backup.md)。
