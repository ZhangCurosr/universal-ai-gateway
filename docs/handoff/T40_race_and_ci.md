# T40 竞态检测与持续集成

- **类型**：验证 + 修复
- **前置**：无
- **需要用户审批**：否（不改 spec）。但如果修复竞态必须改冻结的导出 API，停下来报告。
- **必读**：
  - [`AGENTS.md`](../../AGENTS.md)
  - [`tools/check/main.go`](../../tools/check/main.go)：检查脚本，能检测到 C 编译器时会自动加 `-race`
  - [`spec/02_engineering_rules.md`](../../spec/02_engineering_rules.md)
  - [`spec/06_billing_ledger.md`](../../spec/06_billing_ledger.md) §3–§4（账本的并发约定：LED-05 CAS、LED-08 只结束一次）
  - [`spec/07_scheduler.md`](../../spec/07_scheduler.md)（调度器的锁与租约）

## 1. 背景

项目至今所有的检查都在一台没有 C 编译器的 Windows 机器上运行，`go test -race` 一次也没有跑过（检查脚本会打印 `note: race detector skipped`）。
而以下这些都是并发代码，目前只有普通测试覆盖：

| 包 | 并发点 |
|:--|:--|
| `internal/ledger` | `Memory` 的 CAS 预留（LED-05）、`Reservation` 只能结束一次（LED-08）、事件 channel、刷盘协程 `Flusher` |
| `internal/scheduler` | Key 选择、租约 `Lease.Finish`、熔断与冷却状态、亲和缓存、`OnQuarantine` 回调 |
| `internal/audit` | 非阻塞 `Submit`、`Writer` 批量写入、关闭时的排空 |
| `internal/auth` | `Index.Replace` 与并发 `Lookup`、`Reloader` 的互斥 |
| `internal/admin` | `serial()` 串行化写操作，与数据面并发读 |
| `internal/httpapi` | `OverloadGuard`（OVL-01 ~ OVL-03）、限流器 |
| `internal/ratelimit` | 令牌桶、`sweepLimiters` 定时清理 |
| `internal/core` | `exchange` 中的 `atomic.Bool`（`timedOut`、`idle.fired`）、`time.AfterFunc` 回调里调用 `cancel()` |
| `cmd/gateway` | `run()` 的启动与优雅关闭：`trackHandlers` WaitGroup、`workerCtx`、`persistQuarantines` 通道 |

## 2. 要做的事

### 2.1 在 Linux 上跑竞态检测

在任意装有 gcc 的 Linux（或 WSL2、Docker 的 `golang:1.26` 镜像）中：

```bash
go version   # 需要 go.mod 中的版本：go 1.26
CGO_ENABLED=1 go test -race -count=1 -timeout 30m ./...
```

然后对并发集中的包做**重复压测**，偶发竞态通常要跑多次才能出现：

```bash
CGO_ENABLED=1 go test -race -count=30 -timeout 60m \
  ./internal/ledger/... ./internal/scheduler/... ./internal/audit/... \
  ./internal/auth/... ./internal/ratelimit/... ./internal/httpapi/... \
  ./internal/admin/... ./internal/core/... ./cmd/gateway/...
```

最后用检查脚本确认它在 Linux 上能自动启用 `-race`：

```bash
go run ./tools/check     # 输出中不应再出现 "race detector skipped"
```

### 2.2 修复发现的问题

对每一个 `WARNING: DATA RACE`：

1. 记录：两个冲突访问各自的栈顶 3–5 帧、所在测试名、复现频率（例如「30 次里出现 4 次」）。
2. 判断是**产品代码**的竞态还是**测试代码**的竞态（例如测试里在 goroutine 外读写了没有加锁的变量）。
3. 修复根因：
   - 产品代码：用已有的锁、`atomic` 或把数据所有权限定在单个 goroutine 内来修。**不要**用「多加一个 `time.Sleep`」来规避。
   - 测试代码：可以修测试的同步方式（例如改用 channel 或 `sync.WaitGroup`），但**不能改测试的期望值**，也不能删除或跳过测试。
4. 修复后用 `-count=30` 重跑该包，确认不再出现。

**停下报告的情况**：修复需要改 `internal/core`、`internal/protocol` 的导出 API；或者你认为某个 spec 规定的并发语义本身有问题（例如 LED-07 的阻塞发送导致死锁）。

### 2.3 新增持续集成

新增 `.github/workflows/check.yml`，在 push 和 pull request 时运行：

| Job | 运行环境 | 命令 | 说明 |
|:--|:--|:--|:--|
| `linux-race` | `ubuntu-latest` | `go run ./tools/check` | Ubuntu 自带 gcc，检查脚本会自动加 `-race` |
| `windows` | `windows-latest` | `go run ./tools/check -short` | 保证 Windows 构建与测试也是绿的（主要开发机是 Windows） |

要求：

- 用 `actions/checkout` 与 `actions/setup-go`，Go 版本从 `go.mod` 读取（`go-version-file: go.mod`）。动作版本号写到具体的大版本（如 `@v5`），不要用 `@main`。
- 不需要、也**不许**使用任何仓库密钥（secrets）：全部测试都用本地假上游，不访问外网。
- 打开 Go 模块缓存（`setup-go` 的 `cache: true`）。
- 单个 job 设置 `timeout-minutes: 40`。

> 这不是 `go.mod` 依赖，不违反依赖白名单；但它是新增文件，属于本卡「涉及文件」。

### 2.4 写竞态报告

新增 `docs/handoff/reports/T40_race_report.md`，内容：

1. 运行环境（OS、Go 版本、CPU 核数）与完整命令。
2. 每个发现的竞态：编号、位置、原因、修复方式、对应提交。
3. 没有发现竞态的包也列出来，写「`-count=30` 无告警」。
4. 第一次 CI 运行的链接或截图描述（绿/红，耗时）。

## 3. 涉及文件

- 新增：`.github/workflows/check.yml`、`docs/handoff/reports/T40_race_report.md`
- 按发现修改：上面 §1 表中各包的源文件与测试文件
- **不要改**：`tools/check/main.go` 的检查步骤（除非它在 Linux 上无法自动启用 `-race`，那就先报告）

## 4. 验收标准

- [ ] Linux 上 `go test -race -count=1 ./...` 全部通过，无 `DATA RACE` 告警。
- [ ] §2.1 的 `-count=30` 压测全部通过（日志中无 `DATA RACE`，无偶发失败）。如果有偶发失败但不是竞态（例如超时），在报告中单列，并说明是否已修复。
- [ ] `go run ./tools/check` 在 Linux 上自动启用 `-race` 并全绿；在 Windows 上 `-short` 全绿。
- [ ] CI 工作流在 GitHub 上至少成功运行一次，两个 job 都是绿的。
- [ ] 报告文件按 §2.4 写完。
- [ ] 没有修改任何测试的期望值，没有删除或跳过任何测试。

## 5. 建议提交拆分

1. `fix(<包>): T40 修复 <简述> 的数据竞争`：每个独立的竞态一个提交。
2. `chore(ci): T40 新增 Linux 竞态检测与 Windows 检查工作流`
3. `docs(handoff): T40 竞态检测报告`

## 6. 常见坑

- `modernc.org/sqlite` 在 `-race` 下明显变慢，`cmd/gateway` 的端到端测试可能接近超时。`-timeout` 要放宽，但**不要**改测试里的业务超时（例如 `10*time.Second` 的关闭等待）。如果在 CI 上确实超时，先报告。
- `t.Parallel()` 的测试共享了包级变量（例如测试里替换了 `now` 函数）是常见的测试竞态来源。
- `time.AfterFunc` 的回调运行在独立 goroutine 中：回调里写、主流程里读的变量必须是原子变量或加锁。
