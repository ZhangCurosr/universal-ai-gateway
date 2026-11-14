package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// T64：启动后的第一次评估就发现没有可用 Key 的池，推送到 Webhook（飞书格式），
// 并出现在管理 API、管理面 MCP 与指标中；Webhook 地址不出现在日志里。
func TestE2E_PoolAlerts(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.URL.Path+" "+string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	urlFile := filepath.Join(t.TempDir(), "hook.url")
	if err := os.WriteFile(urlFile, []byte(hook.URL+"/bot/token-abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := startRESTGateway(t, func(c *config.Config) {
		c.Alerts.Webhook = &config.WebhookConfig{URLFile: urlFile, Format: config.WebhookFeishu}
		c.Pools = append(c.Pools, config.PoolConfig{Name: "al", Protocol: "rest", BaseURL: "http://127.0.0.1:1",
			Alerts: &config.PoolAlerts{MinAvailableKeys: 2}})
		c.Models = append(c.Models, config.ModelConfig{Name: "al-svc", Pool: "al", Price: map[string]string{"requests": "0"}})
		c.Normalize()
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	got := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.HasPrefix(got, "/bot/token-abc123 ") || !strings.Contains(got, `"msg_type":"text"`) || !strings.Contains(got, "[critical] al: pool al has 0 available keys (minimum 2)") {
		t.Fatalf("webhook got %q", got)
	}

	var view struct {
		Alerts []struct {
			Pool, Kind, Level string
		} `json:"alerts"`
		CheckedAt string `json:"checked_at"`
	}
	doJSON(t, "GET", "http://"+g.admin+"/admin/api/alerts", g.adminToken, nil, &view)
	if len(view.Alerts) != 1 || view.Alerts[0].Pool != "al" || view.Alerts[0].Kind != "low_keys" || view.Alerts[0].Level != "critical" || view.CheckedAt == "" {
		t.Fatalf("alerts view = %+v", view)
	}
	if res := adminCall(t, g, "gateway_admin_alerts", `{}`); res.IsError || !strings.Contains(res.Content[0].Text, `"kind":"low_keys"`) {
		t.Fatalf("admin MCP = %+v", res)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+g.admin+"/admin/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+g.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // 测试：关闭失败无影响
	if !strings.Contains(string(m), `uaig_alerts_firing{kind="low_keys",pool="al"} 1`) || !strings.Contains(string(m), `uaig_pool_available_keys{pool="al"} 0`) {
		t.Fatalf("metrics lack pool gauges:\n%s", m)
	}
	if strings.Contains(g.stderr.String(), "token-abc123") || !strings.Contains(g.stderr.String(), `"msg":"pool alert"`) {
		t.Fatalf("log leaks the webhook url or lacks the alert:\n%s", g.stderr.String())
	}
}
