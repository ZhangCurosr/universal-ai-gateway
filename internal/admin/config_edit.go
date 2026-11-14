package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// 网页端管理池与服务（T72，spec/09 §9，PE-01 ~ PE-07）。

// ProtocolInfo 是 PE-01 中一个已注册的上游协议：计量单位与常用上游地址。
type ProtocolInfo struct {
	Name           string   `json:"name"`
	Units          []string `json:"units"`
	DefaultBaseURL string   `json:"default_base_url"`
}

// ConfigEditDeps 由装配代码注入：读取配置文件、校验并写入后重载、列出协议。
type ConfigEditDeps struct {
	// Load 返回配置文件的内容与能否写入。
	Load func() (data []byte, writable bool, err error)
	// Apply 在锁内：读取文件 → 比较版本（不一致 409）→ edit → 解析与构造检查（失败 400）→ check 时返回；
	// 否则备份、写入并重载，重载失败恢复原文件（PE-02 ~ PE-05）。返回值同重载 API。
	Apply func(ctx context.Context, actor int64, version string, edit func([]byte) ([]byte, error), check bool, what string) (int, any)
	// Protocols 列出已注册的协议。
	Protocols func() []ProtocolInfo
}

// ConfigVersion 是配置文件内容的 sha256（PE-02 的 version）。
func ConfigVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// EntryView 是 PE-01 中的一项。
type EntryView struct {
	Name     string             `json:"name"`
	YAML     string             `json:"yaml"`
	Fields   config.EntryFields `json:"fields"`
	Pool     string             `json:"pool,omitempty"`     // 服务所属的池
	Protocol string             `json:"protocol,omitempty"` // 池的协议
	Services int                `json:"services"`           // 池下的服务数
	Keys     int                `json:"keys"`               // 池里的 Key 数
}

// EntriesView 是 GET /admin/api/config/entries 的响应（PE-01）。
type EntriesView struct {
	Version   string         `json:"version"`
	Writable  bool           `json:"writable"`
	Pools     []EntryView    `json:"pools"`
	Models    []EntryView    `json:"models"`
	Protocols []ProtocolInfo `json:"protocols"`
}

func (s *server) configEdit() (*ConfigEditDeps, error) {
	ce := s.d.ConfigEdit
	if ce == nil || ce.Load == nil || ce.Apply == nil {
		return nil, apperr.New(apperr.CodeNotFound, "editing the config file is not available (the gateway was not started with a config file)")
	}
	return ce, nil
}

// handleConfigEntries 实现 PE-01。
func (s *server) handleConfigEntries(w http.ResponseWriter, _ *http.Request) {
	ce, err := s.configEdit()
	if err != nil {
		writeError(w, err)
		return
	}
	data, writable, err := ce.Load()
	if err != nil {
		writeError(w, apperr.Wrap(apperr.CodeInternal, "could not read the config file", err))
		return
	}
	v := EntriesView{Version: ConfigVersion(data), Writable: writable, Pools: []EntryView{}, Models: []EntryView{}, Protocols: []ProtocolInfo{}}
	if ce.Protocols != nil {
		v.Protocols = ce.Protocols()
	}
	keys := map[string]int{}
	if s.d.Scheduler != nil {
		for _, k := range s.d.Scheduler.KeyStatus() {
			keys[k.Pool]++
		}
	}
	models, err := config.ListItems(data, "models")
	if err != nil {
		writeError(w, apperr.New(apperr.CodeConflict, err.Error()))
		return
	}
	services := map[string]int{}
	for _, it := range models {
		f, _ := config.ReadEntryFields("models", it.Text) // 文件已经通过校验，解析不会失败
		pool, _ := f["pool"].(string)
		services[pool]++
		v.Models = append(v.Models, EntryView{Name: it.Name, YAML: it.Text, Fields: f, Pool: pool})
	}
	pools, err := config.ListItems(data, "pools")
	if err != nil {
		writeError(w, apperr.New(apperr.CodeConflict, err.Error()))
		return
	}
	for _, it := range pools {
		f, _ := config.ReadEntryFields("pools", it.Text) // 同上
		proto, _ := f["protocol"].(string)
		v.Pools = append(v.Pools, EntryView{Name: it.Name, YAML: it.Text, Fields: f, Protocol: proto, Services: services[it.Name], Keys: keys[it.Name]})
	}
	sort.Slice(v.Pools, func(i, j int) bool { return v.Pools[i].Name < v.Pools[j].Name })
	sort.Slice(v.Models, func(i, j int) bool { return v.Models[i].Name < v.Models[j].Name })
	writeJSON(w, http.StatusOK, v)
}

// handleConfigCompose：表单与 YAML 标签页的同步（PE-02 页面）。请求 {kind, yaml, fields?}，
// 有 fields 时写进 yaml；返回 {yaml, fields}。只处理文本，不读写配置文件。
func (s *server) handleConfigCompose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind   string             `json:"kind"`
		YAML   string             `json:"yaml"`
		Fields config.EntryFields `json:"fields"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	text := req.YAML
	if req.Fields != nil {
		out, err := config.ApplyEntryFields(req.Kind, text, req.Fields)
		if err != nil {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, err.Error()))
			return
		}
		text = out
	}
	f, err := config.ReadEntryFields(req.Kind, text)
	if err != nil {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yaml": text, "fields": f})
}

// handleConfigEntry 实现 PE-02 ~ PE-05 的增删改：kind 为 pools 或 models。
func (s *server) handleConfigEntry(kind, op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ce, err := s.configEdit()
		if err != nil {
			writeError(w, err)
			return
		}
		var req struct {
			Version string `json:"version"`
			YAML    string `json:"yaml"`
			Check   bool   `json:"check"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, err)
			return
		}
		if req.Version == "" {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "version is required (from GET /admin/api/config/entries)"))
			return
		}
		name := r.PathValue("name")
		single := strings.TrimSuffix(kind, "s")
		if kind == "models" {
			single = "service"
		}
		var newName string
		if op != "delete" {
			n, err := config.EntryName(req.YAML)
			if err != nil {
				writeError(w, apperr.New(apperr.CodeInvalidRequest, err.Error()))
				return
			}
			newName = n
			if op == "update" && n != name {
				writeError(w, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("a %s cannot be renamed (%q → %q): delete it and create a new one", single, name, n)))
				return
			}
		}
		var warnings []string
		edit := func(data []byte) ([]byte, error) {
			items, err := config.ListItems(data, kind)
			if err != nil {
				return nil, err
			}
			exists := map[string]bool{}
			for _, it := range items {
				exists[it.Name] = true
			}
			switch op {
			case "create":
				if exists[newName] {
					return nil, fmt.Errorf("%s %q already exists", single, newName)
				}
				return config.AppendListItem(data, kind, req.YAML)
			case "update":
				return config.ReplaceListItem(data, kind, name, req.YAML)
			default:
				if kind == "pools" { // PE-05：池下还有服务不能删
					models, err := config.ListItems(data, "models")
					if err != nil {
						return nil, err
					}
					var used []string
					for _, it := range models {
						f, _ := config.ReadEntryFields("models", it.Text) // 文件已经通过校验
						if f["pool"] == name {
							used = append(used, it.Name)
						}
					}
					if len(used) > 0 {
						return nil, fmt.Errorf("pool %q still has services (%s); delete them first", name, strings.Join(used, ", "))
					}
				}
				return config.RemoveListItem(data, kind, name)
			}
		}
		if op == "delete" && kind == "models" {
			warnings = s.clientsOnlyAllowing(r.Context(), name)
		}
		var actor int64
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			actor = p.ClientID
		}
		target := name
		if target == "" {
			target = newName
		}
		status, body := ce.Apply(r.Context(), actor, req.Version, edit, req.Check, kind+" "+op+" "+target)
		if m, ok := body.(map[string]any); ok && len(warnings) > 0 && status < http.StatusBadRequest {
			m["warnings"] = warnings
		}
		writeJSON(w, status, body)
	}
}

// clientsOnlyAllowing 返回 allowed_models 中只有 model 这一项的客户端（PE-05 的警告）。
func (s *server) clientsOnlyAllowing(ctx context.Context, model string) []string {
	if s.d.Store == nil {
		return nil
	}
	clients, err := s.d.Store.ListClients(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range clients {
		if len(c.AllowedModels) == 1 && c.AllowedModels[0] == model {
			out = append(out, fmt.Sprintf("client %q can only use %s; it will not be able to call anything", c.Name, model))
		}
	}
	return out
}
