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
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/files"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
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
	// ConfigPath 是 serve --config 的路径（HR-01）；为空时不支持配置重载。
	ConfigPath string

	inherit *inherited // 进程内重启时由上一个实例交接（serveLoop 设置）
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

	// 2. logger（级别可热重载）
	levelVar := new(slog.LevelVar)
	levelVar.Set(obs.ParseLevel(cfg.Server.LogLevel))
	logger := obs.NewLoggerVar(stderr, levelVar)
	slog.SetDefault(logger)
	var rs *errRestart
	defer func() {
		if err != nil && !errors.As(err, &rs) {
			logger.Error("gateway stopped with error", "err", err)
		}
	}()
	listening := false
	defer func() { // HR-16：开始服务之前的失败标记为启动失败，serveLoop 据此回滚
		if err != nil && !listening && !errors.As(err, &rs) {
			err = &startupError{err: err}
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
	restoreRuntimeState(ctx, db, sched, logger)      // RT-10：开始服务之前恢复冷却与任务固定
	recomputeQuotaUsage(ctx, db, sched, nil, logger) // QT-10：从计费事件重算本周期额度用量

	// 8. 沿用的组件与第一代（注册表、内核、MCP、数据面路由；spec/13 §6、spec/03 §8）
	auditPipe := audit.NewPipeline(cfg.Audit.QueueCapacity)
	limiter := ratelimit.New(time.Now)
	ipLimiter := ratelimit.New(time.Now)
	overload := httpapi.NewOverloadGuard(cfg.Limits.MaxInFlightRequests,
		cfg.Limits.MaxBufferedBodyBytes, cfg.Limits.MaxRequestBodyBytes)
	gd := genDeps{
		mem: mem, sched: sched, auditPipe: auditPipe, index: index, pepper: keyring.TokenPepper(),
		limiter: limiter, ipLimiter: ipLimiter, overload: overload,
		budget: &responseBudget{limit: cfg.Limits.MaxBufferedBodyBytes}, logger: logger,
	}
	if err := checkWebhookFile(cfg); err != nil { // CFG-22：告警推送地址读不到则拒绝启动
		return err
	}
	fileStore, err := openFileStore(cfg, logger) // T69：nil = 没有开启 files
	if err != nil {
		return err
	}
	gd.files = fileStore
	gen, err := buildGeneration(cfg, gd, nil)
	if err != nil {
		return err
	}

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
	startWorker("runtime_snapshot", runtimeSnapshotter(db, sched, logger, runtimeSnapshotInterval)) // RT-11、RT-12
	startWorker("key_quarantine", func(ctx context.Context) error {
		persistQuarantines(ctx, db, quarantines, logger)
		return nil
	})

	// 11. 指标收集器与动态仪表（spec/15）
	metricsCollector := metrics.NewCollector()
	metricsCollector.RestrictModels(configuredModelNames(cfg)) // MTR-05：model 标签只取已配置的模型名
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
	records := &reloadLog{}
	if opts.inherit != nil && opts.inherit.records != nil {
		records = opts.inherit.records
	}
	rl := &configReloader{
		path: opts.ConfigPath, logger: logger, gd: gd, levelVar: levelVar, master: master,
		st: db, keyring: keyring, collector: metricsCollector, records: records,
		restartCh: make(chan *restartRequest, 1),
	}
	rl.install(gen)
	// 池级告警（T64）：按当前一代的配置评估，热重载后下一轮生效
	alerts := newAlertEngine(sched, func() *config.Config { return rl.gen.Load().cfg }, logger)
	metricsGauges.Pools = alerts.PoolGauges
	startWorker("pool_alerts", alerts.Run)
	if fileStore != nil {
		startWorker("files_sweep", fileStore.Run) // FS-04
	}
	dataHandler := trackHandlers(&handlers, rl) // 数据面处理器可被热重载原子替换（HR-13）
	adminMCPHandler := newAdminMCP(logger)
	adminHandler, adminCtl := admin.NewServerWithControl(admin.Deps{
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
		// spec/03 §8：配置重载
		Reload:        rl.Reload,
		ReloadRecords: records.view,
		Tools:         rl.toolsView,
		MCP:           adminMCPHandler, // spec/14 §6
		Alerts:        alerts.View,     // ADM-26
		Files:         fileStore,       // FS-50
	})
	adminMCPHandler.admin = adminHandler
	rl.adminCtl = adminCtl

	// 12. 两个 http.Server（SRV-01 ~ SRV-03：先 Listen 再 Serve）。进程内重启时地址不变的套接字沿用（§2.3）
	dataLn, dataInherited, err := listenerFor(opts.inherit, "data", cfg.Server.DataListen)
	if err != nil {
		return fmt.Errorf("listen data plane: %w", err)
	}
	adminLn, _, err := listenerFor(opts.inherit, "admin", cfg.Server.AdminListen)
	if err != nil {
		if dataInherited {
			opts.inherit.giveBack("data", cfg.Server.DataListen, dataLn) // 留给回滚
		} else {
			_ = dataLn.Close() // 启动失败的清理
		}
		return fmt.Errorf("listen admin plane: %w", err)
	}
	listening = true
	opts.inherit.closeRest() // 上一个实例留下、本实例用不到的套接字（例如地址已变）
	if in := opts.inherit; in != nil && in.pendingID != 0 {
		if in.rollback != nil {
			records.finish(in.pendingID, reloadStatusRolledBack, in.rollback.Error())
			logger.Error("config reload failed after restart; running with the previous configuration")
		} else {
			records.finish(in.pendingID, reloadStatusApplied, "")
			logger.Info("config reload applied after restart")
		}
		in.pendingID = 0
	}
	dataSrv := newHTTPServer(cfg, dataHandler, logger)
	adminSrv := newHTTPServer(cfg, adminHandler, logger)
	dataConns, adminConns := trackConns(dataSrv), trackConns(adminSrv)

	dataDone, adminDone := make(chan error, 1), make(chan error, 1)
	go func() { dataDone <- serve(dataSrv, dataLn) }()
	go func() { adminDone <- serve(adminSrv, adminLn) }()
	logger.Info("gateway listening", "data", dataLn.Addr().String(), "admin", adminLn.Addr().String(), "version", version)
	logger.Info("admin ui", "url", "http://"+adminLn.Addr().String()+"/admin/ui/") // spec/09 §6
	if opts.OnListening != nil {
		opts.OnListening(dataLn.Addr(), adminLn.Addr())
	}

	var runErr error
	var restart *restartRequest
	dataServing, adminServing := true, true
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case runErr = <-dataDone:
		dataServing = false
		logger.Error("http server failed", "err", runErr)
	case runErr = <-adminDone:
		adminServing = false
		logger.Error("http server failed", "err", runErr)
	case restart = <-rl.restartCh: // HR-15
		logger.Info("restarting with the new configuration")
		dataLn.handover.Store(true) // Shutdown 不关闭套接字，留给下一个实例
		adminLn.handover.Store(true)
	}

	// 关闭 1：数据面排空（drainServer）；超过 shutdown_grace 则强制关闭连接，并等待剩余 handler 结算完
	if dataServing {
		stopAccepting(dataLn, dataDone, logger)
	}
	if !drainServer(dataSrv, dataConns, cfg.Server.ShutdownGrace) {
		logger.Warn("data plane shutdown grace exceeded; closing connections")
	}
	if !waitTimeout(&handlers, drainHandlersTimeout) {
		logger.Error("data plane handlers still running after shutdown; some usage events may be written to the unflushed file")
	}

	// 关闭 2：管理面
	if adminServing {
		stopAccepting(adminLn, adminDone, logger)
	}
	drainServer(adminSrv, adminConns, 5*time.Second)

	// 关闭 3：取消 worker 并等待最后一次刷盘
	cancelWorkers()
	workers.Wait()

	// 关闭 4：数据库与目录锁由 defer 关闭（先 db 后 lock）
	logger.Info("gateway stopped")
	if restart != nil && runErr == nil {
		in := &inherited{listeners: map[string]*sharedListener{
			listenerKey("data", cfg.Server.DataListen):   dataLn.shared,
			listenerKey("admin", cfg.Server.AdminListen): adminLn.shared,
		}, records: records, pendingID: restart.recordID}
		for k, ln := range restart.prepared {
			in.listeners[k] = ln
		}
		return &errRestart{next: restart.cfg, inherit: in}
	}
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
	return newUpstreamClientWithBudget(cfg, &responseBudget{limit: cfg.Limits.MaxBufferedBodyBytes})
}

// maxConnectTimeout 是所有池 timeouts.connect 的最大值（spec/13 §6）。
func maxConnectTimeout(cfg *config.Config) time.Duration {
	connect := time.Duration(0)
	for _, p := range cfg.Pools {
		connect = max(connect, p.Timeouts.Connect)
	}
	if connect <= 0 {
		connect = defaultConnectTimeout
	}
	return connect
}

// newUpstreamClientWithBudget 与 newUpstreamClient 相同，但使用给定的响应缓冲预算：热重载换代时共享同一个预算。
func newUpstreamClientWithBudget(cfg *config.Config, budget *responseBudget) *http.Client {
	connect := maxConnectTimeout(cfg)
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
		Transport: &responseBudgetTransport{base: t, budget: budget},
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

// serve 运行服务端；正常 Shutdown 返回的 ErrServerClosed 与关闭监听器后的 net.ErrClosed 视为成功。
func serve(srv *http.Server, ln net.Listener) error {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// connTracker 统计服务端尚未关闭的连接（通过 http.Server.ConnState）。
type connTracker struct {
	mu      sync.Mutex
	open    map[net.Conn]struct{} // guarded by mu
	waiters []chan struct{}       // guarded by mu；open 归零时全部关闭
}

func trackConns(srv *http.Server) *connTracker {
	t := &connTracker{open: map[net.Conn]struct{}{}}
	srv.ConnState = t.hook
	return t
}

func (t *connTracker) hook(c net.Conn, st http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch st {
	case http.StateNew:
		t.open[c] = struct{}{}
	case http.StateClosed, http.StateHijacked:
		delete(t.open, c)
		if len(t.open) == 0 {
			for _, w := range t.waiters {
				close(w)
			}
			t.waiters = nil
		}
	}
}

// waitZero 等待所有连接关闭，最多 d；超时返回 false。
func (t *connTracker) waitZero(d time.Duration) bool {
	t.mu.Lock()
	if len(t.open) == 0 {
		t.mu.Unlock()
		return true
	}
	w := make(chan struct{})
	t.waiters = append(t.waiters, w)
	t.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-w:
		return true
	case <-timer.C:
		return false
	}
}

// drainServer 在监听器已关闭、Serve 已返回之后排空服务端，最多 grace；超时则强制关闭，返回 false。
//
// 不直接用 Shutdown 排空：net/http 的连接在读完请求后若发现服务端正在 Shutdown，会不作响应直接关闭，
// 已被接受但还没读完请求的连接因此会收到 EOF（进程内重启时这正是要交给新实例之前的最后几个连接）。
// 这里改为关闭 keep-alive（空闲连接立即关闭，活跃连接处理完当前请求后关闭），等全部连接关闭后再 Shutdown 收尾。
func drainServer(srv *http.Server, conns *connTracker, grace time.Duration) bool {
	srv.SetKeepAlivesEnabled(false)
	ok := conns.waitZero(grace)
	if !ok {
		_ = srv.Close() // 已超时：强制关闭剩余连接，关闭错误无可处理
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx) // 此时已没有连接与监听器，只做收尾；超时也不影响结果
	return ok
}

// stopAccepting 关闭监听器并等待 Serve 返回（最多 drainHandlersTimeout）。
func stopAccepting(ln net.Listener, done <-chan error, logger *slog.Logger) {
	_ = ln.Close() // 交接时只关闭虚拟监听器；真正的关闭错误在这里无可处理
	t := time.NewTimer(drainHandlersTimeout)
	defer t.Stop()
	select {
	case err := <-done:
		if err != nil {
			logger.Warn("http server stopped with error", "err", err)
		}
	case <-t.C:
		logger.Warn("http server did not stop accepting in time")
	}
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

// openFileStore 按 files 配置打开文件中转的存储（T69）；没有开启时返回 nil。
func openFileStore(cfg *config.Config, logger *slog.Logger) (*files.Store, error) {
	f := cfg.Files
	if !f.Enabled {
		return nil, nil
	}
	dir := f.Dir
	if dir == "" {
		dir = filepath.Join(cfg.Server.DataDir, "files")
	}
	s, err := files.Open(dir, files.Limits{
		MaxFileBytes: f.MaxFileBytes, MaxFilesPerClient: f.MaxFilesPerClient, MaxBytesPerClient: f.MaxBytesPerClient,
		MaxTotalBytes: f.MaxTotalBytes, DefaultTTL: f.DefaultTTL, MaxTTL: f.MaxTTL,
	}, nil, logger)
	if err != nil {
		return nil, fmt.Errorf("open file store: %w", err)
	}
	return s, nil
}
