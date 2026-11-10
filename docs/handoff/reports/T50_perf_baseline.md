# T50 性能基线与发布工程报告（M3）

- **日期**：2026-11-11
- **分支**：`task/t50-perf-release`
- **HEAD 提交**：`b71f9b0f205c2b44562c0996832efc3be5099772`
- **执行环境**：Ubuntu 24.04 on WSL2 (x86_64)

---

## 1. 测量环境

- **系统**：`Linux wsl-dev 6.18.54.1-microsoft-standard-WSL2 #1 SMP PREEMPT_DYNAMIC Tue Oct  6 19:13:07 UTC 2026 x86_64 x86_64 x86_64 GNU/Linux`
- **Go 版本**：`go version go1.27.3 linux/amd64`
- **CPU**：AMD Ryzen 7 255 w/ Radeon 780M Graphics（8 逻辑核心，`nproc = 8`）
- **内存（`free -h`）**：
  - 总内存：5.8 GiB
  - 已用：969 MiB
  - 空闲：3.3 GiB
  - 可用：4.8 GiB
  - 交换区：8.0 GiB

---

## 2. 单元级基准（10 次测量）

命令：`go test -run '^$' -bench . -benchmem -count=10 ./...`

| 基准 | 所在包 | 测量说明 | ns/op 中位数 (min ~ max) | B/op 中位数 (min ~ max) | allocs/op 中位数 (min ~ max) |
|:--|:--|:--|:-:|:-:|:-:|
| `BenchmarkLookup` | `internal/auth` | ExtractCredential + HashToken + Index.Lookup | 480.5 (449.8 ~ 525.1) | 736.0 (736.0 ~ 736.0) | 11.0 (11.0 ~ 11.0) |
| `BenchmarkAllow/SingleKey` | `internal/ratelimit` | Limiter.Allow 单个 key 令牌桶 | 161.6 (160.8 ~ 162.6) | 0.0 (0.0 ~ 0.0) | 0.0 (0.0 ~ 0.0) |
| `BenchmarkAllow/1000Keys` | `internal/ratelimit` | Limiter.Allow 轮转 1000 个 key | 31.3 (29.5 ~ 34.0) | 0.0 (0.0 ~ 0.0) | 0.0 (0.0 ~ 0.0) |
| `BenchmarkAcquire/least_inflight` | `internal/scheduler` | 8 个 Key 池，获取与释放租约 | 500.4 (493.3 ~ 533.3) | 216.0 (216.0 ~ 216.0) | 5.0 (5.0 ~ 5.0) |
| `BenchmarkAcquire/weighted_round_robin` | `internal/scheduler` | 8 个 Key 池，加权轮询获取与释放 | 933.2 (920.2 ~ 947.0) | 216.0 (216.0 ~ 216.0) | 5.0 (5.0 ~ 5.0) |
| `BenchmarkReserveSettle` | `internal/ledger` | Memory.Reserve + Settle，后台消费事件 | 300.2 (289.3 ~ 308.4) | 71.0 (71.0 ~ 71.0) | 2.0 (2.0 ~ 2.0) |
| `BenchmarkReader` | `internal/sse` | NewReader 解析 1000 个事件（每事件约 200B） | 340,184.5 (332,013.0 ~ 365,129.0) | 489,587.0 (489,587.0 ~ 489,589.0) | 3002.0 (3002.0 ~ 3002.0) |
| `BenchmarkChatToAnthropicRequest` | `internal/translate` | ~8 KiB OpenAI Chat 请求转译至 Anthropic | 88,118.5 (86,490.0 ~ 90,325.0) | 51,631.5 (51,623.0 ~ 51,645.0) | 37.0 (37.0 ~ 37.0) |
| `BenchmarkAnthropicStreamToChat` | `internal/translate` | 200 个 Anthropic SSE 事件流转译至 Chat | 1,341,005.5 (1,326,649.0 ~ 1,402,644.0) | 423,131.0 (423,061.0 ~ 423,203.0) | 8104.0 (8104.0 ~ 8104.0) |
| `BenchmarkMeterStream` | `internal/adapter/openai` | 200 个 SSE 事件流上提取用量与统计 | 1,393,432.5 (1,369,190.0 ~ 1,427,914.0) | 455,300.0 (455,280.0 ~ 455,336.0) | 6701.0 (6700.0 ~ 6701.0) |

---

## 3. 端到端基准（10 次测量）

| 基准 | 场景说明 | ns/op 中位数 (min ~ max) | B/op 中位数 (min ~ max) | allocs/op 中位数 (min ~ max) | ttfb-ns/op 中位数 (min ~ max) |
|:--|:--|:-:|:-:|:-:|:-:|
| `BenchmarkE2EBaselineDirect` | 客户端直连假上游（不经过网关），约 2 KiB 请求体 | 162,469.5 (156,062.0 ~ 170,494.0) | 12,646.0 (12,587.0 ~ 12,722.0) | 92.0 (92.0 ~ 92.0) | — |
| `BenchmarkE2EBufferedPassthrough` | 网关 OpenAI Chat 同协议非流式透传，约 2 KiB 请求体 | 514,640.5 (503,492.0 ~ 546,284.0) | 53,673.5 (53,035.0 ~ 54,035.0) | 455.5 (453.0 ~ 458.0) | — |
| `BenchmarkE2EStreamPassthrough` | 网关 OpenAI Chat 同协议流式透传（100 个事件） | 4,778,903.0 (4,751,020.0 ~ 4,926,970.0) | 450,197.5 (447,918.0 ~ 453,107.0) | 5356.0 (5351.0 ~ 5359.0) | 570,246.0 (551,804.0 ~ 609,750.0) |
| `BenchmarkE2EStreamTranslate` | 网关 OpenAI Chat 入站 → Anthropic 上游流式转译（100 个事件） | 5,811,786.5 (5,744,866.0 ~ 6,256,822.0) | 498,347.0 (496,456.0 ~ 500,147.0) | 6737.5 (6735.0 ~ 6741.0) | 615,244.0 (597,035.0 ~ 735,874.0) |

### 网关自身开销计算

- **同协议非流式透传网关自身耗时** = `BufferedPassthrough` 中位数 − `BaselineDirect` 中位数
  `514,640.5 ns - 162,469.5 ns = 352,171.0 ns ≈ 0.352 ms`（约 352 微秒）。
- **流式首包延迟（TTFB）**：
  - 同协议透传首包延迟中位数：`0.570 ms`（570 微秒）。
  - 跨协议流式转译首包延迟中位数：`0.615 ms`（615 微秒），转译流状态机仅追加约 45 微秒首包延迟。

---

## 4. 并发内存与协程测量

测试命令：`UAIG_MEMTEST=1 go test -run TestMemoryUnderLoad -v -count=1 ./cmd/gateway`
负载特征：并发长连接流式请求，假上游每路每 10ms 写入一个 SSE chunk，共 500 个 chunk（持续约 5 秒）。

| 档位 | 并发流路数 | 缓冲上限配置 | 理论估算峰值（spec/09 §5.1） | 实测 HeapInuse 峰值 | 实测 Sys 峰值 | 协程峰值 | 结束后协程（泄漏检查） | 测试耗时 |
|:--|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|
| **默认档**（≥ 4 GiB 机器） | 256 路 | 512 MiB | ~1.5 GiB | **84.03 MiB**（88,113,152 B） | **116.56 MiB**（122,218,776 B） | 2322 | 开始 17，结束 17（+0） | 7.53s |
| **最小档**（1 GiB 机器） | 64 路 | 128 MiB | ~424 MiB | **97.48 MiB**（102,211,584 B） | **120.56 MiB**（126,413,080 B） | 594 | 开始 17，结束 17（+0） | 7.49s |

### 估算 vs 实测分析

1. **内存占用远低于理论上限**：spec/09 §5.1 的推算公式（`3 × max_buffered_body_bytes`）是假定所有请求同时积压了最大缓冲体的最极端防御边界。在真实并发流式场景下，网关的流式处理管线是基于 chunk 级边收边发的（SSE reader -> meter/translate -> SSE writer），并不在内存中完整攒包，因此即使在 256 路高并发长流式压力下，实际 Heap 峰值仅稳定在 ~84 MiB ~ 97 MiB 左右，操作系统虚拟内存 `Sys` 峰值稳定在 ~120 MiB 左右。
2. **协程无泄漏**：256 路与 64 路测试结束后，等待 2 秒（让连接和运行时清理完毕），goroutine 数量均精准回落至初始值 17，无任何 goroutine 泄露。

---

## 5. 发布工程与产物验证

使用 `go run ./tools/release` 执行构建：
- 编译参数：`CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X main.version=v0.0.0-b71f9b0"`
- 发布工具自检：工作区检查通过、`tools/check` 全绿、`dist/universal-ai-gateway_v0.0.0-b71f9b0_linux_amd64 version` 输出匹配。

### 产物清单与大小

| 产物文件名 | 目标平台 | 文件大小 | SHA-256 校验和 |
|:--|:--|:-:|:--|
| `universal-ai-gateway_v0.0.0-b71f9b0_darwin_arm64` | macOS ARM64 | 13 MiB | `cedf62d13e835c84772f719dc323e33ff2c60ee10574f4263aeb703e8a4b5347` |
| `universal-ai-gateway_v0.0.0-b71f9b0_linux_amd64` | Linux AMD64 | 13 MiB | `1a5ad153e3a7bc487f3efa551a50365e0fee719e9124c717fe2362cdb5d57621` |
| `universal-ai-gateway_v0.0.0-b71f9b0_linux_arm64` | Linux ARM64 | 13 MiB | `924d2e0a141b7bd7a3fad6ecf1ee68980b2d09f79ff0962bf85c7ee68f05fdf7` |
| `universal-ai-gateway_v0.0.0-b71f9b0_windows_amd64.exe` | Windows AMD64 | 13 MiB | `683f2db0c53f3f6b8b2af67534cb49dd3e9b7a3af9a927c66860ac0d38455f4a` |

### 校验和验证结果

`cd dist && sha256sum -c SHA256SUMS` 输出：
```
universal-ai-gateway_v0.0.0-b71f9b0_darwin_arm64: OK
universal-ai-gateway_v0.0.0-b71f9b0_linux_amd64: OK
universal-ai-gateway_v0.0.0-b71f9b0_linux_arm64: OK
universal-ai-gateway_v0.0.0-b71f9b0_windows_amd64.exe: OK
```

---

## 6. 观察到的问题与分析（仅记录，不改代码）

1. **单个 Key 的默认最大在途并发（`max_in_flight`）限制**：
   - **现象**：在并发 256 路长流式负载测试中，若池中只添加了 1 个默认配置的 Key，第 17 个请求会收到 503 `upstream_unavailable`（no upstream key available）。
   - **数据**：`/admin/api/keys` 创建 Key 时，`max_in_flight` 缺省值为 16。
   - **分析**：符合 spec/07 §2.1 设计。用户如果单 Key 上游支持更大并发，或在高并发流式场景部署，应在管理面为 Key 配置更大的 `max_in_flight` 或添加多个 Key。
2. **鉴权前 IP 限流（`limits.preauth_ip_rpm`）与压测客户端**：
   - **现象**：压测循环单 IP 在 1 秒内发起成千上万次基准请求时，默认会触发 429 `rate_limited`。
   - **数据**：`PreAuthIPRPM` 默认值为 600（每分钟 600 次）。
   - **分析**：符合 spec/09 §3 设计（防止未鉴权 IP 泛洪攻击）。在基准压测场景下，网关需通过配置设置 `preauth_ip_rpm: 0` 关闭未鉴权 IP 限流。
3. **HTTP 客户端空闲长连接的协程占用**：
   - **现象**：长流式结束后，若客户端保留大量 Idle Keep-Alive 连接，客户端及服务端内部的 `readLoop`/`writeLoop` 会保持存活直至空闲超时。
   - **分析**：属于标准库 `net/http` 正常的空闲连接连接池行为。测试中在请求结束后调用 `client.CloseIdleConnections()` 与 `slowUpstream.CloseClientConnections()` 后协程立即完全释放回初始值。
