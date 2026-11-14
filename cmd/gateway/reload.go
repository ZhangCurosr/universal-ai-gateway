package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/admin"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/audit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/core"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/files"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/metrics"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite"
)

// 配置重载（spec/03 §8，任务卡 T55）。

const (
	reloadModeNone    = "none"
	reloadModeHot     = "hot"
	reloadModeRestart = "restart"

	reloadStatusApplied    = "applied"
	reloadStatusRestarting = "restarting"
	reloadStatusFailed     = "failed"
	reloadStatusRolledBack = "rolled_back"
	reloadStatusNoop       = "noop"

	reloadRecordCap = 20
	// dirLockProbeTimeout 是预检新 data_dir 的目录锁时最多等待的时间。
	dirLockProbeTimeout = 2 * time.Second
)

// ---------------------------------------------------------------- 配置比较（HR-12）

// changeSet 是一类具名对象（池或服务）的增删改。
type changeSet struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

func (c changeSet) empty() bool { return len(c.Added)+len(c.Removed)+len(c.Changed) == 0 }

// reloadChanges 是新旧配置的差异（ADM-18）。只含名称，不含配置内容。
type reloadChanges struct {
	Pools         changeSet `json:"pools"`
	Services      changeSet `json:"services"`
	Settings      []string  `json:"settings"`       // 其他热重载字段
	RestartFields []string  `json:"restart_fields"` // 需要进程内重启的字段
}

// 热重载的顶层段与 server 子字段（HR-12）；其他字段一律需要进程内重启。
var (
	hotSections     = map[string]bool{"mcp": true, "translation": true, "alerts": true} // alerts：T64，下一次评估即按新设置
	hotServerFields = map[string]bool{"log_level": true, "trusted_proxies": true, "admin_hosts": true}
)

// diffConfig 比较两份已 Normalize 的配置，返回模式与差异（HR-12）。
func diffConfig(oldCfg, newCfg *config.Config) (string, reloadChanges) {
	ch := reloadChanges{
		Pools:    namedDiff(poolsByName(oldCfg.Pools), poolsByName(newCfg.Pools)),
		Services: namedDiff(modelsByName(oldCfg.Models), modelsByName(newCfg.Models)),
	}
	ov, nv := reflect.ValueOf(*oldCfg), reflect.ValueOf(*newCfg)
	t := ov.Type()
	for i := 0; i < t.NumField(); i++ {
		name := yamlName(t.Field(i))
		switch {
		case name == "pools" || name == "models":
		case name == "server":
			st := t.Field(i).Type
			for j := 0; j < st.NumField(); j++ {
				sub := yamlName(st.Field(j))
				if reflect.DeepEqual(ov.Field(i).Field(j).Interface(), nv.Field(i).Field(j).Interface()) {
					continue
				}
				if hotServerFields[sub] {
					ch.Settings = append(ch.Settings, "server."+sub)
				} else {
					ch.RestartFields = append(ch.RestartFields, "server."+sub)
				}
			}
		case hotSections[name]:
			ch.Settings = append(ch.Settings, fieldDiffs(name, ov.Field(i), nv.Field(i))...)
		default:
			ch.RestartFields = append(ch.RestartFields, fieldDiffs(name, ov.Field(i), nv.Field(i))...)
		}
	}
	ch.normalize()
	switch {
	case len(ch.RestartFields) > 0:
		return reloadModeRestart, ch
	case len(ch.Settings) > 0 || !ch.Pools.empty() || !ch.Services.empty():
		return reloadModeHot, ch
	default:
		return reloadModeNone, ch
	}
}

// normalize 把 nil 切片换成空切片（JSON 输出 [] 而不是 null），并排序。
func (ch *reloadChanges) normalize() {
	for _, s := range []*[]string{&ch.Settings, &ch.RestartFields, &ch.Pools.Added, &ch.Pools.Removed, &ch.Pools.Changed,
		&ch.Services.Added, &ch.Services.Removed, &ch.Services.Changed} {
		if *s == nil {
			*s = []string{}
		}
		sort.Strings(*s)
	}
}

// fieldDiffs 比较一个配置段：结构体逐字段比较（返回 "段.字段"），其他类型整体比较。
func fieldDiffs(prefix string, a, b reflect.Value) []string {
	if a.Kind() != reflect.Struct {
		if reflect.DeepEqual(a.Interface(), b.Interface()) {
			return nil
		}
		return []string{prefix}
	}
	var out []string
	for i := 0; i < a.NumField(); i++ {
		if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
			out = append(out, prefix+"."+yamlName(a.Type().Field(i)))
		}
	}
	return out
}

func yamlName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "" {
		return strings.ToLower(f.Name)
	}
	return name
}

func poolsByName(ps []config.PoolConfig) map[string]any {
	m := make(map[string]any, len(ps))
	for _, p := range ps {
		m[p.Name] = p
	}
	return m
}

func modelsByName(ms []config.ModelConfig) map[string]any {
	m := make(map[string]any, len(ms))
	for _, x := range ms {
		m[x.Name] = x
	}
	return m
}

func namedDiff(oldM, newM map[string]any) changeSet {
	var c changeSet
	for name, nv := range newM {
		ov, ok := oldM[name]
		switch {
		case !ok:
			c.Added = append(c.Added, name)
		case !reflect.DeepEqual(ov, nv):
			c.Changed = append(c.Changed, name)
		}
	}
	for name := range oldM {
		if _, ok := newM[name]; !ok {
			c.Removed = append(c.Removed, name)
		}
	}
	return c
}

// ---------------------------------------------------------------- 重载记录（HR-18、ADM-19）

type reloadRecord struct {
	ID            int64          `json:"id"`
	Time          time.Time      `json:"time"`
	ActorClientID int64          `json:"actor_client_id"`
	Mode          string         `json:"mode"`
	Status        string         `json:"status"`
	Changes       *reloadChanges `json:"changes,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// reloadLog 保存最近 reloadRecordCap 次重载，新的在前。进程内重启时交给下一个实例。
type reloadLog struct {
	mu   sync.Mutex
	next int64
	recs []reloadRecord // guarded by mu
}

func (l *reloadLog) add(r reloadRecord) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	r.ID = l.next
	l.recs = append([]reloadRecord{r}, l.recs...)
	if len(l.recs) > reloadRecordCap {
		l.recs = l.recs[:reloadRecordCap]
	}
	return r.ID
}

func (l *reloadLog) finish(id int64, status, errMsg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.recs {
		if l.recs[i].ID == id {
			l.recs[i].Status, l.recs[i].Error = status, errMsg
			return
		}
	}
}

func (l *reloadLog) view() any {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]reloadRecord{}, l.recs...)
	return map[string]any{"records": out}
}

// ---------------------------------------------------------------- 监听套接字交接（§2.3）

const (
	// acceptQueue 是常驻 accept 协程与当前服务端之间的连接队列容量；满了 accept 协程阻塞，
	// 新连接留在内核的 accept 队列中（背压，不丢连接）。
	acceptQueue = 64
	// retryQueue 容纳被关闭中的服务端取到、退回给下一个服务端的连接（每个服务端最多退回一个）。
	retryQueue = 8
)

// sharedListener 持有真正的监听套接字，由一个常驻协程不断 Accept，放进有界队列。
// 进程内重启时它跨实例保留：旧服务端只关闭自己的虚拟监听器，不打断真正的 Accept
// （在 Windows 上取消进行中的 Accept 可能丢掉刚接受的连接），新服务端接着从队列中取连接。
type sharedListener struct {
	ln    net.Listener
	conns chan net.Conn
	retry chan net.Conn
	stop  chan struct{} // Close 时关闭
	done  chan struct{} // accept 协程退出时关闭；之后 err 只读
	err   error
	once  sync.Once
}

func listenTCP(addr string) (*sharedListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &sharedListener{ln: ln, conns: make(chan net.Conn, acceptQueue), retry: make(chan net.Conn, retryQueue),
		stop: make(chan struct{}), done: make(chan struct{})}
	go s.acceptLoop()
	return s, nil
}

func (s *sharedListener) acceptLoop() {
	defer close(s.done)
	for {
		c, err := s.ln.Accept()
		if err != nil {
			s.err = err
			return
		}
		select {
		case s.conns <- c:
		case <-s.stop:
			_ = c.Close() // 监听器已关闭：拒绝这个连接
			s.err = net.ErrClosed
			return
		}
	}
}

// Close 真正关闭套接字，并关闭队列中尚未被服务端取走的连接。
func (s *sharedListener) Close() error {
	var err error
	s.once.Do(func() {
		close(s.stop)
		err = s.ln.Close()
		<-s.done
		for {
			select {
			case c := <-s.retry:
				_ = c.Close() // 没有服务端会再处理它
			case c := <-s.conns:
				_ = c.Close() // 同上
			default:
				return
			}
		}
	})
	return err
}

func (s *sharedListener) requeue(c net.Conn) {
	select {
	case s.retry <- c:
	default:
		_ = c.Close() // 退回队列已满（不应发生）：只能放弃这个连接
	}
}

// server 返回给一个 http.Server 使用的虚拟监听器。
func (s *sharedListener) server() *handoverListener {
	return &handoverListener{shared: s, closed: make(chan struct{})}
}

// handoverListener 是某个 http.Server 看到的监听器。handover 为 true 时 Close 只关闭它自己，
// 真正的套接字留给下一个实例；否则连同 sharedListener 一起关闭。
type handoverListener struct {
	shared   *sharedListener
	handover atomic.Bool
	mu       sync.Mutex
	isClosed bool
	closed   chan struct{}
}

func (l *handoverListener) Accept() (net.Conn, error) {
	for {
		var c net.Conn
		select { // 先取被退回的连接，保持先来先服务
		case c = <-l.shared.retry:
		default:
			select {
			case c = <-l.shared.retry:
			case c = <-l.shared.conns:
			case <-l.closed:
				return nil, net.ErrClosed
			case <-l.shared.done:
				return nil, l.shared.err
			}
		}
		l.mu.Lock()
		closed := l.isClosed
		l.mu.Unlock()
		if closed { // 关闭与取连接同时发生：连接交给下一个服务端
			l.shared.requeue(c)
			return nil, net.ErrClosed
		}
		return c, nil
	}
}

func (l *handoverListener) Close() error {
	l.mu.Lock()
	if !l.isClosed {
		l.isClosed = true
		close(l.closed)
	}
	l.mu.Unlock()
	if l.handover.Load() {
		return nil
	}
	return l.shared.Close()
}

func (l *handoverListener) Addr() net.Addr { return l.shared.ln.Addr() }

// inherited 是进程内重启时上一个实例交给下一个实例的东西。
type inherited struct {
	mu        sync.Mutex
	listeners map[string]*sharedListener // 键为 "data|地址" / "admin|地址"（配置中的地址字符串）
	records   *reloadLog
	pendingID int64 // 触发本次重启的重载记录；0 = 无
	rollback  error // 非 nil：本次启动是回滚，值为新配置启动失败的原因
}

func listenerKey(role, addr string) string { return role + "|" + addr }

// take 取出上一个实例留下的、地址相同的套接字；没有则返回 nil。
func (in *inherited) take(role, addr string) *sharedListener {
	if in == nil {
		return nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	s := in.listeners[listenerKey(role, addr)]
	delete(in.listeners, listenerKey(role, addr))
	return s
}

// giveBack 把取出但没能使用的套接字放回（启动失败时，留给回滚）。
func (in *inherited) giveBack(role, addr string, ln *handoverListener) {
	if in == nil || ln == nil {
		return
	}
	in.mu.Lock()
	in.listeners[listenerKey(role, addr)] = ln.shared
	in.mu.Unlock()
}

// closeRest 关闭所有没被使用的套接字（新实例开始服务后、或进程退出前调用）。
func (in *inherited) closeRest() {
	if in == nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	for k, s := range in.listeners {
		_ = s.Close() // 多余的旧套接字；关闭失败无可处理
		delete(in.listeners, k)
	}
}

// listenerFor 优先使用继承的套接字，否则新建。fromInherit 表示来自上一个实例。
func listenerFor(in *inherited, role, addr string) (ln *handoverListener, fromInherit bool, err error) {
	if s := in.take(role, addr); s != nil {
		return s.server(), true, nil
	}
	s, err := listenTCP(addr)
	if err != nil {
		return nil, false, err
	}
	return s.server(), false, nil
}

// errRestart 由 run 返回，表示已按正常关闭顺序停止，需要用 next 重新启动（HR-15）。
type errRestart struct {
	next    *config.Config
	inherit *inherited
}

func (e *errRestart) Error() string { return "restart requested" }

// startupError 表示 run 在开始服务之前失败（HR-16：进程内重启后据此回滚）。
type startupError struct{ err error }

func (e *startupError) Error() string { return e.err.Error() }
func (e *startupError) Unwrap() error { return e.err }

// serveLoop 运行网关；进程内重启时用新配置再次调用 run，新配置启动失败时用旧配置回滚（HR-15、HR-16）。
func serveLoop(ctx context.Context, cfg *config.Config, opts runOptions) error {
	var prev *config.Config
	for {
		err := run(ctx, cfg, opts)
		var rs *errRestart
		if errors.As(err, &rs) {
			prev, cfg, opts.inherit = cfg, rs.next, rs.inherit
			continue
		}
		var se *startupError
		if errors.As(err, &se) && prev != nil && ctx.Err() == nil {
			opts.inherit.rollback = se.err
			cfg, prev = prev, nil // 回滚也失败时不再重试
			continue
		}
		opts.inherit.closeRest()
		return err
	}
}

// ---------------------------------------------------------------- 一代（HR-13）

// genDeps 是构造一代时沿用的有状态组件。
type genDeps struct {
	mem       *ledger.Memory
	sched     *scheduler.Scheduler
	auditPipe *audit.Pipeline
	index     *auth.Index
	pepper    []byte
	limiter   *ratelimit.Limiter
	ipLimiter *ratelimit.Limiter
	overload  *httpapi.OverloadGuard
	budget    *responseBudget
	files     *files.Store // 文件中转（T69）；nil = 没有开启
	logger    *slog.Logger
}

// generation 是热重载时整体替换的部分：注册表、内核、MCP 与数据面路由。
type generation struct {
	cfg      *config.Config
	registry *protocol.Registry
	client   *http.Client
	connect  time.Duration // client 的连接超时；相同时下一代沿用 client
	handler  http.Handler
}

// buildGeneration 用 cfg 构造新一代；prev 非 nil 且连接超时相同时沿用其上游 client。不修改任何共享状态。
func buildGeneration(cfg *config.Config, d genDeps, prev *generation) (*generation, error) {
	connect := maxConnectTimeout(cfg)
	client := (*http.Client)(nil)
	if prev != nil && prev.connect == connect {
		client = prev.client
	} else {
		client = newUpstreamClientWithBudget(cfg, d.budget)
	}
	registry, err := newRegistry(cfg, d.logger)
	if err != nil {
		return nil, fmt.Errorf("register adapters: %w", err)
	}
	kernel, err := core.New(core.Deps{
		Config: withRestUnitPrices(cfg, restUnits(registry)), Registry: registry,
		Ledger: quotaLedger{Ledger: d.mem, sched: d.sched}, Scheduler: d.sched, // QT-12
		Audit: d.auditPipe, HTTPClient: client, Logger: d.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create core: %w", err)
	}
	trusted, err := clientip.ParseTrusted(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("parse trusted_proxies: %w", err)
	}
	dataDeps := httpapi.DataDeps{
		Config: cfg, Core: kernel, Registry: registry, Index: d.index, Pepper: d.pepper,
		Limiter: d.limiter, IPLimiter: d.ipLimiter, Overload: d.overload, Trusted: trusted, Files: d.files, Logger: d.logger,
	}
	if !cfg.MCP.Disabled {
		dataDeps.MCP = newMCPServer(cfg, registry, kernel, d.files, d.logger)
		dataDeps.MCPErrorWriter = mcp.WriteHTTPError
	}
	return &generation{cfg: cfg, registry: registry, client: client, connect: connect, handler: httpapi.NewDataRouter(dataDeps)}, nil
}

func newMCPServer(cfg *config.Config, registry *protocol.Registry, kernel *core.Core, fs *files.Store, logger *slog.Logger) *mcp.Server {
	return mcp.New(mcp.Deps{
		Registry: registry, Core: kernel, MaxResultBytes: cfg.MCP.MaxResultBytes, Version: version, Logger: logger,
		ToolMode: cfg.MCP.ToolMode, AutoThreshold: cfg.MCP.AutoThreshold, PinnedTools: cfg.MCP.PinnedTools, Files: fs,
	})
}

func configuredModelNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		names = append(names, m.Name)
	}
	return names
}

// ---------------------------------------------------------------- 重载（HR-10 ~ HR-19）

// restartRequest 由重载处理器交给 run 的主循环，触发进程内重启。
type restartRequest struct {
	cfg      *config.Config
	prepared map[string]*sharedListener // 预检时为新地址绑定的套接字
	recordID int64
}

// configReloader 实现配置重载。由 run 创建，管理 API 通过 Reload 调用。
type configReloader struct {
	path   string // HR-01；空 = 不支持重载
	mu     sync.Mutex
	logger *slog.Logger
	gd     genDeps

	cur      atomic.Pointer[config.Config]
	gen      atomic.Pointer[generation]
	handler  atomic.Pointer[http.Handler] // 数据面当前处理器
	levelVar *slog.LevelVar
	master   []byte

	st        store.Store
	keyring   *crypto.Keyring
	adminCtl  *admin.Control
	collector *metrics.Collector
	records   *reloadLog
	restartCh chan *restartRequest
	restarted atomic.Bool // 已触发进程内重启：之后的重载一律 409
}

func (rl *configReloader) install(g *generation) {
	h := g.handler
	rl.handler.Store(&h)
	rl.gen.Store(g)
	rl.cur.Store(g.cfg)
}

// ServeHTTP 把请求交给当前一代的数据面处理器（只有一次原子读，HR-13）。
func (rl *configReloader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*rl.handler.Load()).ServeHTTP(w, r)
}

type reloadResponse struct {
	Mode     string        `json:"mode"`
	Applied  bool          `json:"applied"`
	RecordID int64         `json:"record_id,omitempty"`
	Changes  reloadChanges `json:"changes"`
	Warnings []string      `json:"warnings"`
}

func reloadError(status int, code, msg string) (int, any) {
	return status, map[string]any{"error": map[string]any{"code": code, "message": msg}}
}

// Reload 实现 ADM-18 与 HR-10 ~ HR-17。
func (rl *configReloader) Reload(ctx context.Context, actor int64, dryRun bool) (int, any) {
	if rl.path == "" {
		return reloadError(http.StatusNotFound, "not_found", "reload is not available: the gateway was not started with a config file")
	}
	if rl.restarted.Load() || !rl.mu.TryLock() { // HR-10
		return reloadError(http.StatusConflict, "conflict", "reload in progress")
	}
	restarting := false
	defer func() {
		if !restarting {
			rl.mu.Unlock()
		}
	}()

	newCfg, err := config.Load(rl.path) // HR-11
	if err != nil {
		rl.record(actor, "", reloadStatusFailed, nil, err, dryRun)
		return reloadError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	oldCfg := rl.cur.Load()
	mode, ch := diffConfig(oldCfg, newCfg) // HR-12
	resp := reloadResponse{Mode: mode, Changes: ch, Warnings: []string{}}

	switch mode {
	case reloadModeNone:
		if !dryRun {
			rl.record(actor, mode, reloadStatusNoop, &ch, nil, false)
		}
		return http.StatusOK, resp

	case reloadModeHot:
		g, err := buildGeneration(newCfg, rl.gd, rl.gen.Load())
		if err != nil {
			rl.record(actor, mode, reloadStatusFailed, &ch, err, dryRun)
			return reloadError(http.StatusUnprocessableEntity, "invalid_request", err.Error())
		}
		resp.Warnings = append(resp.Warnings, orphanWarnings(ch)...)
		if dryRun {
			return http.StatusOK, resp
		}
		if err := rl.applyHot(ctx, g); err != nil {
			rl.record(actor, mode, reloadStatusFailed, &ch, err, false)
			return reloadError(http.StatusUnprocessableEntity, "internal", err.Error())
		}
		resp.Applied = true
		resp.RecordID = rl.record(actor, mode, reloadStatusApplied, &ch, nil, false)
		return http.StatusOK, resp

	default: // restart
		prepared, warnings, err := rl.preflight(newCfg, oldCfg, dryRun)
		resp.Warnings = append(resp.Warnings, warnings...)
		if err != nil {
			rl.record(actor, mode, reloadStatusFailed, &ch, err, dryRun)
			return reloadError(http.StatusUnprocessableEntity, "invalid_request", err.Error())
		}
		if dryRun {
			return http.StatusOK, resp
		}
		resp.Applied = true
		resp.RecordID = rl.record(actor, mode, reloadStatusRestarting, &ch, nil, false)
		restarting = true // 保持锁：本实例即将关闭
		rl.restarted.Store(true)
		rl.restartCh <- &restartRequest{cfg: newCfg, prepared: prepared, recordID: resp.RecordID} // 容量为 1，且只发送一次
		return http.StatusAccepted, resp
	}
}

// applyHot 按 HR-13 的顺序应用新一代。数据库读取放在第一步：失败时什么都没改。
func (rl *configReloader) applyHot(ctx context.Context, g *generation) error {
	keys, err := scheduler.LoadKeys(ctx, rl.st, rl.keyring, rl.logger)
	if err != nil {
		return fmt.Errorf("load upstream keys: %w", err)
	}
	prev := rl.gen.Load()
	rl.gd.sched.SetPools(scheduler.PoolsFromConfig(g.cfg.Pools))
	rl.gd.sched.SetKeys(keys)
	if rl.adminCtl != nil {
		rl.adminCtl.SetConfig(g.cfg)
	}
	rl.levelVar.Set(obs.ParseLevel(g.cfg.Server.LogLevel))
	rl.collector.RestrictModels(configuredModelNames(g.cfg))
	rl.install(g)
	if prev != nil && prev.client != g.client { // HR-14
		prev.client.CloseIdleConnections()
	}
	var oldCfg *config.Config
	if prev != nil {
		oldCfg = prev.cfg
	}
	if pools := quotaPoolsToRecompute(oldCfg, g.cfg); len(pools) > 0 { // QT-11：失败只记 Warn，不影响已生效的配置
		recomputeQuotaUsage(ctx, rl.st, rl.gd.sched, pools, rl.logger)
	}
	return nil
}

// preflight 实现 §2.3 的进程内重启预检。dryRun 时探测到的资源立即释放。
func (rl *configReloader) preflight(newCfg, oldCfg *config.Config, dryRun bool) (map[string]*sharedListener, []string, error) {
	var warnings []string
	if _, err := buildGeneration(newCfg, rl.gd, nil); err != nil { // 1：可以构造（不连接上游，不修改共享状态）
		return nil, nil, err
	}
	prepared := map[string]*sharedListener{}
	release := func() {
		for _, ln := range prepared {
			_ = ln.Close() // 预检失败或 dry-run：释放探测用的套接字
		}
	}
	for _, l := range []struct{ role, oldAddr, newAddr string }{ // 2：新地址先绑定
		{"data", oldCfg.Server.DataListen, newCfg.Server.DataListen},
		{"admin", oldCfg.Server.AdminListen, newCfg.Server.AdminListen},
	} {
		if l.oldAddr == l.newAddr {
			continue
		}
		ln, err := listenTCP(l.newAddr)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("%s listen address %s is not available: %w", l.role, l.newAddr, err)
		}
		prepared[listenerKey(l.role, l.newAddr)] = ln
		warnings = append(warnings, fmt.Sprintf("%s plane moves from %s to %s; clients must use the new address", l.role, l.oldAddr, l.newAddr))
	}
	if newCfg.Server.DataDir != oldCfg.Server.DataDir { // 3：新目录可用
		if err := probeDataDir(newCfg.Server.DataDir); err != nil {
			release()
			return nil, nil, err
		}
		if _, err := os.Stat(filepath.Join(newCfg.Server.DataDir, "gateway.db")); errors.Is(err, os.ErrNotExist) {
			warnings = append(warnings, "data_dir changes to a new database: clients, keys and usage start empty, and a new bootstrap admin token will be printed to stderr")
		}
	}
	if newCfg.Server.MasterKeyFile != oldCfg.Server.MasterKeyFile && newCfg.Server.DataDir == oldCfg.Server.DataDir { // 4：待定 5
		data, err := os.ReadFile(newCfg.Server.MasterKeyFile)
		switch {
		case errors.Is(err, os.ErrNotExist):
			release()
			return nil, nil, fmt.Errorf("master_key_file %s does not exist; changing the master key without changing data_dir would make every stored key and token unusable", newCfg.Server.MasterKeyFile)
		case err != nil:
			release()
			return nil, nil, fmt.Errorf("read master_key_file: %w", err)
		case !bytes.Equal(data, rl.master):
			release()
			return nil, nil, errors.New("master_key_file contains a different key; changing the master key without changing data_dir would make every stored key and token unusable")
		}
	}
	if dryRun {
		release()
		return nil, warnings, nil
	}
	return prepared, warnings, nil
}

// probeDataDir 检查新数据目录可创建、目录锁可获取，然后立即释放。
func probeDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dirLockProbeTimeout)
	defer cancel()
	lock, err := sqlite.AcquireDirLock(ctx, filepath.Join(dir, "gateway.lock"))
	if err != nil {
		return fmt.Errorf("data dir %s is not usable: %w", dir, err)
	}
	return lock.Close()
}

func orphanWarnings(ch reloadChanges) []string {
	var out []string
	for _, p := range ch.Pools.Removed {
		out = append(out, fmt.Sprintf("pool %s is removed: its keys stay in the database as orphaned and return if the pool is added again", p))
	}
	return out
}

// record 写 HR-18 的日志，并在非 dry-run 时追加记录。返回记录 ID（dry-run 为 0）。
func (rl *configReloader) record(actor int64, mode, status string, ch *reloadChanges, err error, dryRun bool) int64 {
	attrs := []any{"actor_client_id", actor, "mode", mode, "status", status, "dry_run", dryRun}
	if ch != nil {
		attrs = append(attrs,
			"pools_added", ch.Pools.Added, "pools_removed", ch.Pools.Removed, "pools_changed", ch.Pools.Changed,
			"services_added", ch.Services.Added, "services_removed", ch.Services.Removed, "services_changed", ch.Services.Changed,
			"settings", ch.Settings, "restart_fields", ch.RestartFields)
	}
	msg := ""
	if err != nil {
		// 错误原文可能引用配置中的值，只放进管理员可见的记录（ADM-19），日志不记（HR-18）
		msg = err.Error()
		attrs = append(attrs, "failed", true)
	}
	rl.logger.Info("config reload", attrs...)
	if dryRun {
		return 0
	}
	return rl.records.add(reloadRecord{Time: time.Now().UTC(), ActorClientID: actor, Mode: mode, Status: status, Changes: ch, Error: msg})
}

// toolsView 实现 ADM-20：当前一代注册表中的全部工具（随热重载更新）。
func (rl *configReloader) toolsView() admin.ToolsView {
	g := rl.gen.Load()
	mode := g.cfg.MCP.ToolMode
	if g.cfg.MCP.Disabled {
		mode = "disabled"
	}
	out := admin.ToolsView{ToolMode: mode, Tools: []admin.ToolView{}}
	for _, e := range g.registry.Tools() { // 已按名称升序
		summary, _, _ := strings.Cut(e.Tool.Description, "\n")
		out.Tools = append(out.Tools, admin.ToolView{
			Name: e.Tool.Name, Title: e.Tool.Title, Summary: strings.TrimSpace(summary), Service: e.Tool.Model,
			Protocol: string(e.Ingress.Protocol()), ReadOnly: e.Tool.ReadOnly,
		})
	}
	return out
}
