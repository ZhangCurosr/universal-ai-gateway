# T46 Linux 竞态检测报告（复跑：T47+T48）

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
Mem:           5.8Gi       923Mi       4.4Gi       4.2Mi       653Mi       4.9Gi
Swap:          8.0Gi          0B       8.0Gi
```

## 2. 版本

- HEAD：`22eff24f305ffa8346202b61b13ea01779413cb6`
- T48 合并提交：
  ```
  c51ff83 merge: 合并 T48 Gemini 与其他协议互相转译
  ```

## 3. 结果总览

| 步骤 | 命令 | 结果 | DATA RACE 数 | FAIL 数 | 耗时 |
|:--|:--|:--|:--|:--|:--|
| 5.1 | go run ./tools/check | 通过 | 0 | 0 | ~35s |
| 5.2 | -race -count=30 新并发代码（metrics, audit, translate, admin, cmd/gateway） | 通过 | 0 | 0 | ~78s |
| 5.3 | -race -count=30 核心包（core, ledger, scheduler, auth, ratelimit, httpapi, mcp, store/sqlite, adapter/*） | 通过 | 0 | 0 | ~90s |

## 4. 输出摘录

### 5.1 `go run ./tools/check`

```
==> gofmt -l cmd internal tools
==> go vet ./...
==> go build ./...
==> go build -o /dev/null ./cmd/gateway
==> go test -count=1 -race ./...
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	2.334s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.098s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/gemini	1.112s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	1.172s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	1.109s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	1.098s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.084s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	2.010s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/apperr	1.018s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/archtest	1.081s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	1.103s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.042s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/clientip	1.018s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/config	1.054s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	1.876s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/crypto	1.020s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	1.059s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ir	1.017s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx	1.017s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	1.232s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	1.173s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.049s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/obs	1.016s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/pricing	1.018s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/protocol	1.030s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.021s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	1.091s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/sse	1.017s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/store	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	1.919s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	1.332s
?   	github.com/ZhangCurosr/universal-ai-gateway/tools/check	[no test files]
ALL CHECKS PASSED
```

### 5.2 新并发代码重复 30 次（`-count=30`）

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.833s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	3.398s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	8.951s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	30.423s
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	35.811s
```

### 5.3 其余并发核心包重复 30 次（`-count=30`）

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	25.702s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	7.422s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	3.585s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.737s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.147s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	1.988s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	6.078s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	27.950s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.865s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/gemini	2.095s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	4.221s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	2.008s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	2.058s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.772s
```

### 5.4 汇总 grep

```
$ grep -c "WARNING: DATA RACE" ~/t46-logs-r4/*.log
/home/jack/t46-logs-r4/01_check.log:0
/home/jack/t46-logs-r4/02_race_new.log:0
/home/jack/t46-logs-r4/03_race_core.log:0

$ grep -E "^(FAIL|--- FAIL|panic:)" ~/t46-logs-r4/*.log | sort | uniq -c
（无输出，0 处失败）
```

## 5. 问题清单

无。全部测试在 `CGO_ENABLED=1 -race` 环境下以 30 次循环无竞态、无失败、无超时通过。`internal/adapter/gemini` 与 `internal/translate` 的表驱动转译注册在并发高压下表现稳定。

## 6. 未完成或有风险的地方

无。
