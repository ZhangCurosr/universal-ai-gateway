package main

import (
	"context"
	"io"
	"net"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// runOptions 让测试可以注入输出与监听回调。
type runOptions struct {
	Stderr io.Writer
	// OnListening 在两个监听器都就绪后被调用（测试用 127.0.0.1:0 时据此拿到实际地址）。
	OnListening func(data, admin net.Addr)
}

// run 按 spec/03 §5 的顺序启动全部组件，阻塞直到 ctx 取消，然后按 §5 的顺序关闭。
func run(ctx context.Context, cfg *config.Config, opts runOptions) error {
	// TODO(T14): 启动顺序（spec/03 §5）：
	//   1. obs.NewLogger(opts.Stderr, cfg.Server.LogLevel)
	//   2. os.MkdirAll(data_dir, 0o700) + sqlite.AcquireDirLock(data_dir/gateway.lock)
	//   3. crypto.LoadOrCreateMasterKey + crypto.NewKeyring(master, 1)
	//   4. sqlite.Open(data_dir/gateway.db)
	//   5. ledger.RecoverUnflushed(data_dir/unflushed_usage.jsonl)
	//   6. 管理员引导 BOOT-01 ~ BOOT-03（Token 明文只用 fmt.Fprintln(opts.Stderr, ...) 输出一次）
	//   7. 加载：auth.Reloader.Reload；ledger.LoadAccounts（SumCostByClient）；scheduler.LoadKeys + SetKeys
	//   8. 上游 http.Client（spec/13 §6）
	//   9. 后台 worker：ledger.Flusher、audit.Writer、audit.Retention、ratelimit.Sweep（每 5 分钟）
	//  10. MCP：cfg.MCP.Disabled 为 false 时 mcp.New(...)，作为 DataDeps.MCP，DataDeps.MCPErrorWriter = mcp.WriteHTTPError（spec/14）
	//  11. 数据面 + 管理面 http.Server；调用 opts.OnListening
	// 关闭顺序：数据面 Shutdown(shutdown_grace) → 管理面 Shutdown → 取消 worker 并等待 → 关闭 DB → 释放目录锁。
	_ = ctx
	_ = cfg
	_ = opts
	return apperr.NotImplemented("T14")
}
