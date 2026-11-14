package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// countingStore 统计 ListUpstreamKeys 的调用次数（LoadKeys 也经过它）。
type countingStore struct {
	store.Store
	mu    sync.Mutex
	lists int
}

func (c *countingStore) ListUpstreamKeys(ctx context.Context) ([]store.UpstreamKey, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.Store.ListUpstreamKeys(ctx)
}

// importServer 用计数的存储与写入缓冲区的日志重新装配一个管理面。
func importServer(env *testEnv) (http.Handler, *countingStore, *bytes.Buffer) {
	cs := &countingStore{Store: env.store}
	var logs bytes.Buffer
	h := NewServer(Deps{
		Config: env.cfg, Store: cs, Keyring: env.keyring, Index: env.index, Reloader: env.reloader,
		Ledger: env.ledger, Scheduler: env.scheduler, Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		Now: func() time.Time { return *env.now },
	})
	return h, cs, &logs
}

func doImport(t *testing.T, h http.Handler, token string, body any) (int, ImportKeysResponse, string) {
	t.Helper()
	var raw []byte
	if s, ok := body.(string); ok {
		raw = []byte(s)
	} else {
		raw, _ = json.Marshal(body) // 测试数据，必然可以序列化
	}
	r := httptest.NewRequest("POST", "/admin/api/keys/import", bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var resp ImportKeysResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v %s", err, rec.Body.String())
		}
	}
	return rec.Code, resp, rec.Body.String()
}

func strp(s string) *string { return &s }

// ADM-23、ADM-24：混合的新建、无效、重复（含同一批内重复），名称编号接续，整批只重载一次，响应与日志不含密钥
func TestImportKeys(t *testing.T) {
	env := newTestEnv(t)
	h, cs, logs := importServer(env)
	// 已有一把同前缀的 Key：编号要接在 007 之后
	rec := env.req("POST", "/admin/api/keys", CreateKeyRequest{Pool: "openai-pool", Name: "batch-007", Secret: "sk-existing-key-0000001"}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	future := env.now.Add(48 * time.Hour).UTC().Format(time.RFC3339)
	secrets := []string{"sk-import-secret-aaaa01", "sk-import-secret-bbbb02", "", "sk-existing-key-0000001", "sk-import-secret-aaaa01", "sk-import-secret-cccc03"}
	items := []ImportKeyItem{
		{Secret: secrets[0], ExpiresAt: strp(future), Label: strp("web-2026-11")},
		{Secret: secrets[1]},
		{Secret: secrets[2]},
		{Secret: secrets[3]},
		{Secret: secrets[4]},
		{Secret: secrets[5], ExpiresAt: strp("2026-12-01")},
	}
	cs.lists = 0
	code, resp, body := doImport(t, h, env.adminTok, ImportKeysRequest{Pool: "openai-pool", NamePrefix: "batch", Items: items})
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, body)
	}
	if resp.Created != 2 || resp.Duplicate != 2 || resp.Invalid != 2 || resp.Updated != 0 || resp.Failed != 0 || len(resp.Results) != 6 {
		t.Fatalf("counts = %+v", resp)
	}
	want := []string{ImportCreated, ImportCreated, ImportInvalid, ImportDuplicate, ImportDuplicate, ImportInvalid}
	for i, r := range resp.Results {
		if r.Index != i || r.Status != want[i] {
			t.Fatalf("result %d = %+v, want %s", i, r, want[i])
		}
	}
	if resp.Results[0].Name != "batch-008" || resp.Results[1].Name != "batch-009" {
		t.Fatalf("names = %q %q", resp.Results[0].Name, resp.Results[1].Name)
	}
	if resp.Results[3].KeyID == 0 || resp.Results[4].KeyID != resp.Results[0].KeyID {
		t.Fatalf("duplicates must carry the existing id: %+v", resp.Results)
	}
	if !strings.Contains(resp.Results[5].Error, "RFC 3339") || resp.Results[2].SecretHint != "" {
		t.Fatalf("invalid results = %+v %+v", resp.Results[2], resp.Results[5])
	}
	// 只有处理前的一次列出 + 处理后的一次重载
	if cs.lists != 2 {
		t.Fatalf("ListUpstreamKeys called %d times, want 2 (one reload for the whole batch)", cs.lists)
	}
	v := findKeyView(t, env, resp.Results[0].KeyID)
	if v.Label != "web-2026-11" || v.ExpiresAt != future || v.Pool != "openai-pool" || v.Weight != 1 || v.MaxInFlight != 16 {
		t.Fatalf("created key = %+v", v)
	}
	// 新 Key 已经进入调度器
	found := 0
	for _, st := range env.scheduler.KeyStatus() {
		if st.ID == resp.Results[0].KeyID || st.ID == resp.Results[1].KeyID {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("scheduler has %d of the 2 new keys", found)
	}
	for _, s := range secrets {
		if s != "" && (strings.Contains(body, s) || strings.Contains(logs.String(), s)) {
			t.Fatalf("secret leaked into the response or logs")
		}
	}
	if !strings.Contains(logs.String(), `"msg":"admin: upstream keys imported"`) || !strings.Contains(logs.String(), `"created":2`) {
		t.Fatalf("import log missing: %s", logs.String())
	}

	// 全部重复：没有变化，不重载
	cs.lists = 0
	code, resp, _ = doImport(t, h, env.adminTok, ImportKeysRequest{Pool: "openai-pool", Items: items[:2]})
	if code != http.StatusOK || resp.Duplicate != 2 || cs.lists != 1 {
		t.Fatalf("all duplicates: %d %+v lists %d", code, resp, cs.lists)
	}
}

// ADM-23：update_existing 写入出现的字段；空的 expires_at 清除过期时间；没有字段可写时仍计为重复
func TestImportKeysUpdateExisting(t *testing.T) {
	env := newTestEnv(t)
	h, _, _ := importServer(env)
	future := env.now.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	code, resp, body := doImport(t, h, env.adminTok, ImportKeysRequest{Pool: "openai-pool", Items: []ImportKeyItem{
		{Secret: "sk-upd-secret-000000001", ExpiresAt: strp(future), Label: strp("old")},
		{Secret: "sk-upd-secret-000000002", Label: strp("keep")},
	}})
	if code != http.StatusOK || resp.Created != 2 {
		t.Fatalf("seed: %d %s", code, body)
	}
	if resp.Results[0].Name != "openai-pool-001" {
		t.Fatalf("default prefix name = %q", resp.Results[0].Name)
	}
	id1, id2 := resp.Results[0].KeyID, resp.Results[1].KeyID
	later := env.now.Add(72 * time.Hour).UTC().Format(time.RFC3339)
	code, resp, body = doImport(t, h, env.adminTok, ImportKeysRequest{Pool: "openai-pool", UpdateExisting: true, Items: []ImportKeyItem{
		{Secret: "sk-upd-secret-000000001", ExpiresAt: strp(""), Label: strp("new")},
		{Secret: "sk-upd-secret-000000002"},
		{Secret: "sk-upd-secret-000000003", ExpiresAt: strp(later)},
	}})
	if code != http.StatusOK || resp.Updated != 1 || resp.Duplicate != 1 || resp.Created != 1 {
		t.Fatalf("update: %d %s", code, body)
	}
	if v := findKeyView(t, env, id1); v.ExpiresAt != "" || v.Label != "new" {
		t.Fatalf("updated key = %+v", v)
	}
	if v := findKeyView(t, env, id2); v.Label != "keep" {
		t.Fatalf("untouched key = %+v", v)
	}
	if resp.Results[2].Name != "openai-pool-003" {
		t.Fatalf("next name = %q", resp.Results[2].Name)
	}
}

// ADM-23：整体校验失败返回 400
func TestImportKeysRejects(t *testing.T) {
	env := newTestEnv(t)
	h, _, _ := importServer(env)
	many := make([]ImportKeyItem, maxImportItems+1)
	for i := range many {
		many[i] = ImportKeyItem{Secret: fmt.Sprintf("sk-many-%06d", i)}
	}
	one := []ImportKeyItem{{Secret: "sk-reject-000000001"}}
	w0 := 0
	tests := []struct {
		name string
		body any
	}{
		{"no items", ImportKeysRequest{Pool: "openai-pool"}},
		{"too many items", ImportKeysRequest{Pool: "openai-pool", Items: many}},
		{"unknown pool", ImportKeysRequest{Pool: "nope", Items: one}},
		{"long prefix", ImportKeysRequest{Pool: "openai-pool", NamePrefix: strings.Repeat("p", maxImportNamePrefix+1), Items: one}},
		{"bad weight", ImportKeysRequest{Pool: "openai-pool", Weight: &w0, Items: one}},
		{"bad max_in_flight", ImportKeysRequest{Pool: "openai-pool", MaxInFlight: &w0, Items: one}},
		{"unknown field", `{"pool":"openai-pool","items":[{"secret":"sk-reject-000000001","name":"x"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, _, body := doImport(t, h, env.adminTok, tt.body); code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", code, body)
			}
		})
	}
	if code, _, _ := doImport(t, h, "", ImportKeysRequest{Pool: "openai-pool", Items: one}); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
}

func TestNameSeq(t *testing.T) {
	for _, tt := range []struct {
		name string
		n    int
		ok   bool
	}{{"p-001", 1, true}, {"p-120", 120, true}, {"p-", 0, false}, {"p-1a", 0, false}, {"q-001", 0, false}, {"p-x-001", 0, false}} {
		if n, ok := nameSeq(tt.name, "p"); n != tt.n || ok != tt.ok {
			t.Errorf("nameSeq(%q) = %d %v", tt.name, n, ok)
		}
	}
}
