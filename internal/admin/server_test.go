package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store/sqlite"
)

type testEnv struct {
	server    http.Handler
	store     *sqlite.DB
	keyring   *crypto.Keyring
	index     *auth.Index
	reloader  *auth.Reloader
	ledger    *ledger.Memory
	scheduler *scheduler.Scheduler
	cfg       *config.Config
	adminTok  string
	adminID   int64
	now       *time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, nil)
}

// newTestEnvWith 与 newTestEnv 相同，构造服务端之前可以用 mutate 修改依赖。
func newTestEnvWith(t *testing.T, mutate func(*Deps)) *testEnv {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	nowPtr := &now

	nowFunc := func() time.Time {
		return *nowPtr
	}

	dbPath := filepath.Join(t.TempDir(), "admin_test.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i + 1)
	}
	kr, err := crypto.NewKeyring(masterKey, 1)
	if err != nil {
		t.Fatalf("crypto.NewKeyring: %v", err)
	}

	idx := auth.NewIndex()
	reloader := &auth.Reloader{Store: db, Index: idx}
	mem := ledger.NewMemory(ledger.MemoryConfig{EventQueueCapacity: 1024})

	cfg := &config.Config{
		Server: config.ServerConfig{
			TrustedProxies: []string{"10.0.0.0/8"},
		},
		Pools: []config.PoolConfig{
			{Name: "openai-pool", Protocol: "openai"},
			{Name: "anthropic-pool", Protocol: "anthropic"},
		},
		Models: []config.ModelConfig{
			{Name: "gpt-4o", Pool: "openai-pool"},
			{Name: "claude-3-5-sonnet", Pool: "anthropic-pool"},
		},
	}

	sched := scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{
		Now:    nowFunc,
		Logger: obs.Discard(),
	})

	adminToken, display, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("auth.GenerateToken: %v", err)
	}
	tokenHash := auth.HashToken(kr.TokenPepper(), adminToken)
	adminID, err := db.CreateClient(ctx, &store.Client{
		Name:           "admin",
		Role:           store.RoleAdmin,
		TokenHash:      tokenHash,
		TokenDisplay:   display,
		QuotaUnlimited: true,
		RPMLimit:       1000,
		Disabled:       false,
	})
	if err != nil {
		t.Fatalf("db.CreateClient: %v", err)
	}

	if err := reloader.Reload(ctx); err != nil {
		t.Fatalf("reloader.Reload: %v", err)
	}
	mem.UpsertAccount(ledger.Account{
		ClientID:  adminID,
		Unlimited: true,
	})

	deps := Deps{
		Config:    cfg,
		Store:     db,
		Keyring:   kr,
		Index:     idx,
		Reloader:  reloader,
		Ledger:    mem,
		Scheduler: sched,
		Logger:    obs.Discard(),
		Version:   "1.0.0-test",
		StartedAt: now.Add(-100 * time.Second),
		Now:       nowFunc,
	}

	if mutate != nil {
		mutate(&deps)
	}
	srv := NewServer(deps)
	return &testEnv{
		server:    srv,
		store:     db,
		keyring:   kr,
		index:     idx,
		reloader:  reloader,
		ledger:    mem,
		scheduler: sched,
		cfg:       cfg,
		adminTok:  adminToken,
		adminID:   adminID,
		now:       nowPtr,
	}
}

func (e *testEnv) req(method, path string, body any, token string, remoteAddr ...string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			bodyReader = strings.NewReader(v)
		case []byte:
			bodyReader = bytes.NewReader(v)
		default:
			b, _ := json.Marshal(v)
			bodyReader = bytes.NewReader(b)
		}
	}
	r := httptest.NewRequest(method, path, bodyReader)
	if len(remoteAddr) > 0 {
		r.RemoteAddr = remoteAddr[0]
	} else {
		r.RemoteAddr = "127.0.0.1:54321"
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.server.ServeHTTP(rec, r)
	return rec
}

func TestHealth(t *testing.T) {
	env := newTestEnv(t)

	// No auth -> 401
	rec := env.req("GET", "/admin/api/health", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}

	// With admin auth -> 200
	rec = env.req("GET", "/admin/api/health", nil, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if res["status"] != "ok" {
		t.Errorf("status = %v, want ok", res["status"])
	}
	if res["version"] != "1.0.0-test" {
		t.Errorf("version = %v, want 1.0.0-test", res["version"])
	}
	if res["uptime_seconds"] != float64(100) {
		t.Errorf("uptime_seconds = %v, want 100", res["uptime_seconds"])
	}
}

func TestClientsCRUD(t *testing.T) {
	env := newTestEnv(t)

	// POST /admin/api/clients - success
	rpm60 := 60
	createReq := CreateClientRequest{
		Name:          "alice",
		Role:          "user",
		QuotaNanoUSD:  5000,
		RPMLimit:      &rpm60,
		AllowedModels: []string{"gpt-4o"},
	}
	rec := env.req("POST", "/admin/api/clients", createReq, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Client ClientView `json:"client"`
		Token  string     `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created.Client.Name != "alice" || created.Client.Role != "user" {
		t.Errorf("unexpected client: %+v", created.Client)
	}
	if !strings.HasPrefix(created.Token, auth.TokenPrefix) {
		t.Errorf("token does not have prefix: %s", created.Token)
	}
	aliceID := created.Client.ID

	// Verify Reload made alice's account immediately visible in auth.Index
	tokenHash := auth.HashToken(env.keyring.TokenPepper(), created.Token)
	p, ok := env.index.Lookup(tokenHash)
	if !ok || p.Name != "alice" {
		t.Errorf("expected alice in auth index, got ok=%v, p=%+v", ok, p)
	}

	// Verify alice (role="user") cannot call admin endpoints -> 403
	rec = env.req("GET", "/admin/api/clients", nil, created.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user calling admin API got %d, want 403", rec.Code)
	}

	// Validation errors on POST
	tests := []struct {
		name string
		req  any
		want int
	}{
		{"empty name", CreateClientRequest{Name: "  "}, 400},
		{"name too long", CreateClientRequest{Name: strings.Repeat("x", 65)}, 400},
		{"unknown role", CreateClientRequest{Name: "bob", Role: "superadmin"}, 400},
		{"negative quota", CreateClientRequest{Name: "bob", QuotaNanoUSD: -1}, 400},
		{"negative rpm", CreateClientRequest{Name: "bob", RPMLimit: ptr(-1)}, 400},
		{"unknown model", CreateClientRequest{Name: "bob", AllowedModels: []string{"non-existent-model"}}, 400},
		{"unknown field", `{"name":"bob","unknown_field":123}`, 400},
		{"body too large", `{"name":"` + strings.Repeat("a", (1<<20)+100) + `"}`, 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := env.req("POST", "/admin/api/clients", tt.req, env.adminTok)
			if r.Code != tt.want {
				t.Fatalf("status = %d, want %d, body=%s", r.Code, tt.want, r.Body.String())
			}
		})
	}

	// GET /admin/api/clients
	rec = env.req("GET", "/admin/api/clients", nil, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("list clients failed: %d", rec.Code)
	}
	var listRes struct {
		Clients []ClientView `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listRes); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listRes.Clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(listRes.Clients))
	}

	// GET /admin/api/clients/{id}
	rec = env.req("GET", fmt.Sprintf("/admin/api/clients/%d", aliceID), nil, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("get client failed: %d", rec.Code)
	}
	var getRes struct {
		Client ClientView `json:"client"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &getRes); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}
	if getRes.Client.ID != aliceID {
		t.Errorf("got id %d, want %d", getRes.Client.ID, aliceID)
	}

	// GET non-existent client -> 404
	rec = env.req("GET", "/admin/api/clients/999999", nil, env.adminTok)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get 999999 status = %d, want 404", rec.Code)
	}

	// GET invalid id -> 400
	rec = env.req("GET", "/admin/api/clients/abc", nil, env.adminTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("get abc status = %d, want 400", rec.Code)
	}

	// PATCH /admin/api/clients/{id} - update alice
	newName := "alice-updated"
	newQuota := int64(10000)
	patchReq := UpdateClientRequest{
		Name:         &newName,
		QuotaNanoUSD: &newQuota,
	}
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/clients/%d", aliceID), patchReq, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch client failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var patchRes struct {
		Client ClientView `json:"client"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &patchRes); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if patchRes.Client.Name != "alice-updated" || patchRes.Client.QuotaNanoUSD != 10000 {
		t.Errorf("patch result mismatch: %+v", patchRes.Client)
	}
}

func TestCannotDisableLastAdmin(t *testing.T) {
	env := newTestEnv(t)

	// Currently only 1 admin exists (env.adminID)
	// Try disabling the last admin -> 409
	rec := env.req("PATCH", fmt.Sprintf("/admin/api/clients/%d", env.adminID), UpdateClientRequest{
		Disabled: ptr(true),
	}, env.adminTok)
	if rec.Code != http.StatusConflict {
		t.Fatalf("disable last admin status = %d, want 409: body=%s", rec.Code, rec.Body.String())
	}

	// Try demoting the last admin -> 409
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/clients/%d", env.adminID), UpdateClientRequest{
		Role: ptr("user"),
	}, env.adminTok)
	if rec.Code != http.StatusConflict {
		t.Fatalf("demote last admin status = %d, want 409: body=%s", rec.Code, rec.Body.String())
	}

	// Create a second admin
	rec = env.req("POST", "/admin/api/clients", CreateClientRequest{
		Name: "admin2",
		Role: "admin",
	}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create admin2 failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Client ClientView `json:"client"`
		Token  string     `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	admin2ID := created.Client.ID

	// Now disabling the first admin should succeed because admin2 is active
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/clients/%d", env.adminID), UpdateClientRequest{
		Disabled: ptr(true),
	}, created.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable admin1 with admin2 active failed: %d, body=%s", rec.Code, rec.Body.String())
	}

	// Now only admin2 is active. Trying to disable admin2 must fail -> 409
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/clients/%d", admin2ID), UpdateClientRequest{
		Disabled: ptr(true),
	}, created.Token)
	if rec.Code != http.StatusConflict {
		t.Fatalf("disable last remaining admin2 status = %d, want 409: body=%s", rec.Code, rec.Body.String())
	}
}

func TestRotateTokenInvalidatesOld(t *testing.T) {
	env := newTestEnv(t)

	// Create a client
	rec := env.req("POST", "/admin/api/clients", CreateClientRequest{
		Name: "client-rotate",
		Role: "user",
	}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client failed: %d", rec.Code)
	}
	var created struct {
		Client ClientView `json:"client"`
		Token  string     `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	clientID := created.Client.ID
	oldToken := created.Token

	oldHash := auth.HashToken(env.keyring.TokenPepper(), oldToken)
	if _, ok := env.index.Lookup(oldHash); !ok {
		t.Fatalf("old token hash should be in index")
	}

	// Rotate token
	rec = env.req("POST", fmt.Sprintf("/admin/api/clients/%d/rotate-token", clientID), nil, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate-token failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var rotated struct {
		Client ClientView `json:"client"`
		Token  string     `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("unmarshal rotated: %v", err)
	}
	newToken := rotated.Token
	if newToken == oldToken {
		t.Fatalf("new token should not equal old token")
	}

	// ADM-12: Old token must be invalidated immediately
	if _, ok := env.index.Lookup(oldHash); ok {
		t.Fatalf("old token hash must NOT be in index after rotate-token")
	}
	newHash := auth.HashToken(env.keyring.TokenPepper(), newToken)
	if p, ok := env.index.Lookup(newHash); !ok || p.ClientID != clientID {
		t.Fatalf("new token hash should be in index, got ok=%v, p=%+v", ok, p)
	}
}

func TestKeysCRUD(t *testing.T) {
	env := newTestEnv(t)

	// POST /admin/api/keys - success
	keySecret := "sk-upstream-secret-key-12345678"
	rec := env.req("POST", "/admin/api/keys", CreateKeyRequest{
		Pool:        "openai-pool",
		Name:        "test-key-1",
		Secret:      keySecret,
		Weight:      ptr(3),
		MaxInFlight: ptr(8),
	}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key KeyView `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal key: %v", err)
	}
	if created.Key.Pool != "openai-pool" || created.Key.Name != "test-key-1" {
		t.Errorf("unexpected key: %+v", created.Key)
	}
	if created.Key.SecretHint != obs.MaskSecret(keySecret) {
		t.Errorf("secret hint = %s, want %s", created.Key.SecretHint, obs.MaskSecret(keySecret))
	}
	keyID := created.Key.ID

	// Validation errors on POST keys
	tests := []struct {
		name string
		req  any
		want int
	}{
		{"unknown pool", CreateKeyRequest{Pool: "unknown-pool", Name: "k", Secret: "sec"}, 400},
		{"empty name", CreateKeyRequest{Pool: "openai-pool", Name: "", Secret: "sec"}, 400},
		{"empty secret", CreateKeyRequest{Pool: "openai-pool", Name: "k", Secret: ""}, 400},
		{"secret too long", CreateKeyRequest{Pool: "openai-pool", Name: "k", Secret: strings.Repeat("s", 4097)}, 400},
		{"weight <= 0", CreateKeyRequest{Pool: "openai-pool", Name: "k", Secret: "sec", Weight: ptr(0)}, 400},
		{"max_in_flight <= 0", CreateKeyRequest{Pool: "openai-pool", Name: "k", Secret: "sec", MaxInFlight: ptr(-1)}, 400},
		{"duplicate secret", CreateKeyRequest{Pool: "openai-pool", Name: "test-key-2", Secret: keySecret}, 409},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := env.req("POST", "/admin/api/keys", tt.req, env.adminTok)
			if r.Code != tt.want {
				t.Fatalf("status = %d, want %d, body=%s", r.Code, tt.want, r.Body.String())
			}
		})
	}

	// GET /admin/api/keys
	rec = env.req("GET", "/admin/api/keys", nil, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys failed: %d", rec.Code)
	}
	var listRes struct {
		Keys []KeyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listRes); err != nil {
		t.Fatalf("unmarshal keys: %v", err)
	}
	if len(listRes.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(listRes.Keys))
	}

	// PATCH /admin/api/keys/{id} - disable key
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/keys/%d", keyID), UpdateKeyRequest{
		State: ptr("disabled"),
	}, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch key disabled failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var patchRes struct {
		Key KeyView `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &patchRes)
	if patchRes.Key.State != "disabled" {
		t.Errorf("key state = %s, want disabled", patchRes.Key.State)
	}

	// PATCH /admin/api/keys/{id} - enable key (ready) clears state_reason
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/keys/%d", keyID), UpdateKeyRequest{
		State: ptr("ready"),
	}, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch key ready failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &patchRes)
	if patchRes.Key.State != "ready" || patchRes.Key.StateReason != "" {
		t.Errorf("key state = %s, reason = %s, want ready and empty reason", patchRes.Key.State, patchRes.Key.StateReason)
	}

	// PATCH invalid state
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/keys/%d", keyID), UpdateKeyRequest{
		State: ptr("quarantined"), // ADM-15: only ready or disabled accepted
	}, env.adminTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch invalid state status = %d, want 400", rec.Code)
	}

	// PATCH non-existent key
	rec = env.req("PATCH", "/admin/api/keys/999999", UpdateKeyRequest{
		Weight: ptr(5),
	}, env.adminTok)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch non-existent key status = %d, want 404", rec.Code)
	}
}

func TestUsage(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Insert some usage events into DB
	events := []store.UsageEvent{
		{
			RequestID:   "req_1",
			ClientID:    env.adminID,
			KeyID:       1,
			Model:       "gpt-4o",
			Op:          "chat",
			Usage:       map[string]int64{"input_tokens": 100, "output_tokens": 50},
			CostNanoUSD: 1500,
			UsageSource: store.UsageSourceUpstream,
			CreatedAt:   (*env.now).Add(-2 * time.Hour),
		},
		{
			RequestID:   "req_2",
			ClientID:    env.adminID,
			KeyID:       1,
			Model:       "gpt-4o",
			Op:          "chat",
			Usage:       map[string]int64{"input_tokens": 200, "output_tokens": 80},
			CostNanoUSD: 2800,
			UsageSource: store.UsageSourceUpstream,
			CreatedAt:   (*env.now).Add(-1 * time.Hour),
		},
	}
	if err := env.store.InsertUsageEvents(ctx, events); err != nil {
		t.Fatalf("insert usage events: %v", err)
	}

	// Default query (last 24 hours)
	rec := env.req("GET", "/admin/api/usage", nil, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage query failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		Items []UsageItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("expected 1 aggregated item, got %d", len(res.Items))
	}
	item := res.Items[0]
	if item.Requests != 2 || item.CostNanoUSD != 4300 {
		t.Errorf("item = %+v, want 2 requests and 4300 cost", item)
	}
	if item.Usage["input_tokens"] != 300 || item.Usage["output_tokens"] != 130 {
		t.Errorf("usage counts = %+v, want 300 input and 130 output", item.Usage)
	}

	// Query with client_id filter
	rec = env.req("GET", fmt.Sprintf("/admin/api/usage?client_id=%d", env.adminID), nil, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage query by client_id failed: %d", rec.Code)
	}

	// Query with from >= to -> 400
	fromStr := (*env.now).Format(time.RFC3339)
	toStr := (*env.now).Add(-time.Hour).Format(time.RFC3339)
	rec = env.req("GET", fmt.Sprintf("/admin/api/usage?from=%s&to=%s", fromStr, toStr), nil, env.adminTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("from >= to status = %d, want 400", rec.Code)
	}

	// Query with to - from > 92 days -> 400
	oldFrom := (*env.now).Add(-95 * 24 * time.Hour).Format(time.RFC3339)
	rec = env.req("GET", fmt.Sprintf("/admin/api/usage?from=%s&to=%s", oldFrom, fromStr), nil, env.adminTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("range > 92 days status = %d, want 400", rec.Code)
	}

	// Invalid from format -> 400
	rec = env.req("GET", "/admin/api/usage?from=not-a-date", nil, env.adminTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid from format status = %d, want 400", rec.Code)
	}

	// Invalid client_id -> 400
	rec = env.req("GET", "/admin/api/usage?client_id=not-a-number", nil, env.adminTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid client_id status = %d, want 400", rec.Code)
	}
}

func TestAuthFailureIPLockout(t *testing.T) {
	env := newTestEnv(t)
	clientIP := "192.168.1.100:54321"

	// Send 20 failed auth requests
	for i := 0; i < 20; i++ {
		rec := env.req("GET", "/admin/api/health", nil, "invalid-token", clientIP)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: expected 401, got %d", i+1, rec.Code)
		}
	}

	// 21st request from the same IP must be locked out with 429
	rec := env.req("GET", "/admin/api/health", nil, "invalid-token", clientIP)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("21st request: expected 429, got %d: %s", rec.Code, rec.Body.String())
	}

	// Even with a VALID token, the locked IP should still get 429
	rec = env.req("GET", "/admin/api/health", nil, env.adminTok, clientIP)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request with valid token from locked IP: expected 429, got %d", rec.Code)
	}

	// A request from a DIFFERENT IP should NOT be locked out
	otherIP := "192.168.1.101:54321"
	rec = env.req("GET", "/admin/api/health", nil, env.adminTok, otherIP)
	if rec.Code != http.StatusOK {
		t.Fatalf("request from different IP: expected 200, got %d", rec.Code)
	}

	// Advance time by 61 seconds: lockout should expire
	*env.now = env.now.Add(61 * time.Second)
	rec = env.req("GET", "/admin/api/health", nil, env.adminTok, clientIP)
	if rec.Code != http.StatusOK {
		t.Fatalf("request after 61s: expected 200, got %d", rec.Code)
	}
}

func TestNoSecretsInResponses(t *testing.T) {
	env := newTestEnv(t)

	// Create key with a distinct secret
	secretKey := "SECRET-SUPER-CONFIDENTIAL-KEY-8888"
	rec := env.req("POST", "/admin/api/keys", CreateKeyRequest{
		Pool:   "openai-pool",
		Name:   "sec-key",
		Secret: secretKey,
	}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key failed: %d, body=%s", rec.Code, rec.Body.String())
	}

	// Create client with a distinct name
	rec = env.req("POST", "/admin/api/clients", CreateClientRequest{
		Name: "sec-client",
		Role: "user",
	}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var createdClient struct {
		Client ClientView `json:"client"`
		Token  string     `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &createdClient)
	clientTokenHash := auth.HashToken(env.keyring.TokenPepper(), createdClient.Token)

	// Endpoints to check
	endpoints := []string{
		"/admin/api/health",
		"/admin/api/clients",
		fmt.Sprintf("/admin/api/clients/%d", createdClient.Client.ID),
		"/admin/api/keys",
		"/admin/api/usage",
	}

	forbiddenStrings := []string{
		secretKey,
		clientTokenHash,
		"wrapped_dek",
		"ciphertext",
	}

	for _, ep := range endpoints {
		rec = env.req("GET", ep, nil, env.adminTok)
		if rec.Code != http.StatusOK {
			t.Fatalf("endpoint %s returned %d: %s", ep, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, forbidden := range forbiddenStrings {
			if strings.Contains(body, forbidden) {
				t.Errorf("endpoint %s leaked %q in response: %s", ep, forbidden, body)
			}
		}
	}
}

func TestAdminAuth(t *testing.T) {
	env := newTestEnv(t)

	// 1. Missing Authorization header -> 401
	rec := env.req("GET", "/admin/api/health", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing auth header: got %d, want 401", rec.Code)
	}

	// 2. Token without uai_live_ prefix -> 401
	rec = env.req("GET", "/admin/api/health", nil, "invalid_prefix_token")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid token prefix: got %d, want 401", rec.Code)
	}

	// 3. User role client -> 403
	rec = env.req("POST", "/admin/api/clients", CreateClientRequest{
		Name: "standard-user",
		Role: "user",
	}, env.adminTok)
	var userClient struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &userClient)

	rec = env.req("GET", "/admin/api/health", nil, userClient.Token)
	if rec.Code != http.StatusForbidden {
		t.Errorf("user role token: got %d, want 403", rec.Code)
	}

	// 4. Disabled admin -> 403
	// Create admin2, then disable admin2
	rec = env.req("POST", "/admin/api/clients", CreateClientRequest{
		Name: "admin-disabled",
		Role: "admin",
	}, env.adminTok)
	var admin2 struct {
		Client ClientView `json:"client"`
		Token  string     `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &admin2)

	// Disable admin2
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/clients/%d", admin2.Client.ID), UpdateClientRequest{
		Disabled: ptr(true),
	}, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed to disable admin2: %d", rec.Code)
	}

	// Try calling health with disabled admin2 token -> 403
	rec = env.req("GET", "/admin/api/health", nil, admin2.Token)
	if rec.Code != http.StatusForbidden {
		t.Errorf("disabled admin token: got %d, want 403", rec.Code)
	}
}

func TestNotFoundRoute(t *testing.T) {
	env := newTestEnv(t)
	rec := env.req("GET", "/admin/api/nonexistent", nil, env.adminTok)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPatchKeyMeta(t *testing.T) {
	env := newTestEnv(t)
	rec := env.req("POST", "/admin/api/keys", CreateKeyRequest{
		Pool:        "openai-pool",
		Name:        "key-to-patch",
		Secret:      "sk-secret-patch-meta-12345",
		Weight:      ptr(1),
		MaxInFlight: ptr(5),
	}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key failed: %d", rec.Code)
	}
	var created struct {
		Key KeyView `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	keyID := created.Key.ID

	newName := "key-patched"
	newWeight := 10
	newMax := 20
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/keys/%d", keyID), UpdateKeyRequest{
		Name:        &newName,
		Weight:      &newWeight,
		MaxInFlight: &newMax,
	}, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch key meta failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	var patched struct {
		Key KeyView `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.Key.Name != "key-patched" || patched.Key.Weight != 10 || patched.Key.MaxInFlight != 20 {
		t.Errorf("unexpected patched key: %+v", patched.Key)
	}
}

func ptr[T any](v T) *T {
	return &v
}

func TestMetricsEndpoint(t *testing.T) {
	env := newTestEnv(t)

	// 1. Without metrics handler configured: GET -> 404, POST -> 405
	rec := env.req("GET", "/admin/metrics", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /admin/metrics without handler configured: expected 404, got %d", rec.Code)
	}
	rec = env.req("POST", "/admin/metrics", nil, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /admin/metrics without handler configured: expected 405, got %d", rec.Code)
	}

	// 2. With metrics handler configured:
	mockMetrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("uaig_test 1\n"))
	})

	deps := Deps{
		Config:    env.cfg,
		Store:     env.store,
		Keyring:   env.keyring,
		Index:     env.index,
		Reloader:  env.reloader,
		Ledger:    env.ledger,
		Scheduler: env.scheduler,
		Logger:    obs.Discard(),
		Version:   "1.0.0-test",
		StartedAt: *env.now,
		Now: func() time.Time {
			return *env.now
		},
		Metrics: mockMetrics,
	}
	srv := NewServer(deps)

	// GET /admin/metrics without Authorization header (MTR-02)
	req := httptest.NewRequest("GET", "/admin/metrics", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/metrics without auth: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("GET /admin/metrics Content-Type: expected %q, got %q", "text/plain; version=0.0.4; charset=utf-8", ct)
	}
	if !strings.Contains(w.Body.String(), "uaig_test 1") {
		t.Fatalf("expected uaig_test 1 in body, got: %s", w.Body.String())
	}

	// POST /admin/metrics (MTR-01): non-GET must return 405 with Allow: GET
	reqPost := httptest.NewRequest("POST", "/admin/metrics", nil)
	wPost := httptest.NewRecorder()
	srv.ServeHTTP(wPost, reqPost)

	if wPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /admin/metrics: expected 405, got %d", wPost.Code)
	}
	if allow := wPost.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow header: expected GET, got %q", allow)
	}
}
