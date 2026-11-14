package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func findKeyView(t *testing.T, env *testEnv, id int64) KeyView {
	t.Helper()
	rec := env.req("GET", "/admin/api/keys", nil, env.adminTok)
	var list struct {
		Keys []KeyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, k := range list.Keys {
		if k.ID == id {
			return k
		}
	}
	t.Fatalf("key %d not listed", id)
	return KeyView{}
}

// T62：过期时间与标签的创建、展示、延长、清除与校验
func TestKeyExpiryAndLabel(t *testing.T) {
	env := newTestEnv(t)
	past := env.now.Add(-time.Hour).UTC().Truncate(time.Second)
	rec := env.req("POST", "/admin/api/keys", CreateKeyRequest{
		Pool: "openai-pool", Name: "exp", Secret: "sk-secret-expiry-1234567",
		ExpiresAt: past.In(time.FixedZone("", 8*3600)).Format(time.RFC3339), Label: "signup-2026-11",
	}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key KeyView `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created) // 解析失败时下面的断言会报告
	id := created.Key.ID
	v := findKeyView(t, env, id)
	if v.State != KeyStateExpired || v.StoredState != "ready" || v.ExpiresAt != past.Format(time.RFC3339) || v.Label != "signup-2026-11" {
		t.Fatalf("expired key view = %+v", v)
	}

	future := env.now.Add(24 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/keys/%d", id), fmt.Sprintf(`{"expires_at":%q}`, future), env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("extend: %d %s", rec.Code, rec.Body.String())
	}
	if v := findKeyView(t, env, id); v.State != "ready" || v.StoredState != "" || v.ExpiresAt != future || v.Label != "signup-2026-11" {
		t.Fatalf("after extending = %+v", v)
	}

	rec = env.req("PATCH", fmt.Sprintf("/admin/api/keys/%d", id), `{"expires_at":null,"label":""}`, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if v := findKeyView(t, env, id); v.ExpiresAt != "" || v.Label != "" || v.State != "ready" {
		t.Fatalf("after clearing = %+v", v)
	}
	if !strings.Contains(rec.Body.String(), `"label":""`) || strings.Contains(rec.Body.String(), "expires_at") {
		t.Fatalf("view JSON = %s", rec.Body.String())
	}

	// 禁用的过期 Key 显示 disabled，不显示 expired
	rec = env.req("PATCH", fmt.Sprintf("/admin/api/keys/%d", id), fmt.Sprintf(`{"expires_at":%q,"state":"disabled"}`, past.Format(time.RFC3339)), env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if v := findKeyView(t, env, id); v.State != "disabled" {
		t.Fatalf("disabled expired key = %+v", v)
	}

	for _, tc := range []struct{ name, method, path, body string }{
		{"create bad time", "POST", "/admin/api/keys", `{"pool":"openai-pool","name":"x","secret":"sk-x-123456789","expires_at":"2026-12-01"}`},
		{"create long label", "POST", "/admin/api/keys", `{"pool":"openai-pool","name":"x","secret":"sk-x-123456789","label":"` + strings.Repeat("a", 65) + `"}`},
		{"patch bad time", "PATCH", fmt.Sprintf("/admin/api/keys/%d", id), `{"expires_at":"tomorrow"}`},
		{"patch number", "PATCH", fmt.Sprintf("/admin/api/keys/%d", id), `{"expires_at":1767225600}`},
		{"patch epoch", "PATCH", fmt.Sprintf("/admin/api/keys/%d", id), `{"expires_at":"1970-01-01T00:00:00Z"}`},
	} {
		if rec := env.req(tc.method, tc.path, tc.body, env.adminTok); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// T62：重复的 Key 返回 409 并带上已存在 Key 的 id，响应不含密钥
func TestKeyConflictReturnsID(t *testing.T) {
	env := newTestEnv(t)
	body := CreateKeyRequest{Pool: "openai-pool", Name: "dup", Secret: "sk-secret-dup-987654321"}
	rec := env.req("POST", "/admin/api/keys", body, env.adminTok)
	var created struct {
		Key KeyView `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created) // 解析失败时下面的断言会报告
	rec = env.req("POST", "/admin/api/keys", body, env.adminTok)
	var e struct {
		Error struct {
			Code  string `json:"code"`
			KeyID int64  `json:"key_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict || e.Error.KeyID != created.Key.ID || created.Key.ID == 0 {
		t.Fatalf("duplicate: %d %s (want key_id %d)", rec.Code, rec.Body.String(), created.Key.ID)
	}
	if strings.Contains(rec.Body.String(), body.Secret) {
		t.Fatal("conflict response leaks the secret")
	}
}

// ADM-27：停用原因只能与 state=disabled 一起使用；写入后显示在 state_reason；恢复时清除
func TestKeyDisableReason(t *testing.T) {
	env := newTestEnv(t)
	rec := env.req("POST", "/admin/api/keys", CreateKeyRequest{Pool: "openai-pool", Name: "r", Secret: "sk-reason-key-0000001"}, env.adminTok)
	var created struct {
		Key KeyView `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created) // 解析失败时下面的断言会报告
	path := fmt.Sprintf("/admin/api/keys/%d", created.Key.ID)
	for _, body := range []string{`{"reason":"x"}`, `{"state":"ready","reason":"x"}`, `{"state":"disabled","reason":"  "}`,
		`{"state":"disabled","reason":"` + strings.Repeat("r", 201) + `"}`} {
		if rec := env.req("PATCH", path, body, env.adminTok); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want 400", body, rec.Code)
		}
	}
	if rec := env.req("PATCH", path, `{"state":"disabled","reason":"sync: not in source"}`, env.adminTok); rec.Code != http.StatusOK {
		t.Fatalf("disable with reason: %d %s", rec.Code, rec.Body.String())
	}
	if v := findKeyView(t, env, created.Key.ID); v.State != "disabled" || v.StateReason != "sync: not in source" {
		t.Fatalf("after disable = %+v", v)
	}
	env.req("PATCH", path, `{"state":"ready"}`, env.adminTok)
	env.req("PATCH", path, `{"state":"disabled"}`, env.adminTok)
	if v := findKeyView(t, env, created.Key.ID); v.StateReason != "disabled by admin" {
		t.Fatalf("default reason = %q", v.StateReason)
	}
}
