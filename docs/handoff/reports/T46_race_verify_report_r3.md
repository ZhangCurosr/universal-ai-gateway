# T46 Linux 竞态检测报告（复跑：T49）

## 1. 环境

```
$ uname -a
Linux wsl-dev 6.18.54.1-microsoft-standard-WSL2 #1 SMP PREEMPT_DYNAMIC Tue Oct  6 19:13:07 UTC 2026 x86_64 x86_64 x86_64 GNU/Linux

$ go version
go version go1.27.3 linux/amd64

$ gcc --version | head -1
gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0

$ go env CGO_ENABLED
1

$ nproc && free -h
8
               total        used        free      shared  buff/cache   available
Mem:           5.8Gi       910Mi       4.7Gi       4.2Mi       398Mi       4.9Gi
Swap:          8.0Gi          0B       8.0Gi
```

## 2. 版本

- HEAD：`7083a4fd68858a439beeb0d4151e4fe905dbe310`
- T49 合并与轮次提交：
  ```
  7083a4f docs(handoff): T46 复跑改为轮次表，新增 r3（T49）
  911454d merge: 合并 T49 管理 Web UI
  ```

## 3. 结果总览

| 步骤 | 命令 | 结果 | DATA RACE 数 | FAIL 数 | 耗时 |
|:--|:--|:--|:--|:--|:--|
| 5.1 | go run ./tools/check | 通过 | 0 | 0 | ~35s |
| 5.2 | -race -count=30 新并发代码（metrics, audit, translate, admin, cmd/gateway） | 通过 | 0 | 0 | ~65s |
| 5.3 | -race -count=30 核心包（core, ledger, scheduler, auth, ratelimit, httpapi, mcp, store/sqlite, adapter/*） | 通过 | 0 | 0 | ~88s |

## 4. 输出摘录

### 5.1 `go run ./tools/check`

```
==> gofmt -l cmd internal tools
==> go vet ./...
==> go build ./...
==> go build -o /dev/null ./cmd/gateway
==> go test -count=1 -race ./...
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	1.884s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.103s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	1.184s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	1.114s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	1.120s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.076s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	1.987s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/apperr	1.021s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/archtest	1.070s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	1.095s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.039s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/clientip	1.023s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/config	1.045s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	1.871s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/crypto	1.023s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	1.053s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ir	1.018s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx	1.020s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	1.225s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	1.182s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.055s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/obs	1.015s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/pricing	1.018s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/protocol	1.031s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.023s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	1.090s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/sse	1.016s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/store	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	1.881s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	1.282s
?   	github.com/ZhangCurosr/universal-ai-gateway/tools/check	[no test files]
ALL CHECKS PASSED
```

### 5.2 新并发代码重复 30 次（`-count=30`）

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.791s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	3.434s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	6.030s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	28.891s
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	24.141s
```

### 5.3 其余并发核心包重复 30 次（`-count=30`）

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	25.587s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	7.458s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	3.669s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.739s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.140s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	1.995s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	6.100s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	27.084s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.899s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	4.213s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	1.991s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	2.018s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.785s
```

### 5.4 汇总 grep

```
$ grep -c "WARNING: DATA RACE" ~/t46-logs-r3/*.log
/home/jack/t46-logs-r3/01_check.log:0
/home/jack/t46-logs-r3/02_race_new.log:0
/home/jack/t46-logs-r3/03_race_core.log:0

$ grep -E "^(FAIL|--- FAIL|panic:)" ~/t46-logs-r3/*.log | sort | uniq -c
（无输出，0 处失败）
```

## 5. 问题清单

无。全部测试在 `CGO_ENABLED=1 -race` 环境下以 30 次循环无竞态、无失败、无超时通过。

## 6. 未完成或有风险的地方

无。
