package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/admin"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/audit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/core"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/metrics"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite"
)

// runOptions 让测试可以注入输出与监听回调。
type runOptions struct {
	Stderr io.Writer
	// OnListening 在两个监听器都就绪后被调用（测试用 127.0.0.1:0 时据此拿到实际地址）。
	OnListening func(data, admin net.Addr)
}

const (
	// sweepInterval 是限流器清理空闲桶的周期（RL-06）。
	sweepInterval = 5 * time.Minute
	// defaultConnectTimeout 在所有池都没有配置 timeouts.connect 时使用（Normalize 正常会填充）。
	defaultConnectTimeout = 10 * time.Second
	// drainHandlersTimeout 是 Shutdown 超时后强制关闭连接、等待剩余 handler 退出（并完成 Settle）的上限。
	drainHandlersTimeout = 10 * time.Second
	// quarantineQueue 是 Key 隔离写库请求的缓冲；隔离极少发生，满了只记 Warn（内存状态已生效）。
	quarantineQueue = 64
)

// run 按 spec/03 §5 的顺序启动全部组件，阻塞直到 ctx 取消，然后按 §5 的顺序关闭。
// 任一步失败返回错误（main 以退出码 1 退出）；正常关闭返回 nil。
func run(ctx context.Context, cfg *config.Config, opts runOptions) (err error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// 2. logger
	logger := obs.NewLogger(stderr, cfg.Server.LogLevel)
	slog.SetDefault(logger)
	defer func() {
		if err != nil {
			logger.Error("gateway stopped with error", "err", err)
		}
	}()

	// 3. 数据目录 + 目录锁
	if err := os.MkdirAll(cfg.Server.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	lock, err := sqlite.AcquireDirLock(ctx, filepath.Join(cfg.Server.DataDir, "gateway.lock"))
	if err != nil {
		return fmt.Errorf("acquire data dir lock: %w", err)
	}
	defer lock.Close() //nolint:errcheck // 进程退出前释放；失败不影响退出

	// 4. 主密钥与 Keyring
	master, err := crypto.LoadOrCreateMasterKey(cfg.Server.MasterKeyFile)
	if err != nil {
		return fmt.Errorf("load master key: %w", err)
	}
	keyring, err := crypto.NewKeyring(master, 1)
	if err != nil {
		return fmt.Errorf("create keyring: %w", err)
	}

	// 5. 数据库（含迁移）
	db, err := sqlite.Open(ctx, filepath.Join(cfg.Server.DataDir, "gateway.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close() //nolint:errcheck // 关闭顺序的最后一步；失败不影响退出

	// FLU-05：在加载账户之前恢复上次未写入的计费事件
	unflushedPath := filepath.Join(cfg.Server.DataDir, "unflushed_usage.jsonl")
	if err := ledger.RecoverUnflushed(ctx, db, unflushedPath); err != nil {
		return fmt.Errorf("recover unflushed usage events: %w", err)
	}

	// 6. 管理员引导（BOOT-01 ~ BOOT-03）
	if err := bootstrapAdmin(ctx, db, keyring, stderr); err != nil {
		return err
	}

	// 7. 加载：客户端 → Index；用量 → ledger；上游 Key → scheduler
	index := auth.NewIndex()
	reloader := &auth.Reloader{Store: db, Index: index}
	if err := reloader.Reload(ctx); err != nil {
		return err
	}
	mem := ledger.NewMemory(ledger.MemoryConfig{
		EventQueueCapacity:     cfg.Ledger.EventQueueCapacity,
		AdmissionHighWatermark: cfg.Ledger.AdmissionHighWatermark,
	})
	if err := loadAccounts(ctx, db, mem); err != nil {
		return err
	}

	quarantines := make(chan quarantineReq, quarantineQueue)
	sched := scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{
		Logger: logger,
		// ST-06：回调在请求路径上被调用，不能做磁盘 I/O——只投递给后台 worker。
		OnQuarantine: func(keyID int64, reason string) {
			select {
			case quarantines <- quarantineReq{id: keyID, reason: reason}:
			default:
				logger.Warn("quarantine queue full; key state not persisted", "key_id", keyID)
			}
		},
	})
	keys, err := scheduler.LoadKeys(ctx, db, keyring, logger)
	if err != nil {
		return fmt.Errorf("load upstream keys: %w", err)
	}
	sched.SetKeys(keys)

	// 8. 上游 http.Client（spec/13 §6）
	upstreamClient := newUpstreamClient(cfg)

	registry, err := newRegistry(cfg, logger)
	if err != nil {
		return fmt.Errorf("register adapters: %w", err)
	}
	auditPipe := audit.NewPipeline(cfg.Audit.QueueCapacity)
	kernel, err := core.New(core.Deps{
		Config:     cfg,
		Registry:   registry,
		Ledger:     mem,
		Scheduler:  sched,
		Audit:      auditPipe,
		HTTPClient: upstreamClient,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("create core: %w", err)
	}

	trusted, err := clientip.ParseTrusted(cfg.Server.TrustedProxies)
	if err != nil {
		return fmt.Errorf("parse trusted_proxies: %w", err)
	}
	limiter := ratelimit.New(time.Now)
	ipLimiter := ratelimit.New(time.Now)

	// 9. 后台 worker。用独立的 ctx：关闭时要等数据面排空后才取消，保证计费事件全部刷盘。
	workerCtx, cancelWorkers := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWorkers()
	var workers sync.WaitGroup
	startWorker := func(name string, fn func(context.Context) error) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := fn(workerCtx); err != nil {
				logger.Error("background worker failed", "worker", name, "err", err)
			}
		}()
	}
	startWorker("ledger_flusher", ledger.NewFlusher(db, mem.Events(), ledger.FlusherConfig{
		Interval:      cfg.Ledger.FlushInterval,
		QueueCapacity: cfg.Ledger.EventQueueCapacity,
		UnflushedPath: unflushedPath,
	}, logger).Run)
	startWorker("audit_writer", audit.NewWriter(auditPipe, db, audit.WriterConfig{
		BatchSize:     cfg.Audit.BatchSize,
		FlushInterval: cfg.Audit.FlushInterval,
	}, logger).Run)
	startWorker("audit_retention", (&audit.Retention{St: db, RetentionDays: cfg.Audit.RetentionDays, Log: logger}).Run)
	startWorker("ratelimit_sweep", func(ctx context.Context) error {
		sweepLimiters(ctx, sweepInterval, limiter, ipLimiter)
		return nil
	})
	startWorker("key_quarantine", func(ctx context.Context) error {
		persistQuarantines(ctx, db, quarantines, logger)
		return nil
	})

	// 10. MCP（spec/14）
	overload := httpapi.NewOverloadGuard(cfg.Limits.MaxInFlightRequests,
		cfg.Limits.MaxBufferedBodyBytes, cfg.Limits.MaxRequestBodyBytes)
	dataDeps := httpapi.DataDeps{
		Config:    cfg,
		Core:      kernel,
		Registry:  registry,
		Index:     index,
		Pepper:    keyring.TokenPepper(),
		Limiter:   limiter,
		IPLimiter: ipLimiter,
		Overload:  overload,
		Trusted:   trusted,
		Logger:    logger,
	}
	if !cfg.MCP.Disabled {
		dataDeps.MCP = mcp.New(mcp.Deps{
			Registry:       registry,
			Core:           kernel,
			MaxResultBytes: cfg.MCP.MaxResultBytes,
			Version:        version,
			Logger:         logger,
		})
		dataDeps.MCPErrorWriter = mcp.WriteHTTPError
	}

	// 11. 指标收集器与动态仪表（spec/15）
	metricsCollector := metrics.NewCollector()
	modelNames := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		modelNames = append(modelNames, m.Name)
	}
	metricsCollector.RestrictModels(modelNames) // MTR-05：model 标签只取已配置的模型名
	auditPipe.SetObserver(metricsCollector.ObserveRequest)

	startedAt := time.Now()
	metricsGauges := metrics.Gauges{
		Keys: func() []metrics.KeyGauge {
			statuses := sched.KeyStatus()
			out := make([]metrics.KeyGauge, 0, len(statuses))
			for _, ks := range statuses {
				out = append(out, metrics.KeyGauge{
					Pool:              ks.Pool,
					ID:                ks.ID,
					Name:              ks.Name,
					Persisted:         ks.Persisted.String(),
					InFlight:          ks.InFlight,
					CooldownRemaining: ks.CooldownRemaining,
					BreakerOpen:       ks.Breaker == scheduler.BreakerOpen,
				})
			}
			return out
		},
		LedgerQueue: func() (length, capacity int) {
			evts := mem.Events()
			return len(evts), cap(evts)
		},
		InFlight: func() (requests, bodyBytes int64) {
			return overload.InFlight()
		},
		AuditDropped: func() int64 {
			return auditPipe.Dropped()
		},
		Version:   version,
		StartedAt: startedAt,
	}

	// 在途 handler 计数：Shutdown 超时后仍要等它们退出（Settle 在 handler 内完成），
	// 否则 Flusher 可能先于最后的 Settle 退出，计费事件丢失（AGENTS §3 #4）。
	var handlers sync.WaitGroup
	dataHandler := trackHandlers(&handlers, httpapi.NewDataRouter(dataDeps))
	adminHandler := admin.NewServer(admin.Deps{
		Config:    cfg,
		Store:     db,
		Keyring:   keyring,
		Index:     index,
		Reloader:  reloader,
		Ledger:    mem,
		Scheduler: sched,
		Logger:    logger,
		Version:   version,
		StartedAt: startedAt,
		Metrics:   metricsCollector.Handler(metricsGauges),
		// spec/09 §6：管理 Web UI 的 Host 检查与概览数据
		HostCheck:    true,
		AdminHosts:   cfg.Server.AdminHosts,
		LedgerQueue:  metricsGauges.LedgerQueue,
		AuditDropped: metricsGauges.AuditDropped,
	})

	// 12. 两个 http.Server（SRV-01 ~ SRV-03：先 Listen 再 Serve）
	dataLn, err := net.Listen("tcp", cfg.Server.DataListen)
	if err != nil {
		return fmt.Errorf("listen data plane: %w", err)
	}
	adminLn, err := net.Listen("tcp", cfg.Server.AdminListen)
	if err != nil {
		_ = dataLn.Close() // 启动失败的清理
		return fmt.Errorf("listen admin plane: %w", err)
	}
	dataSrv := newHTTPServer(cfg, dataHandler, logger)
	adminSrv := newHTTPServer(cfg, adminHandler, logger)

	serveErr := make(chan error, 2)
	go func() { serveErr <- serve(dataSrv, dataLn) }()
	go func() { serveErr <- serve(adminSrv, adminLn) }()
	logger.Info("gateway listening", "data", dataLn.Addr().String(), "admin", adminLn.Addr().String(), "version", version)
	logger.Info("admin ui", "url", "http://"+adminLn.Addr().String()+"/admin/ui/") // spec/09 §6
	if opts.OnListening != nil {
		opts.OnListening(dataLn.Addr(), adminLn.Addr())
	}

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case runErr = <-serveErr:
		logger.Error("http server failed", "err", runErr)
	}

	// 关闭 1：数据面 Shutdown(shutdown_grace)；超时则强制关闭连接，并等待剩余 handler 结算完
	shutCtx, cancelShut := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace)
	if err := dataSrv.Shutdown(shutCtx); err != nil {
		logger.Warn("data plane shutdown grace exceeded; closing connections", "err", err)
		_ = dataSrv.Close() // 已经在报告超时，关闭错误无可处理
	}
	cancelShut()
	if !waitTimeout(&handlers, drainHandlersTimeout) {
		logger.Error("data plane handlers still running after shutdown; some usage events may be written to the unflushed file")
	}

	// 关闭 2：管理面
	adminCtx, cancelAdmin := context.WithTimeout(context.Background(), 5*time.Second)
	if err := adminSrv.Shutdown(adminCtx); err != nil {
		_ = adminSrv.Close() // 同上
	}
	cancelAdmin()

	// 关闭 3：取消 worker 并等待最后一次刷盘
	cancelWorkers()
	workers.Wait()

	// 关闭 4：数据库与目录锁由 defer 关闭（先 db 后 lock）
	logger.Info("gateway stopped")
	return runErr
}

// bootstrapAdmin 实现 BOOT-01 ~ BOOT-03。
func bootstrapAdmin(ctx context.Context, st store.Store, kr *crypto.Keyring, stderr io.Writer) error {
	n, err := st.CountAdmins(ctx)
	if err != nil {
		return fmt.Errorf("count admins: %w", err)
	}
	if n > 0 {
		return nil // BOOT-03
	}
	plain, display, err := auth.GenerateToken()
	if err != nil {
		return fmt.Errorf("generate bootstrap token: %w", err)
	}
	if _, err := st.CreateClient(ctx, &store.Client{
		Name:           "bootstrap-admin",
		Role:           store.RoleAdmin,
		TokenHash:      auth.HashToken(kr.TokenPepper(), plain),
		TokenDisplay:   display,
		QuotaUnlimited: true,
		RPMLimit:       0,
	}); err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	// BOOT-02：全系统唯一允许输出 Token 明文的地方；不经过 slog。
	fmt.Fprintln(stderr, "BOOTSTRAP ADMIN TOKEN (shown once): "+plain)
	return nil
}

// loadAccounts 用客户端额度与已消费成本初始化内存账本。
func loadAccounts(ctx context.Context, st store.Store, mem *ledger.Memory) error {
	clients, err := st.ListClients(ctx)
	if err != nil {
		return fmt.Errorf("list clients: %w", err)
	}
	spent, err := st.SumCostByClient(ctx)
	if err != nil {
		return fmt.Errorf("sum cost by client: %w", err)
	}
	accts := make([]ledger.Account, 0, len(clients))
	for _, c := range clients {
		accts = append(accts, ledger.Account{
			ClientID:     c.ID,
			Unlimited:    c.QuotaUnlimited,
			QuotaNanoUSD: c.QuotaNanoUSD,
			SpentNanoUSD: spent[c.ID],
		})
	}
	mem.LoadAccounts(accts)
	return nil
}

// newUpstreamClient 按 spec/13 §6 创建共享的上游客户端。不设 Client.Timeout（会截断长流）。
func newUpstreamClient(cfg *config.Config) *http.Client {
	connect := time.Duration(0)
	for _, p := range cfg.Pools {
		connect = max(connect, p.Timeouts.Connect)
	}
	if connect <= 0 {
		connect = defaultConnectTimeout
	}
	t := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: &responseBudgetTransport{base: t, budget: &responseBudget{limit: cfg.Limits.MaxBufferedBodyBytes}},
		Timeout:   0,
		// HDR-06：自定义鉴权头不在 net/http 的敏感头清单里；即使同域也不跟随，
		// 避免凭据泄露、HTTPS 降级以及 POST 被自动改写为 GET。
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// newHTTPServer 按 spec/03 §6 创建服务端；WriteTimeout 必须为 0（流式响应可能持续数分钟）。
func newHTTPServer(cfg *config.Config, h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      0,
		IdleTimeout:       cfg.Server.IdleTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// serve 运行服务端；正常 Shutdown 返回的 ErrServerClosed 视为成功。
func serve(srv *http.Server, ln net.Listener) error {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// trackHandlers 统计在途 handler，关闭时据此等待它们完成结算。
func trackHandlers(wg *sync.WaitGroup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Add(1)
		defer wg.Done()
		next.ServeHTTP(w, r)
	})
}

// waitTimeout 等待 wg 归零，最多 d；超时返回 false。
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// sweepLimiters 每 interval 清理一次空闲的限流桶（RL-06），直到 ctx 取消。
func sweepLimiters(ctx context.Context, interval time.Duration, ls ...*ratelimit.Limiter) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, l := range ls {
				l.Sweep(now)
			}
		}
	}
}

type quarantineReq struct {
	id     int64
	reason string
}

// keyStateSetter 是隔离 worker 需要的最小存储能力。
type keyStateSetter interface {
	SetUpstreamKeyState(ctx context.Context, id int64, state, reason string) error
}

// persistQuarantines 把调度器的隔离决定写库（ST-06），直到 ctx 取消。
func persistQuarantines(ctx context.Context, st keyStateSetter, reqs <-chan quarantineReq, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-reqs:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := st.SetUpstreamKeyState(wctx, q.id, store.KeyStateQuarantined, q.reason); err != nil {
				logger.Warn("persist key quarantine failed", "key_id", q.id, "err", err)
			} else {
				logger.Warn("upstream key quarantined", "key_id", q.id, "reason", q.reason)
			}
			cancel()
		}
	}
}
