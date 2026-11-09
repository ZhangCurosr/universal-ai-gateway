package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// 允许列表：创建与更新都校验模型已配置，并去掉重复项（保持首次出现的顺序）。
func TestAllowedModelsNormalized(t *testing.T) {
	env := newTestEnv(t)
	rec := env.req("POST", "/admin/api/clients", CreateClientRequest{
		Name: "dup", Role: "user", AllowedModels: []string{"gpt-4o", "claude-3-5-sonnet", "gpt-4o"},
	}, env.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Client ClientView `json:"client"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if want := []string{"gpt-4o", "claude-3-5-sonnet"}; !slices.Equal(created.Client.AllowedModels, want) {
		t.Fatalf("created allowed_models = %v, want %v", created.Client.AllowedModels, want)
	}

	path := fmt.Sprintf("/admin/api/clients/%d", created.Client.ID)
	rec = env.req("PATCH", path, UpdateClientRequest{AllowedModels: &[]string{"claude-3-5-sonnet", "claude-3-5-sonnet"}}, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Client ClientView `json:"client"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude-3-5-sonnet"}; !slices.Equal(updated.Client.AllowedModels, want) {
		t.Fatalf("updated allowed_models = %v, want %v", updated.Client.AllowedModels, want)
	}

	if rec = env.req("PATCH", path, UpdateClientRequest{AllowedModels: &[]string{"nope"}}, env.adminTok); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch unknown model: %d, want 400", rec.Code)
	}
	// 空列表 = 不限制
	rec = env.req("PATCH", path, UpdateClientRequest{AllowedModels: &[]string{}}, env.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch empty: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil || len(updated.Client.AllowedModels) != 0 {
		t.Fatalf("empty allowed_models = %v (err %v)", updated.Client.AllowedModels, err)
	}
}
