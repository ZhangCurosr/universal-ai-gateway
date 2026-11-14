package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/clientip"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// Deps 是管理面的依赖。
type Deps struct {
	Config    *config.Config
	Store     store.Store
	Keyring   *crypto.Keyring
	Index     *auth.Index
	Reloader  *auth.Reloader
	Ledger    *ledger.Memory
	Scheduler *scheduler.Scheduler
	Logger    *slog.Logger
	Version   string
	StartedAt time.Time
	Now       func() time.Time // 为 nil 时用 time.Now
	Metrics   http.Handler     // Prometheus 指标端点（/admin/metrics）

	// HostCheck 为 true 时按 spec/09 UI-20 检查 Host 头（本机地址 + AdminHosts）；零值不检查（UI-21）。
	HostCheck  bool
	AdminHosts []string

	// health 的附加字段（spec/09 §6.4）；为 nil 时不输出对应字段。
	LedgerQueue  func() (length, capacity int)
	AuditDropped func() int64

	// Reload 执行一次配置重载（ADM-18，spec/03 §8）；返回 HTTP 状态码与响应体。为 nil 时端点返回 404。
	// actor 是发起重载的管理员客户端 ID。
	Reload func(ctx context.Context, actor int64, dryRun bool) (status int, body any)
	// ReloadRecords 返回最近的重载记录（ADM-19）；为 nil 时端点返回 404。
	ReloadRecords func() any
	// Tools 返回当前注册表中的工具（ADM-20）；为 nil 时端点返回 404。
	Tools func() ToolsView
	// MCP 是管理面 MCP 端点（ADM-21，spec/14 §6），挂在 /admin/mcp，经过管理面的全部中间件；为 nil 时不挂载。
	MCP http.Handler
}

// Control 让调用方在运行中替换管理面读取的配置（配置热重载，spec/03 §8）。
type Control struct{ s *server }

// SetConfig 原子替换配置：池、模型列表、可信代理与 admin_hosts 都按新配置生效。
func (c *Control) SetConfig(cfg *config.Config) { c.s.setConfig(cfg) }

// netState 是由配置推导的网络判定参数，整体原子替换。
type netState struct {
	trusted clientip.Trusted
	hosts   map[string]bool // UI-20 允许的 Host 主机名（小写）；只在 d.HostCheck 时使用
}

type server struct {
	d         Deps
	cfg       atomic.Pointer[config.Config] // 当前配置；可能为 nil
	net       atomic.Pointer[netState]
	ipTracker *ipFailureTracker

	// writeMu 串行化所有写操作（创建/修改/轮换）。管理面写入频率很低，串行化后
	// 「读-改-写」不会互相覆盖（例如 rotate-token 把刚禁用的客户端写回启用），
	// ADM-03 的「至少保留一个管理员」检查与写入之间也不会被并发请求穿插。
	writeMu sync.Mutex
}

// serial 把写操作 handler 包成串行执行。
func (s *server) serial(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		h(w, r)
	}
}

type ipFailureTracker struct {
	mu       sync.Mutex
	failures map[string][]time.Time // guarded by mu
}

func newIPFailureTracker() *ipFailureTracker {
	return &ipFailureTracker{
		failures: make(map[string][]time.Time),
	}
}

func (t *ipFailureTracker) isLocked(ip string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	times, ok := t.failures[ip]
	if !ok {
		return false
	}
	cutoff := now.Add(-time.Minute)
	i := 0
	for _, ts := range times {
		if ts.After(cutoff) {
			times[i] = ts
			i++
		}
	}
	times = times[:i]
	if len(times) == 0 {
		delete(t.failures, ip)
		return false
	}
	t.failures[ip] = times
	return len(times) >= 20
}

func (t *ipFailureTracker) recordFailure(ip string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	times := t.failures[ip]
	i := 0
	for _, ts := range times {
		if ts.After(cutoff) {
			times[i] = ts
			i++
		}
	}
	times = append(times[:i], now)
	t.failures[ip] = times
}

// NewServer 返回管理面 http.Handler（spec/09 §4）。
func NewServer(d Deps) http.Handler {
	h, _ := NewServerWithControl(d)
	return h
}

// NewServerWithControl 与 NewServer 相同，另外返回用于热重载配置的 Control。
func NewServerWithControl(d Deps) (http.Handler, *Control) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = obs.Discard()
	}

	s := &server{
		d:         d,
		ipTracker: newIPFailureTracker(),
	}
	s.cfg.Store(d.Config)
	s.net.Store(&netState{trusted: s.parseTrusted(d.Config), hosts: allowedHostSet(d.AdminHosts)})

	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /admin/api/health", s.handleHealth)

	// Clients
	mux.HandleFunc("POST /admin/api/clients", s.serial(s.handleCreateClient))
	mux.HandleFunc("GET /admin/api/clients", s.handleListClients)
	mux.HandleFunc("GET /admin/api/clients/{id}", s.handleGetClient)
	mux.HandleFunc("PATCH /admin/api/clients/{id}", s.serial(s.handleUpdateClient))
	mux.HandleFunc("POST /admin/api/clients/{id}/rotate-token", s.serial(s.handleRotateToken))

	// Keys
	mux.HandleFunc("POST /admin/api/keys", s.serial(s.handleCreateKey))
	mux.HandleFunc("GET /admin/api/keys", s.handleListKeys)
	mux.HandleFunc("PATCH /admin/api/keys/{id}", s.serial(s.handleUpdateKey))
	mux.HandleFunc("POST /admin/api/keys/import", s.serial(s.handleImportKeys))

	// Usage
	mux.HandleFunc("GET /admin/api/usage", s.handleQueryUsage)

	// 管理 Web UI 用的只读配置（spec/09 §6.4）
	mux.HandleFunc("GET /admin/api/config", s.handleConfig)

	// 管理 Web UI 静态资源（UI-01）
	mux.HandleFunc(uiRoot, serveUI)
	mux.HandleFunc(uiPrefix, serveUI)

	// Metrics (MTR-01 ~ MTR-03)
	mux.HandleFunc("/admin/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.d.Metrics != nil {
			s.d.Metrics.ServeHTTP(w, r)
			return
		}
		writeError(w, apperr.New(apperr.CodeNotFound, "metrics not configured"))
	})

	// 配置重载（ADM-18、ADM-19）
	mux.HandleFunc("POST /admin/api/reload", s.handleReload)
	mux.HandleFunc("GET /admin/api/reload", s.handleReloadRecords)

	// 工具列表（ADM-20）与管理面 MCP（ADM-21）
	mux.HandleFunc("GET /admin/api/tools", s.handleTools)
	if d.MCP != nil {
		mux.Handle("POST /admin/mcp", d.MCP)
		mux.HandleFunc("/admin/mcp", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		})
	}

	// Fallback
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, apperr.New(apperr.CodeNotFound, "route not found"))
	})

	return s.wrap(mux), &Control{s: s}
}

// config 返回当前配置（可能为 nil）。
func (s *server) config() *config.Config { return s.cfg.Load() }

func (s *server) parseTrusted(cfg *config.Config) clientip.Trusted {
	if cfg == nil || len(cfg.Server.TrustedProxies) == 0 {
		return clientip.Trusted{}
	}
	trusted, err := clientip.ParseTrusted(cfg.Server.TrustedProxies)
	if err != nil {
		s.d.Logger.Warn("admin: parse trusted proxies failed", "error", err)
	}
	return trusted
}

func (s *server) setConfig(cfg *config.Config) {
	hosts := []string(nil)
	if cfg != nil {
		hosts = cfg.Server.AdminHosts
	}
	s.net.Store(&netState{trusted: s.parseTrusted(cfg), hosts: allowedHostSet(hosts)})
	s.cfg.Store(cfg)
}

// handleReload 实现 ADM-18。
func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.d.Reload == nil {
		writeError(w, apperr.New(apperr.CodeNotFound, "reload is not available"))
		return
	}
	var req struct {
		DryRun bool `json:"dry_run"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, err)
			return
		}
	}
	var actor int64
	if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
		actor = p.ClientID
	}
	status, body := s.d.Reload(r.Context(), actor, req.DryRun)
	writeJSON(w, status, body)
}

// handleTools 实现 ADM-20。
func (s *server) handleTools(w http.ResponseWriter, _ *http.Request) {
	if s.d.Tools == nil {
		writeError(w, apperr.New(apperr.CodeNotFound, "tool list is not available"))
		return
	}
	writeJSON(w, http.StatusOK, s.d.Tools())
}

// handleReloadRecords 实现 ADM-19。
func (s *server) handleReloadRecords(w http.ResponseWriter, _ *http.Request) {
	if s.d.ReloadRecords == nil {
		writeError(w, apperr.New(apperr.CodeNotFound, "reload is not available"))
		return
	}
	writeJSON(w, http.StatusOK, s.d.ReloadRecords())
}

func (s *server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := s.d.Now()

		// 1. Request ID (MW-01)
		reqID := obs.NewRequestID()
		w.Header().Set("X-Request-Id", reqID)
		ctx := obs.WithRequestID(r.Context(), reqID)
		r = r.WithContext(ctx)

		// DNS 重绑定防护（UI-20）：在 IP 锁定计数之前，对所有路径生效
		if s.d.HostCheck && !s.hostAllowed(r.Host) {
			// 421 不在 apperr 的状态码表里（新增错误码需要改第 0 层），这里直接写同样格式的错误体
			writeJSON(w, http.StatusMisdirectedRequest, map[string]any{"error": map[string]any{
				"code": string(apperr.CodeInvalidRequest), "message": "host not allowed",
			}})
			return
		}

		// /admin/metrics 端点与管理页面静态资源无需鉴权与 IP 锁定（MTR-01, MTR-02, UI-02）
		if r.URL.Path == "/admin/metrics" || r.URL.Path == uiRoot || strings.HasPrefix(r.URL.Path, uiPrefix) {
			next.ServeHTTP(w, r)
			return
		}

		// 2. IP Lockout (ADM-04)
		remoteIP := s.net.Load().trusted.From(r)
		ipStr := "unknown"
		if remoteIP.IsValid() {
			ipStr = remoteIP.String()
		}

		if s.ipTracker.isLocked(ipStr, now) {
			writeError(w, apperr.New(apperr.CodeRateLimited, "too many authentication failures"))
			return
		}

		// 3. Admin Authentication (ADM-01, ADM-02)
		cred, ok := auth.ExtractCredential(r.Header)
		if !ok {
			s.ipTracker.recordFailure(ipStr, now)
			writeError(w, apperr.New(apperr.CodeUnauthenticated, "unauthenticated"))
			return
		}

		if s.d.Keyring == nil || s.d.Index == nil {
			writeError(w, apperr.New(apperr.CodeInternal, "authentication service unavailable"))
			return
		}

		tokenHash := auth.HashToken(s.d.Keyring.TokenPepper(), cred)
		principal, ok := s.d.Index.Lookup(tokenHash)
		if !ok {
			s.ipTracker.recordFailure(ipStr, now)
			writeError(w, apperr.New(apperr.CodeUnauthenticated, "unauthenticated"))
			return
		}

		if principal.Role != store.RoleAdmin {
			s.ipTracker.recordFailure(ipStr, now)
			writeError(w, apperr.New(apperr.CodePermissionDenied, "admin role required"))
			return
		}

		if principal.Disabled {
			s.ipTracker.recordFailure(ipStr, now)
			writeError(w, apperr.New(apperr.CodePermissionDenied, "principal is disabled"))
			return
		}

		// Store principal in context
		ctx = auth.WithPrincipal(ctx, principal)
		r = r.WithContext(ctx)

		// 4. Request Body Limit 1 MiB
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}

		next.ServeHTTP(w, r)
	})
}

func parseID(r *http.Request, name string) (int64, error) {
	val := r.PathValue(name)
	if val == "" {
		return 0, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("missing %s", name))
	}
	id, err := strconv.ParseInt(val, 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("invalid %s: %s", name, val))
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return apperr.New(apperr.CodeInvalidRequest, "request body required")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return apperr.New(apperr.CodeRequestTooLarge, "request body too large")
		}
		if errors.Is(err, io.EOF) {
			return apperr.New(apperr.CodeInvalidRequest, "request body empty")
		}
		return apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("invalid json: %v", err))
	}
	return nil
}
