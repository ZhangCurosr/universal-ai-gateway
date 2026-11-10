# 02 工程规范（Engineering Rules）

本章规定代码**必须长什么样**。代码审查按本章逐条检查。

## 1. 语言与工具链

- Go 版本：`go.mod` 声明 `go 1.26.0`（由 `modernc.org/sqlite` 的最低要求决定，本机工具链为 Go 1.27）。可以使用该版本标准库的全部特性（含 `crypto/hkdf`、`for range int`、`http.ServeMux` 方法路由、泛型）。**不要**手动修改 `go` 指令或运行会升级依赖的命令（`go get -u`）。
- 构建必须在 `CGO_ENABLED=0` 下通过。
- 格式：`gofmt`。导入分组：标准库 / 第三方 / 本仓库，三组之间空一行。
- 一键检查：`go run ./tools/check`。它依次执行 `gofmt -l`、`go vet ./...`、`go build ./...`、`CGO_ENABLED=0 go build ./cmd/gateway`、`go test ./...`（含 `internal/archtest` 分层检查）。**任何一步失败都不算完成。**
- 推送前检查钩子（T50）：仓库提供 `.githooks/pre-push`，内容只运行 `go run ./tools/check -short`。每台开发机执行一次 `git config core.hooksPath .githooks` 启用。禁止用 `git push --no-verify` 绕过。GitHub Actions 恢复后 `.github/workflows/check.yml` 照常生效，钩子保留。

## 2. 目录与命名

| 规则 | 正例 | 反例 |
|:--|:--|:--|
| 包名小写单词，与目录名相同 | `package ledger` | `package ledgerImpl` |
| 文件名小写下划线 | `memory_ledger.go` | `MemoryLedger.go` |
| 测试与被测代码同包（白盒），文件名 `xxx_test.go` | `package ledger` | — |
| 接口以能力命名，不加 `I` 前缀 | `Ledger`、`Adapter` | `ILedger` |
| 构造函数 `New` 或 `NewXxx` | `ledger.NewMemory(...)` | `ledger.Create(...)` |
| 缩写全大写 | `RequestID`、`HTTPStatus`、`URL` | `RequestId`、`HttpStatus` |
| 布尔字段/函数用肯定形式 | `Enabled`、`IsStream` | `NotDisabled` |

## 3. 依赖白名单

`go.mod` 只允许以下直接依赖。新增依赖必须由用户批准。

| 模块 | 用途 |
|:--|:--|
| `modernc.org/sqlite` | 纯 Go SQLite 驱动（仅 `internal/store/sqlite` 可导入） |
| `gopkg.in/yaml.v3` | 配置文件解析（仅 `internal/config` 可导入） |

其余全部使用标准库：`net/http`、`encoding/json`、`log/slog`、`crypto/*`、`database/sql`、`sync/atomic`、`context`、`time`。
**不许**引入 Web 框架（gin、echo、chi）、ORM、日志框架、第三方测试断言库（testify 等）。

## 4. 错误处理

1. 跨包传播的业务错误一律使用 `internal/apperr`：

   ```go
   return apperr.New(apperr.CodeInsufficientQuota, "client quota exhausted")
   return apperr.Wrap(apperr.CodeUpstreamError, "read upstream body", err)
   ```

2. 底层错误（I/O、SQL）用 `fmt.Errorf("动词短语: %w", err)` 包装，消息小写开头、不加句号，说明「在做什么时失败」。
3. 判断错误用 `errors.Is` / `errors.As` / `apperr.CodeOf(err)`，**禁止**比较 `err.Error()` 字符串。
4. 一个错误只处理一次：要么记录日志，要么向上返回，**不要既记录又返回**。最终由 HTTP 层或 worker 顶层记录。
5. `apperr.Error.Message` 会返回给客户端，**禁止**包含凭据、SQL、内部文件路径、堆栈。`Cause` 不会返回给客户端，只进日志。
6. 哨兵错误（同包内用于 `errors.Is` 判断）命名 `ErrXxx`，用 `errors.New` 定义在包级 `var` 块中。

## 5. 并发

1. 每个 goroutine 必须有明确的退出条件（`ctx.Done()` 或 channel 关闭），并由启动者负责等待（`sync.WaitGroup` 或 `Run` 返回）。
2. channel 必须有容量上限；向可能满的 channel 发送时，必须明确选择**阻塞**（附注释说明为什么可以阻塞）或 `select { ... default: }` 丢弃（附计数器）。
3. 共享计数器用 `sync/atomic` 的类型化原子（`atomic.Int64`、`atomic.Int32`、`atomic.Pointer[T]`），不用 `atomic.AddInt64(&x, ...)` 这类旧函数。
4. 互斥锁保护的字段在结构体中紧跟在锁后面，并注释 `// guarded by mu`。持锁期间**禁止**做网络 I/O、磁盘 I/O、调用外部回调。
5. 所有测试必须在 `go test -race` 下通过（`tools/check` 在支持的平台上会启用 `-race`；不支持时跳过 race，但代码仍须无竞态）。
6. 「检查后再修改」必须用 CAS 循环，**禁止** `if x.Load() < max { x.Add(1) }` 这种写法（典型竞态，见 07 §4）。

## 6. 上下文与超时

1. 所有可能阻塞的函数第一个参数是 `ctx context.Context`。
2. 发往上游的 HTTP 请求必须使用 `http.NewRequestWithContext`，其 `ctx` 继承自客户端请求，客户端断开时上游请求随之取消。
3. 数据库操作使用调用方传入的 `ctx`；后台 worker 的最后一次刷盘使用新的 `context.WithTimeout(context.Background(), 5*time.Second)`，因为此时原 `ctx` 已取消。
4. `io.ReadAll` 只能用于已用 `http.MaxBytesReader` 或 `io.LimitReader` 限长的 Reader。

## 7. 日志（`log/slog`）

1. 只用 `log/slog`，通过依赖注入的 `*slog.Logger`，**禁止**使用 `fmt.Println`、`log.Printf`。
2. 日志键名固定为蛇形英文，常用键：`request_id`、`client_id`、`key_id`、`pool`、`model`、`status`、`err`、`latency_ms`、`attempt`。
3. 级别：`Debug` 调试细节；`Info` 生命周期事件（启动、关闭、配置加载）；`Warn` 可自愈的异常（Key 冷却、重试、日志丢弃）；`Error` 需要人处理的问题（刷盘持续失败、Key 被隔离）。
4. 数据面每个请求最多一条 `Info` 级访问日志（由 `httpapi` 中间件输出），其余为 `Debug`。
5. 凭据一律经 `obs.MaskSecret(s)` 后再记录。

## 8. JSON

1. 结构体字段必须写 `json:"snake_case"` 标签；可选字段加 `omitempty`。
2. 透传路径中**禁止**把整个请求体反序列化为结构体再序列化（会丢字段、改数字精度）。只能用 `internal/jsonx` 做顶层字段的局部读写（见 08 §3）。
3. 读取数字时注意：`encoding/json` 默认把数字解成 `float64`。需要整数的地方解到 `int64` 字段或使用 `json.Number`。

## 9. HTTP

1. 路由使用标准库 `http.ServeMux` 的方法模式：`mux.Handle("POST /v1/messages", h)`。
2. 上游 `http.Client` 由 `cmd/gateway` 创建并注入，**禁止**使用 `http.DefaultClient`，**禁止**每个请求新建 `http.Transport`。
3. 上游响应体必须 `defer resp.Body.Close()`；读完或放弃前不要提前 return 而不关闭。
4. 向客户端流式写入时每个 SSE 事件写完后调用 `http.Flusher.Flush()`（封装在 `sse.Writer` 中）。

## 10. 测试

1. 测试只用标准库 `testing`、`net/http/httptest`。断言写法：

   ```go
   if got != want {
       t.Fatalf("Reserve() remaining = %d, want %d", got, want)
   }
   ```

2. 多个用例使用表驱动：`tests := []struct{ name string; ... }{...}`，循环内 `t.Run(tt.name, ...)`。
3. 与时间有关的逻辑注入假时钟，**禁止** `time.Sleep` 等待业务逻辑（等待 goroutine 退出除外，且需设置超时）。
4. 上游一律用 `httptest.NewServer` 模拟，测试**禁止**访问外网。
5. 数据库测试使用 `sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))`。
6. 测试用例覆盖规格中编号行为时，在用例名或注释中写编号，如 `t.Run("LED-03 reserve exceeds quota", ...)`。
7. 一个测试只验证一件事；失败信息必须说明「调用了什么、得到什么、期望什么」。

### 10.1 基准测试（T50）

| 编号 | 规则 |
|:--|:--|
| BEN-01 | 基准测试与被测代码放在同一个包的 `*_test.go` 里，函数名 `BenchmarkXxx`，只用标准库 `testing`，必须调用 `b.ReportAllocs()` |
| BEN-02 | 基准测试不得访问外网、不得依赖真实上游；上游一律用 `httptest.NewServer` 返回事先序列化好的固定内容，不得 sleep |
| BEN-03 | 准备工作（建库、起服务、构造请求体）放在 `b.ResetTimer()` 之前；循环体内不得有日志输出；端到端基准在循环内检查状态码，非预期即 `b.Fatalf` |
| BEN-04 | 基准测试只记录，不作为检查门槛：`tools/check` 不运行它，CI 不比较数值。对比只在同一台机器的前后两次测量之间进行 |
| BEN-05 | 每个基准测试在 `-benchtime=1x` 下必须能跑通（快速冒烟：`go test -run '^$' -bench . -benchtime=1x ./...`） |
| BEN-06 | 耗时较长的负载测试（如并发内存测量）写成普通测试，默认 `t.Skip`，只在环境变量 `UAIG_MEMTEST=1` 时运行；这是 §10 之外唯一允许默认跳过的测试类型，跳过消息必须写明启用方法 |

## 11. 注释与文档

1. 每个导出标识符都要有以名称开头的文档注释：`// Reserve 冻结预估成本……`。中英文均可，同一文件内保持一致。
2. 注释解释「为什么」，不复述代码在「做什么」。
3. 实现规格中某条编号行为的代码，在附近注释编号：`// LED-05: 结算金额大于预留时允许透支`。

## 12. 安全编码清单

- [ ] 比较密钥、Token、HMAC 用 `crypto/subtle.ConstantTimeCompare` 或 `hmac.Equal`。
- [ ] 随机数只用 `crypto/rand`（`rand.Read` / `rand.Text`）。
- [ ] SQL 只用占位符 `?`。
- [ ] 不信任客户端传来的任何头部来做鉴权决策（例如不能用 `X-Forwarded-For` 判断管理员）。
- [ ] 转发上游时**只复制白名单头部**（见 08 §3.3），绝不把客户端的 `Authorization` / `x-api-key` 转发给上游。
- [ ] 上游返回的错误体转发给客户端前长度截断到 8 KiB。
