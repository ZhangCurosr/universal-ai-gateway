# T50 性能基线与发布工程（M3）

- **类型**：规格 + 实现 + 测量
- **前置**：无。代码部分随时可以开工；**正式基线报告**建议在 T48 合入之后测，这样能覆盖 Gemini 转译。T48 没合入就先测，报告里注明 HEAD，T48 合入后补测一轮即可。
- **需要用户审批**：**是**（§3 规格草案，含 4 个待定项）。
- **不改业务行为**：本卡只新增基准测试、发布工具和文档，不改任何数据面、管理面、转译层的运行逻辑。发现性能问题只记录，不在本卡里修（修复另开卡）。
- **必读**：
  - [`spec/02_engineering_rules.md`](../../spec/02_engineering_rules.md) §1、§3、§10（工具链、依赖白名单、测试规则）
  - [`spec/09_http_api.md`](../../spec/09_http_api.md) §5（过载保护与 §5.1 内存估算）
  - [`spec/12_deployment.md`](../../spec/12_deployment.md) §4（资源与调参）
  - [`spec/13_core.md`](../../spec/13_core.md) §6（缓冲上限）
  - [`tools/check/main.go`](../../tools/check/main.go)（工具程序的写法，本卡的 `tools/release` 照它写）
  - [`cmd/gateway/main.go`](../../cmd/gateway/main.go)（`version` 变量与 `-ldflags` 注入）
  - [`cmd/gateway/e2e_test.go`](../../cmd/gateway/e2e_test.go)（`fakeUpstream`、`startGateway`，端到端基准复用它们）
  - [`.github/workflows/check.yml`](../../.github/workflows/check.yml)
  - [`docs/DEPLOYMENT.md`](../DEPLOYMENT.md) §1、§4

## 1. 背景与目标

M1、M2 和 M3 的大部分功能已经完成，但有三件事一直缺着：

1. **没有任何性能数据。** 仓库里没有一个 `Benchmark` 函数。网关自己加了多少延迟、每个请求分配多少内存、256 路并发流式时占多少内存，都不知道。spec/09 §5.1 的内存估算只是推算，从没实测过。
2. **没有发布流程。** 部署指南只写了 `go build -o bin/gateway ./cmd/gateway`。`version` 默认是 `dev`，没有约定版本号怎么取、二进制怎么命名、要不要附校验和。
3. **持续集成实际上不运行。** `.github/workflows/check.yml` 在 T40 已写好，但 GitHub Actions 因账单问题一直不跑。现在的质量保证全靠开发者手动跑 `tools/check`，加上在 Ubuntu 机器上手动补竞态检测（T46）。

本卡的目标：

- 建立一组**可重复**的基准测试和一份基线报告，以后改动核心路径时可以对比。
- 写一个纯 Go 的发布工具，一条命令产出多平台二进制和校验和。
- 在 GitHub Actions 恢复之前，用一个低成本的办法让检查不再依赖人记得跑（方案见待定 3）。

## 2. 关键设计：测量，不设门槛

基准测试**只记录，不当作检查门槛**：

- 本机是内存很小的 Windows 机器，WSL 机器也是共享资源，数值抖动大。拿绝对数值当门槛，只会出现「换台机器就红」的假失败。
- 因此 `tools/check` **不运行**基准测试（`go test` 不带 `-bench` 时本来就不会跑），也不比较数值。
- 基准测试的价值在于对比：改动前后在**同一台机器**上各跑一次，用 `benchstat` 风格的「中位数 + 波动」比较。`benchstat` 是外部工具，不进 `go.mod`；没有它就手工看中位数。
- 基线报告写清机器配置，以后对比时只和同一台机器的数据比。

端到端基准的上游一律用 `httptest` 假上游，返回固定内容，这样测到的是**网关自己的开销**，与网络和真实上游无关（spec/02 §10 第 4 条：测试禁止访问外网）。

## 3. 规格草案（阶段 A：写进 spec 后提交审批）

### 3.1 写进 `spec/02_engineering_rules.md`：新增 §10.1「基准测试」

| 编号 | 规则 |
|:--|:--|
| BEN-01 | 基准测试与被测代码放在同一个包的 `*_test.go` 里，函数名 `BenchmarkXxx`，只用标准库 `testing`。必须调用 `b.ReportAllocs()` |
| BEN-02 | 基准测试不得访问外网、不得依赖真实上游；上游一律用 `httptest.NewServer` 返回固定内容 |
| BEN-03 | 准备工作（建库、起服务、构造请求体）放在 `b.ResetTimer()` 之前；循环体内不得有 `t.Log` 之类的输出 |
| BEN-04 | 基准测试不作为检查门槛：`tools/check` 不运行它，CI 不比较数值 |
| BEN-05 | 每个基准测试在 `-benchtime=1x` 下也必须能跑通（用于快速冒烟：`go test -run '^$' -bench . -benchtime=1x ./...`） |

### 3.2 写进 `spec/12_deployment.md`：新增 §7「版本与发布」

| 编号 | 规则 |
|:--|:--|
| REL-01 | 版本号用语义化版本 `vMAJOR.MINOR.PATCH`，以 git 标签记录。*待定 1：首个版本号* |
| REL-02 | 发布构建一律 `CGO_ENABLED=0`、`-trimpath`、`-ldflags "-s -w -X main.version=<版本>"`。`<版本>` 取 `git describe --tags --always --dirty`；没有任何标签时为 `v0.0.0-<短提交号>` |
| REL-03 | 目标平台。*待定 2：平台清单* |
| REL-04 | 产物文件名 `universal-ai-gateway_<版本>_<GOOS>_<GOARCH>[.exe]`，放在 `dist/`（加入 `.gitignore`）；同目录生成 `SHA256SUMS`，格式与 `sha256sum` 输出一致（`<64 位小写十六进制>  <文件名>`） |
| REL-05 | 发布工具在构建前检查：工作树干净（`git status --porcelain` 为空）、`go run ./tools/check` 通过。任何一项不满足就以非零码退出，不产出任何文件。提供 `-allow-dirty` 仅供本地试验，此时版本号带 `-dirty` |
| REL-06 | 发布工具**不**创建标签、**不**推送、**不**上传。打标签和发布由用户手动完成（AGENTS.md §6） |
| REL-07 | 构建完成后，对与当前机器平台相同的那个产物执行 `<产物> version`，输出必须等于 REL-02 的版本号，否则失败 |

### 3.3 写进 `spec/02_engineering_rules.md` §1 或 `docs/DEPLOYMENT.md`：本地检查钩子

*待定 3：CI 恢复前怎么保证检查有人跑。* 候选：

| 方案 | 做法 | 优点 | 缺点 |
|:--|:--|:--|:--|
| **A（推荐）** | 仓库提交 `.githooks/pre-push`（POSIX sh，Git Bash 可用），内容只有 `go run ./tools/check -short`；文档写明一次性启用：`git config core.hooksPath .githooks` | 零成本，Windows 和 WSL 都能用；推送前自动挡住红的提交 | 每台机器要手动启用一次；可以 `--no-verify` 绕过（AGENTS 本来就禁止） |
| B | 在 Ubuntu/WSL 机器上注册 GitHub 自托管 runner，`check.yml` 加一个 `runs-on: self-hosted` 任务 | 真正的 CI，含 `-race` | 机器要常开；自托管 runner 跑公开仓库的 PR 有安全风险，必须限制为只跑 push |
| C | 不做，维持现状 | 无 | 依赖人记得跑 |

`check.yml` 保持不动，等 Actions 恢复后直接生效。

### 3.4 写进 `spec/12_deployment.md` §4：内存实测值

阶段 B 测完后，把 §4.4 的实测结果补进 spec/12 §4 表格下方，作为「估算 vs 实测」对照。这一步属于阶段 B 的 spec 修改，在交付说明里单独列出，由用户在验收时一并批准。

### 3.5 待定项汇总

| # | 问题 | 建议 |
|:--|:--|:--|
| 1 | 首个版本号 | `v0.3.0`：M1 = 0.1、M2 = 0.2、M3 = 0.3。M3 剩下的 WASM 插件做完再出 `v0.4.0` 或 `v1.0.0` |
| 2 | 平台清单 | `linux/amd64`、`linux/arm64`、`windows/amd64`、`darwin/arm64`。纯 Go + modernc sqlite，四个平台都能零 CGO 交叉编译 |
| 3 | 本地检查钩子 | 方案 A |
| 4 | 基线报告里的资源档位 | 只测默认配置（`max_in_flight_requests: 256`）和 spec/12 §4 表格里最小内存那一档，两档足够 |

## 4. 实现要求（阶段 B：用户批准后）

### 4.1 单元级基准（各包 `*_test.go`）

每个基准都覆盖数据面热路径上的一环。下表是最低要求，可以多加，但不要为了凑数测无关代码。

| 包 | 基准 | 测什么 |
|:--|:--|:--|
| `internal/auth` | `BenchmarkLookup` | `ExtractCredential` + `HashToken` + `Index.Lookup` 一整套（数据面每个请求都走），`b.RunParallel` |
| `internal/ratelimit` | `BenchmarkAllow` | `Limiter.Allow`：单个 key 与 1000 个 key 两种情况，`b.RunParallel` |
| `internal/scheduler` | `BenchmarkAcquire` | `Scheduler.Acquire` + `Lease.Finish`：一个池 8 个 Key，`least_inflight` 与 `weighted_round_robin` 各一个子基准，`b.RunParallel` |
| `internal/ledger` | `BenchmarkReserveSettle` | `Memory.Reserve` + `Memory.Settle`；`Events()` 由后台 goroutine 持续读走（可用包内已有的假写入器配 `Flusher.Run`），不得因为测吞吐而丢事件（AGENTS §3 第 4 条） |
| `internal/sse` | `BenchmarkReader` | `NewReader` 解析 1000 个事件的流，每个事件约 200 字节 |
| `internal/translate` | `BenchmarkChatToAnthropicRequest`、`BenchmarkAnthropicStreamToChat` | 一个约 8 KiB 的典型请求；一条 200 个事件的流（复用 `testdata` 里已有的样例，不新增黄金文件） |
| `internal/adapter/openai` | `BenchmarkMeterStream` | 用量提取在 200 个事件上的开销 |

### 4.2 端到端基准（`cmd/gateway/bench_test.go`，新建）

复用 `e2e_test.go` 的 `fakeUpstream` 与 `startGateway`。这两个辅助函数现在收 `*testing.T`，允许把参数类型改成 `testing.TB` 以便基准复用；**只改参数类型，不改函数行为，不改任何已有测试的断言**。

| 基准 | 场景 | 额外报告的指标（`b.ReportMetric`） |
|:--|:--|:--|
| `BenchmarkE2EBufferedPassthrough` | OpenAI Chat 同协议透传，非流式，请求体约 2 KiB | 无 |
| `BenchmarkE2EStreamPassthrough` | OpenAI Chat 同协议透传，流式，假上游一次性写出 100 个事件 | `ttfb-ns/op`：从发出请求到读到第一个事件 |
| `BenchmarkE2EStreamTranslate` | OpenAI Chat 入站 → Anthropic 上游，流式 100 个事件 | `ttfb-ns/op` |
| `BenchmarkE2EBaselineDirect` | 客户端直接请求假上游（不经过网关），同 `BufferedPassthrough` 的请求 | 无。用来算「网关自身开销 = 透传 − 直连」 |

要求：

- 假上游在基准里**不得 sleep**，固定内容事先序列化好。
- 每个基准的客户端 Token 额度要设够大，或者每次迭代前不重复建客户端，避免基准中途因额度耗尽返回 402 而测到错误路径。循环里检查状态码，不是 200 就 `b.Fatalf`。
- 计费事件照常入账；基准结束后等账本刷盘再关闭，不得丢事件。

### 4.3 并发内存测量（`cmd/gateway/bench_test.go`）

`TestMemoryUnderLoad`，默认 `t.Skip`，只在设了环境变量 `UAIG_MEMTEST=1` 时运行（`tools/check` 不跑）。这是唯一允许默认跳过的新测试，跳过原因写在 `t.Skip` 的消息里。

- 用默认配置起网关，同时打开 256 路流式请求。假上游每 10 ms 写一个事件，共 500 个事件，使连接保持约 5 秒。
- 每 100 ms 采样一次 `runtime.ReadMemStats` 的 `HeapInuse` 和 `Sys`，以及 `runtime.NumGoroutine()`。
- 结束后 `t.Logf` 打印峰值，并检查：全部请求都是 200；结束 2 秒后 goroutine 数回到开始前 + 10 以内（检查泄漏）。
- 再测一档 spec/12 §4 最小内存档的配置（待定 4），同样输出峰值。

### 4.4 发布工具（`tools/release/main.go`，新建）

照 `tools/check` 的写法：单个 `main.go`，只用标准库，用 `os/exec` 调 `git` 和 `go`。

```
go run ./tools/release                      # 按 REL-02 ~ REL-07 构建全部平台
go run ./tools/release -allow-dirty         # 本地试验
go run ./tools/release -platforms linux/amd64,windows/amd64
```

- 校验和用 `crypto/sha256` 计算，不调外部命令。
- 构建前清空 `dist/`。`dist/` 之外的文件一律不写。
- 每个外部命令设超时（`exec.CommandContext`，`tools/check` 整体最长 15 分钟，单个平台构建最长 5 分钟），防止卡死（AGENTS §3 第 6 条）。
- 失败时打印是哪一步、哪个平台失败，并以非零码退出。

### 4.5 本地检查钩子（按待定 3 的决定）

方案 A：新建 `.githooks/pre-push`：

```sh
#!/bin/sh
# 推送前运行质量检查（spec/02 §1）。启用：git config core.hooksPath .githooks
exec go run ./tools/check -short
```

文件要带可执行位（`git update-index --chmod=+x .githooks/pre-push`），换行符 LF。

### 4.6 文档

- `docs/DEPLOYMENT.md` §1：在 `go build` 那行下面加「或者用发布工具构建全部平台」，并说明产物在 `dist/`、如何用 `SHA256SUMS` 校验（Linux `sha256sum -c`，Windows `Get-FileHash`）。
- `docs/DEPLOYMENT.md` §4：引用基线报告里的内存实测值。
- 新建 `docs/handoff/reports/T50_perf_baseline.md`，格式见 §6.3。

## 5. 涉及文件

- 阶段 A：`spec/02_engineering_rules.md`、`spec/12_deployment.md`、`spec/11_tasks.md`
- 阶段 B：
  - 新建：`cmd/gateway/bench_test.go`、`tools/release/main.go`、`.githooks/pre-push`（方案 A 时）、`docs/handoff/reports/T50_perf_baseline.md`
  - 新增基准（只加 `Benchmark` 函数，可以新建 `bench_test.go`）：`internal/auth`、`internal/ratelimit`、`internal/scheduler`、`internal/ledger`、`internal/sse`、`internal/translate`、`internal/adapter/openai`
  - 修改：`cmd/gateway/e2e_test.go`（只把辅助函数参数改成 `testing.TB`）、`.gitignore`（加 `/dist/`）、`docs/DEPLOYMENT.md`、`spec/12_deployment.md`（§3.4 的实测值）
- **不要改**：任何非测试的 `internal/` 代码、`internal/archtest`、`internal/adapter/adaptertest`、`tools/check`、`.github/workflows/check.yml`、`go.mod`

`tools/release` 不在 `internal/archtest` 的分层表里（`tools/check` 也不在），不需要登记。如果分层检查报错，停下报告，不要改 archtest。

## 6. 测试与测量要求

### 6.1 必须通过

- `go run ./tools/check` 全绿（基准测试不会被它运行，但会被编译，编译错误会让检查失败）。
- `go test -run '^$' -bench . -benchtime=1x ./...` 全部通过（BEN-05 冒烟）。
- `tools/release` 至少有一个可测的纯函数（例如校验和行格式、版本号拼接、产物命名），为它写表驱动单元测试，放在 `tools/release/main_test.go`。

### 6.2 正式测量（在 Ubuntu/WSL 机器上做，和 T46 同一台）

```bash
go test -run '^$' -bench . -benchmem -count=10 ./... | tee ~/t50-bench.txt
UAIG_MEMTEST=1 go test -run TestMemoryUnderLoad -v -count=1 ./cmd/gateway | tee ~/t50-mem.txt
go run ./tools/release
ls -l dist/ && cat dist/SHA256SUMS && (cd dist && sha256sum -c SHA256SUMS)
```

Windows 本机只做 §6.1 的冒烟，不出正式数据。

### 6.3 基线报告格式（`docs/handoff/reports/T50_perf_baseline.md`）

1. 环境：`uname -a`、`go version`、`nproc`、`free -h`、HEAD 提交号。
2. 单元级基准：每个基准一行，列出 `ns/op`、`B/op`、`allocs/op` 的 10 次中位数和最大最小值。
3. 端到端：同上，外加 `ttfb-ns/op`；单独算出「网关自身开销 = `BufferedPassthrough` − `BaselineDirect`」。
4. 内存：两档配置各自的 `HeapInuse` / `Sys` 峰值、goroutine 峰值与结束后的数量；与 spec/09 §5.1 的估算并列对比。
5. 发布：`dist/` 的文件列表、大小、`sha256sum -c` 的输出、`version` 自检的输出。
6. 观察到的问题：只记录，不修。每条写「现象、数据、怀疑的原因」，审查方据此决定要不要开新卡。

## 7. 验收标准

- [ ] 阶段 A 的 spec 修改单独提交并获批准后才开始阶段 B。
- [ ] §4.1 表里的每个基准都存在，都调用了 `b.ReportAllocs()`，`-benchtime=1x` 冒烟全部通过。
- [ ] 端到端基准循环内检查状态码；基准结束时没有丢计费事件。
- [ ] `TestMemoryUnderLoad` 默认跳过，设 `UAIG_MEMTEST=1` 后能跑完并输出峰值，泄漏检查通过。
- [ ] `tools/release` 满足 REL-02 ~ REL-07；工作树不干净时拒绝构建，且不产出任何文件；不打标签、不推送。
- [ ] `dist/` 已加入 `.gitignore`，仓库里没有提交任何二进制。
- [ ] 基线报告按 §6.3 写完，数据来自 Ubuntu/WSL 机器。
- [ ] 没有改动 §5「不要改」列出的任何文件；`go.mod` 无变化。
- [ ] `go run ./tools/check` 全绿，交付说明按 README §3.2 写好。

## 8. 建议提交拆分

1. `docs(spec): T50 基准测试规则与版本发布规格（待审批）`：阶段 A，只改 spec。**提交后停下等批准。**
2. `test(bench): T50 新增热路径单元级基准`：§4.1。
3. `test(gateway): T50 新增端到端基准与并发内存测量`：§4.2、§4.3，含 `e2e_test.go` 的参数类型调整。
4. `chore(release): T50 新增发布工具与本地检查钩子`：§4.4、§4.5、`.gitignore`。
5. `docs: T50 性能基线报告与部署文档更新`：§4.6、spec/12 的实测值，`spec/11` 状态改为 `DONE`。
