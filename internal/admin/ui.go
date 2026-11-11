package admin

import (
	"embed"
	"io/fs"
	"net"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
)

// uiFiles 是管理 Web UI 的静态资源（spec/09 §6）。只读，随二进制分发。
//
//go:embed ui
var uiFiles embed.FS

const (
	uiRoot   = "/admin/ui"
	uiPrefix = "/admin/ui/"

	// uiCSP 是 UI-03 规定的内容安全策略（逐字一致，测试会比较）。
	uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// uiContentTypes 是 UI-04 允许的扩展名及其 Content-Type；其他扩展名一律 404。
var uiContentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
}

// serveUI 处理 /admin/ui 与 /admin/ui/*（UI-01 ~ UI-04）。不鉴权（UI-02）。
func serveUI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		uiText(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path == uiRoot {
		http.Redirect(w, r, uiPrefix, http.StatusMovedPermanently)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, uiPrefix)
	if name == "" {
		name = "index.html"
	}
	ct, ok := uiContentTypes[path.Ext(name)]
	if !ok || !fs.ValidPath(name) {
		uiText(w, http.StatusNotFound, "not found")
		return
	}
	b, err := fs.ReadFile(uiFiles, "ui/"+name)
	if err != nil {
		uiText(w, http.StatusNotFound, "not found")
		return
	}

	if name == "index.html" {
		h.Set("Cache-Control", "no-store")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b) // 客户端断开时写失败无需处理
}

func uiText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n")) // 同上
}

// allowedHostSet 返回 UI-20 允许的主机名集合：本机地址加上配置的 admin_hosts（小写、去方括号）。
func allowedHostSet(extra []string) map[string]bool {
	set := map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}
	for _, h := range extra {
		set[normalizeHost(h)] = true
	}
	return set
}

func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))
}

// hostAllowed 判断请求的 Host 头是否允许（UI-20）。Host 可能带端口，也可能是带方括号的 IPv6。
func (s *server) hostAllowed(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	return s.net.Load().hosts[normalizeHost(host)]
}

// ConfigView 是 GET /admin/api/config 的响应（spec/09 §6.4）：只含池与模型的名称及归属。
type ConfigView struct {
	Pools  []ConfigPool  `json:"pools"`
	Models []ConfigModel `json:"models"`
}

// ConfigPool 是配置中的一个池。不含 base_url 等其他字段。
type ConfigPool struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

// ConfigModel 是配置中的一个模型（服务）。不含价格等其他字段。
type ConfigModel struct {
	Name string `json:"name"`
	Pool string `json:"pool"`
}

func (s *server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	out := ConfigView{Pools: []ConfigPool{}, Models: []ConfigModel{}}
	if c := s.config(); c != nil {
		for _, p := range c.Pools {
			out.Pools = append(out.Pools, ConfigPool{Name: p.Name, Protocol: p.Protocol})
		}
		for _, m := range c.Models {
			out.Models = append(out.Models, ConfigModel{Name: m.Name, Pool: m.Pool})
		}
	}
	sort.Slice(out.Pools, func(i, j int) bool { return out.Pools[i].Name < out.Pools[j].Name })
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Name < out.Models[j].Name })
	writeJSON(w, http.StatusOK, out)
}
