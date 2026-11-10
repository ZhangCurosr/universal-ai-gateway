# T46 在 Linux 上补做竞态检测（验证卡）

- **类型**：纯验证。**不改任何代码、测试、spec**。
- **执行环境**：一台 Ubuntu 机器（已装 Go ≥ 1.26、gcc、git）。
- **前置**：无。
- **需要用户审批**：否。但发现竞态时**只报告、不修复**（见 §6）。
- **必读**：[`AGENTS.md`](../../AGENTS.md)（尤其 §3、§6）；[`tools/check/main.go`](../../tools/check/main.go)（了解检查程序何时启用 `-race`）。

## 1. 为什么要做

Go 的竞态检测器（`-race`）需要 cgo 和 C 编译器。主力开发机是 Windows，没有 C 编译器，`go run ./tools/check` 每次都会打印
`note: race detector skipped`。GitHub Actions 的 Linux 竞态任务（`.github/workflows/check.yml`）因为账户原因目前跑不起来。

T40 曾在 WSL 上跑过一次 `-race`，但那是在 T42（转译层字段容忍）、T45（Prometheus 指标）以及验收修复合入**之前**。
这些新代码里有并发结构（`sync.Map`、`atomic.Pointer`、带锁的直方图、审计流水线的同步观察者），**从来没有在 `-race` 下跑过**。本卡就是补上这一步。

## 2. 规则（违反任何一条，结果作废）

1. **不修改仓库里的任何已跟踪文件**。唯一允许新增的文件是 §7 的报告。
2. **不 push 到 `main`**，不改写历史（`rebase`、`commit --amend`、`reset --hard`、`push --force` 都不行）。
3. 不安装 Go 模块依赖之外的东西进仓库；不运行 `go get`、`go mod tidy`（会改 `go.mod` / `go.sum`）。
4. 发现竞态或失败**不要动手修**，按 §6 收集信息写进报告。修复由审查方决定。
5. 输出里如果出现任何形如 API Key、Token 的字符串（正常情况下不会出现，测试只用假值），贴进报告前替换为 `***`。
6. 每一步都把**实际命令和输出的最后部分**原样记下来，不要转述成「通过了」。

## 3. 环境确认

逐条执行，并把输出贴进报告 §1：

```bash
uname -a
go version            # 必须 >= go1.26
gcc --version | head -1
go env CGO_ENABLED    # 必须是 1；如果是 0，后面所有命令前加 CGO_ENABLED=1
nproc && free -h
```

如果 `go version` 低于 1.26：停下，告诉用户，不要继续。

## 4. 获取代码并确认版本

```bash
# 第一次：
git clone https://github.com/ZhangCurosr/universal-ai-gateway.git
cd universal-ai-gateway
# 已经克隆过：
cd universal-ai-gateway && git fetch origin && git checkout main && git pull --ff-only origin main

git status -sb        # 必须干净
git log --oneline -8
```

**版本要求**：`git log` 里必须能看到下面两个提交（验收时的修复）。看不到，说明用户还没推送，停下告诉用户：

```bash
git log --oneline | grep -E "T45 .*model 标签|T44 .*G3/G4"
```

预期能看到两行，标题分别包含：
- `fix(metrics): T45 model 标签只取已配置模型名（MTR-05）`
- `fix(translate): T44 G3/G4 收窄到决策原意`

把 `git rev-parse HEAD` 的结果写进报告，后面所有结论都只对这个提交有效。

## 5. 执行步骤

所有命令都在仓库根目录执行。耗时较长的步骤建议用 `tee` 把完整输出存到仓库**外**的目录，例如 `~/t46-logs/`：

```bash
mkdir -p ~/t46-logs
```

### 5.1 完整检查（自动启用 -race）

```bash
go run ./tools/check 2>&1 | tee ~/t46-logs/01_check.log
```

通过标准（两条都要满足）：
- 最后一行是 `ALL CHECKS PASSED`；
- 输出里**没有** `race detector skipped`，并且 `go test` 步骤的命令行里带 `-race`（检查程序会打印 `==> go test -count=1 -race ./...`）。

如果出现 `race detector skipped`：说明 cgo 不可用，回到 §3 检查 `CGO_ENABLED` 和 gcc，不要继续。

### 5.2 新并发代码重复 30 次

这些包包含 T42、T45 以及验收修复新增的并发代码，是本卡的重点：

```bash
CGO_ENABLED=1 go test -race -count=30 -timeout 60m \
  ./internal/metrics/... ./internal/audit/... ./internal/translate/... \
  ./internal/admin/... ./cmd/gateway/... \
  2>&1 | tee ~/t46-logs/02_race_new.log
```

### 5.3 其余并发核心包重复 30 次

```bash
CGO_ENABLED=1 go test -race -count=30 -timeout 60m \
  ./internal/core/... ./internal/ledger/... ./internal/scheduler/... \
  ./internal/auth/... ./internal/ratelimit/... ./internal/httpapi/... \
  ./internal/mcp/... ./internal/store/sqlite/... ./internal/adapter/... \
  2>&1 | tee ~/t46-logs/03_race_core.log
```

### 5.4 汇总

```bash
grep -c "WARNING: DATA RACE" ~/t46-logs/*.log
grep -E "^(FAIL|--- FAIL|panic:)" ~/t46-logs/*.log | sort | uniq -c
grep -E "^ok|^FAIL" ~/t46-logs/02_race_new.log ~/t46-logs/03_race_core.log
```

5.2、5.3 的通过标准：每个包都是 `ok`，`WARNING: DATA RACE` 计数为 0，没有 `FAIL` 和 `panic`。

## 6. 出现问题时怎么做（只收集，不修复）

### 6.1 出现 `WARNING: DATA RACE`

对**每一处**竞态：

1. 从日志里完整复制这一段：从 `WARNING: DATA RACE` 到下一个 `==================` 为止（包括 `Write at`、`Previous read at`、两段 goroutine 的创建栈）。
2. 记下它出现在哪个测试里（日志中紧邻的 `--- FAIL: TestXxx` 或 `testing.go:... race detected during execution of test`）。
3. 单独复现，确认是否稳定：
   ```bash
   CGO_ENABLED=1 go test -race -count=100 -run '^TestXxx$' ./internal/<包>/ 2>&1 | tail -40
   ```
4. 判断竞态在哪一侧，写进报告：
   - **生产代码**：栈里两侧都有非 `_test.go` 的帧（例如 `internal/metrics/collector.go`）。这是必须修的缺陷。
   - **只在测试代码**：竞态的一侧或两侧只在 `_test.go`（例如测试里共享的 `bytes.Buffer`）。这是测试缺陷。
5. 不要猜修法；如果你有看法，写在报告「建议」一栏，标明是推测。

### 6.2 测试失败但没有竞态

1. 记下测试名、失败输出（`--- FAIL` 往下 30 行）。
2. 单独重跑 `-count=100 -run '^TestXxx$'`，统计失败次数，例如「100 次失败 3 次」。
3. 不加 `-race` 再跑同样次数，看是否也会失败：
   ```bash
   go test -count=100 -run '^TestXxx$' ./internal/<包>/ 2>&1 | tail -20
   ```
4. 如果只在 `-race` 下失败，通常是超时太紧（`-race` 会让程序慢 2–10 倍），在报告里注明。

### 6.3 超时（`panic: test timed out`）

记下哪个包、卡在哪个测试（日志里 `goroutine ... [running]` 下的第一个 `Test` 帧），然后对那个包单独用 `-count=5 -timeout 20m` 重跑一次，看能否复现。

## 7. 交付

在仓库里新建**唯一**一个文件 `docs/handoff/reports/T46_race_verify_report.md`，格式如下：

```markdown
# T46 Linux 竞态检测报告

## 1. 环境
（§3 五条命令的输出原文）

## 2. 版本
- HEAD：<git rev-parse HEAD>
- 两个验收修复提交：<git log 那两行>

## 3. 结果总览
| 步骤 | 命令 | 结果 | DATA RACE 数 | FAIL 数 | 耗时 |
|:--|:--|:--|:--|:--|:--|
| 5.1 | go run ./tools/check | 通过/失败 | | | |
| 5.2 | -race -count=30 新并发代码 | | | | |
| 5.3 | -race -count=30 核心包 | | | | |

## 4. 输出摘录
（每个步骤输出的最后 25 行，原文；5.4 三条 grep 的输出原文）

## 5. 问题清单
（没有就写「无」。有的话每个问题一节：类型（生产代码竞态 / 测试竞态 / 偶发失败 / 超时）、
测试名、包、复现率、完整竞态报告或失败输出、你的判断（标明是否推测））

## 6. 未完成或有风险的地方
```

提交与交付：

```bash
git checkout -b task/t46-race-verify
git add docs/handoff/reports/T46_race_verify_report.md
git commit -m "docs(handoff): T46 Linux 竞态检测报告

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git push origin task/t46-race-verify
```

如果这台机器没有推送权限，就不要推送，把报告文件和 `~/t46-logs/` 下的日志直接交给用户。

`git diff main --stat` 必须只有这一个报告文件。出现其他文件改动，说明违反了 §2 第 1 条，先 `git status` 查清楚是什么、怎么来的，在报告里说明，不要自己丢弃。

## 8. 验收标准（审查方会逐条核对）

- [ ] 报告里有 §3 的环境输出，`CGO_ENABLED=1`，Go ≥ 1.26。
- [ ] HEAD 包含两个验收修复提交。
- [ ] 5.1 的输出里能看到 `-race` 实际启用，并以 `ALL CHECKS PASSED` 结束。
- [ ] 5.2、5.3 都跑满 30 次，每个包的 `ok` / `FAIL` 都列出。
- [ ] 每一处竞态或失败都有完整原文和复现率；没有擅自修改代码。
- [ ] 分支里只新增了报告文件。

## 9. 复跑

代码有并发相关的改动合入 main 后，按本节再跑一遍。每一轮的编号、要确认的合并提交、日志目录、报告文件和分支见下表。

| 轮次 | 原因 | 合并提交 | 版本检查（§4 改为这条） | 日志目录 | 报告 | 分支 | 状态 |
|:--|:--|:--|:--|:--|:--|:--|:--|
| r2 | T43 响应转换重构 | `6efea9e` | `git log --oneline \| grep -E "合并 T43 响应转换经过 IR"` | `~/t46-logs-r2/` | `T46_race_verify_report_r2.md` | `task/t46-race-verify-r2` | 已通过（`8566a4b` 合入） |
| **r3** | **T49 管理 Web UI**（管理面新增路由、Host 检查、health 取数函数） | `911454d` | `git log --oneline \| grep -E "合并 T49 管理 Web UI"` | `~/t46-logs-r3/` | `T46_race_verify_report_r3.md` | `task/t46-race-verify-r3` | **待执行** |

每一轮的步骤：

1. 按 §3、§4 准备环境、拉取最新 main。§4 的版本检查换成上表那一行的命令，能看到对应的 `merge: ...` 提交才继续。
2. 按 §5 全部重跑，日志放到上表的新目录（把 §5 命令里的 `~/t46-logs/` 都换成它）。r3 的改动在 `internal/admin` 和 `cmd/gateway`，§5.2 已经包含这两个包，不需要增加命令。
3. 报告写到 `docs/handoff/reports/` 下上表的文件，格式同 §7，标题写「T46 Linux 竞态检测报告（复跑：<原因中的任务号>）」。
4. 用上表的分支，其余提交与推送规则同 §7、§2。
5. 验收标准同 §8，另加一条：HEAD 包含上表的合并提交。
