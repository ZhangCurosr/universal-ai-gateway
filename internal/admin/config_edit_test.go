package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// PE-01 ~ PE-03：没有注入时 404；读取、改名、缺少 version 的校验；compose 的错误。写入与重载由 cmd/gateway 的端到端测试覆盖。
func TestConfigEntriesAPI(t *testing.T) {
	plain := newTestEnv(t)
	if rec := plain.req("GET", "/admin/api/config/entries", nil, plain.adminTok); rec.Code != http.StatusNotFound {
		t.Fatalf("not available: %d", rec.Code)
	}
	file := "server:\n  data_dir: x\npools:\n  - name: openai-pool\n    protocol: openai\n    base_url: \"https://api.openai.com\"\nmodels:\n  - name: gpt-4o\n    pool: openai-pool\n    price: { input_tokens: \"1/1M\" }\n"
	var applied []string
	env := newTestEnvWith(t, func(d *Deps) {
		d.ConfigEdit = &ConfigEditDeps{
			Load: func() ([]byte, bool, error) { return []byte(file), true, nil },
			Apply: func(_ context.Context, _ int64, version string, edit func([]byte) ([]byte, error), check bool, what string) (int, any) {
				if version != ConfigVersion([]byte(file)) {
					return http.StatusConflict, map[string]any{"error": map[string]any{"code": "conflict"}}
				}
				out, err := edit([]byte(file))
				if err != nil {
					return http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}}
				}
				applied = append(applied, what)
				return http.StatusOK, map[string]any{"version": ConfigVersion(out), "file": string(out)}
			},
			Protocols: func() []ProtocolInfo { return []ProtocolInfo{{Name: "openai_chat", Units: []string{"input_tokens"}}} },
		}
	})
	rec := env.req("GET", "/admin/api/config/entries", nil, env.adminTok)
	var v EntriesView
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("entries: %d %s", rec.Code, rec.Body)
	}
	if v.Version != ConfigVersion([]byte(file)) || !v.Writable || len(v.Pools) != 1 || v.Pools[0].Services != 1 || v.Pools[0].Protocol != "openai" ||
		len(v.Models) != 1 || v.Models[0].Pool != "openai-pool" || len(v.Protocols) != 1 {
		t.Fatalf("view = %+v", v)
	}
	post := func(method, path, body string) (int, string) {
		r := env.req(method, path, body, env.adminTok)
		return r.Code, r.Body.String()
	}
	if code, body := post("PUT", "/admin/api/config/pools/openai-pool", `{"version":"`+v.Version+`","yaml":"name: other\nprotocol: openai\n"}`); code != 400 || !strings.Contains(body, "renamed") {
		t.Fatalf("rename: %d %s", code, body)
	}
	if code, body := post("POST", "/admin/api/config/pools", `{"yaml":"name: p\n"}`); code != 400 || !strings.Contains(body, "version") {
		t.Fatalf("no version: %d %s", code, body)
	}
	if code, body := post("DELETE", "/admin/api/config/pools/openai-pool", `{"version":"`+v.Version+`"}`); code != 400 || !strings.Contains(body, "still has services") {
		t.Fatalf("delete pool with services: %d %s", code, body)
	}
	if code, body := post("POST", "/admin/api/config/models", `{"version":"`+v.Version+`","yaml":"name: gpt-4o\npool: openai-pool\n"}`); code != 400 || !strings.Contains(body, "already exists") {
		t.Fatalf("duplicate: %d %s", code, body)
	}
	if code, body := post("DELETE", "/admin/api/config/models/gpt-4o", `{"version":"`+v.Version+`"}`); code != 200 || !strings.Contains(body, "models delete gpt-4o") && len(applied) != 1 {
		t.Fatalf("delete service: %d %s %v", code, body, applied)
	}
	if code, _ := post("POST", "/admin/api/config/compose", `{"kind":"x","yaml":""}`); code != 400 {
		t.Fatalf("compose unknown kind: %d", code)
	}
	if code, body := post("POST", "/admin/api/config/compose", `{"kind":"pools","yaml":"","fields":{"name":"p","protocol":"rest"}}`); code != 200 || !strings.Contains(body, `"protocol":"rest"`) {
		t.Fatalf("compose: %d %s", code, body)
	}
}
