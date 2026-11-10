# T46 Linux 竞态检测报告

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
Mem:           5.8Gi       881Mi       4.7Gi       4.2Mi       396Mi       4.9Gi
Swap:          8.0Gi          0B       8.0Gi
```

## 2. 版本

- HEAD：`a9f34beaf7dd3f24123cd628e1dc690464cb7b4c`
- 两个验收修复提交：
  ```
  62ba43d fix(translate): T44 G3/G4 收窄到决策原意
  413e829 fix(metrics): T45 model 标签只取已配置模型名（MTR-05）
  ```

## 3. 结果总览

| 步骤 | 命令 | 结果 | DATA RACE 数 | FAIL 数 | 耗时 |
|:--|:--|:--|:--|:--|:--|
| 5.1 | `go run ./tools/check` | 通过 | 0 | 0 | ~35s |
| 5.2 | `-race -count=30` 新并发代码 | 通过 | 0 | 0 | ~57s |
| 5.3 | `-race -count=30` 核心包 | 通过 | 0 | 0 | ~94s |

## 4. 输出摘录

### 4.1 步骤 5.1（`go run ./tools/check`）输出最后 25 行

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/apperr	1.019s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/archtest	1.066s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	1.096s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.041s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/clientip	1.019s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/config	1.043s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	1.876s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/crypto	1.052s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	1.060s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/ir	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx	1.033s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	1.245s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	1.212s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.070s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/obs	1.021s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/pricing	1.020s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/protocol	1.036s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.024s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	1.111s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/sse	1.019s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/store	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	2.027s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	1.190s
?   	github.com/ZhangCurosr/universal-ai-gateway/tools/check	[no test files]
ALL CHECKS PASSED
```

### 4.2 步骤 5.2（`-race -count=30` 新并发代码）输出完整结果

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.898s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	3.426s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	5.780s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	20.331s
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	25.729s
```

### 4.3 步骤 5.3（`-race -count=30` 核心包）输出完整结果

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	25.550s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	7.463s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	3.844s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.743s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.177s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	2.071s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	6.567s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	29.492s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.924s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	4.391s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	2.123s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	2.133s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.873s
```

### 4.4 步骤 5.4（汇总 Grep 结果原文）

```
$ grep -c "WARNING: DATA RACE" ~/t46-logs/*.log
/home/jack/t46-logs/01_check.log:0
/home/jack/t46-logs/02_race_new.log:0
/home/jack/t46-logs/03_race_core.log:0

$ grep -E "^(FAIL|--- FAIL|panic:)" ~/t46-logs/*.log | sort | uniq -c
（无任何匹配输出，FAIL/panic 计数为 0）

$ grep -E "^ok|^FAIL" ~/t46-logs/02_race_new.log ~/t46-logs/03_race_core.log
/home/jack/t46-logs/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.898s
/home/jack/t46-logs/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	3.426s
/home/jack/t46-logs/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	5.780s
/home/jack/t46-logs/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	20.331s
/home/jack/t46-logs/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	25.729s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	25.550s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	7.463s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	3.844s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.743s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.177s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	2.071s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	6.567s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	29.492s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.924s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	4.391s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	2.123s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	2.133s
/home/jack/t46-logs/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.873s
```

## 5. 问题清单

无。未发现任何数据竞争警告（`WARNING: DATA RACE`）、测试失败（`FAIL`）或 `panic`。

## 6. 未完成或有风险的地方

无。所有包含并发结构（`sync.Map`、`atomic.Pointer`、带锁直方图、指标原子计数、审计观察者、调度器亲和 LRU、CAS 账本等）的新老代码在 30 轮并发压力与 `-race` 竞态检测下均全部通过。
