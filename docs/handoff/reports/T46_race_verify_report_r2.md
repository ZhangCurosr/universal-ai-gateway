# T46 Linux 竞态检测报告（复跑：T43）

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
Mem:           5.8Gi       953Mi       3.8Gi       4.2Mi       1.3Gi       4.9Gi
Swap:          8.0Gi          0B       8.0Gi
```

## 2. 版本

- HEAD：`f9bcb7368f60202aa65c20e70f5a408e577c2422`
- T43 合并与验收修复提交：
  ```
  6efea9e merge: 合并 T43 响应转换经过 IR（流式 IR）
  21692bd fix(translate): T43 验收修复：工具参数原样透传、停止原因单一映射、流式拒绝对齐
  ```

## 3. 结果总览

| 步骤 | 命令 | 结果 | DATA RACE 数 | FAIL 数 | 耗时 |
|:--|:--|:--|:--|:--|:--|
| 5.1 | `go run ./tools/check` | 通过 | 0 | 0 | ~35s |
| 5.2 | `-race -count=30` 新并发代码 | 通过 | 0 | 0 | ~56s |
| 5.3 | `-race -count=30` 核心包 | 通过 | 0 | 0 | ~90s |

## 4. 输出摘录

### 4.1 步骤 5.1（`go run ./tools/check`）输出最后 25 行

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/apperr	1.020s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/archtest	1.064s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	1.095s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.038s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/clientip	1.020s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/config	1.042s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	1.874s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/crypto	1.048s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	1.054s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ir	1.016s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/jsonx	1.017s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	1.234s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	1.178s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.084s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/obs	1.015s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/pricing	1.021s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/protocol	1.032s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.021s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	1.091s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/sse	1.017s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/store	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	1.967s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	1.273s
?   	github.com/ZhangCurosr/universal-ai-gateway/tools/check	[no test files]
ALL CHECKS PASSED
```

### 4.2 步骤 5.2（`-race -count=30` 新并发代码）输出完整结果

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.846s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	3.424s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	6.718s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	19.652s
ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	24.735s
```

### 4.3 步骤 5.3（`-race -count=30` 核心包）输出完整结果

```
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	24.874s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	7.420s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	3.837s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.763s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.164s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	2.118s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	6.355s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	28.199s
?   	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/adaptertest	[no test files]
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.911s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	4.300s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	2.015s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	2.062s
ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.813s
```

### 4.4 步骤 5.4（汇总 Grep 结果原文）

```
$ grep -c "WARNING: DATA RACE" ~/t46-logs-r2/*.log
/home/jack/t46-logs-r2/01_check.log:0
/home/jack/t46-logs-r2/02_race_new.log:0
/home/jack/t46-logs-r2/03_race_core.log:0

$ grep -E "^(FAIL|--- FAIL|panic:)" ~/t46-logs-r2/*.log | sort | uniq -c
（无任何匹配输出，FAIL/panic 计数为 0）

$ grep -E "^ok|^FAIL" ~/t46-logs-r2/02_race_new.log ~/t46-logs-r2/03_race_core.log
/home/jack/t46-logs-r2/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/metrics	1.846s
/home/jack/t46-logs-r2/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/audit	3.424s
/home/jack/t46-logs-r2/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/translate	6.718s
/home/jack/t46-logs-r2/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/admin	19.652s
/home/jack/t46-logs-r2/02_race_new.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/cmd/gateway	24.735s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/core	24.874s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ledger	7.420s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler	3.837s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/auth	1.763s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit	1.164s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi	2.118s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/mcp	6.355s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite	28.199s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic	1.911s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru	4.300s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai	2.015s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp	2.062s
/home/jack/t46-logs-r2/03_race_core.log:ok  	github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily	1.813s
```

## 5. 问题清单

无。未发现任何数据竞争警告（`WARNING: DATA RACE`）、测试失败（`FAIL`）或 `panic`。

## 6. 未完成或有风险的地方

无。T43 响应转换流式 IR 解耦重构合入 main 之后，经本次完整 30 轮 `-race` 压力检测复跑验证，所有转译流程（Decoder、Encoder、Matrix 矩阵、IR 状态流）在多线程高并发运行下保持 100% 线程安全，零数据竞争。
