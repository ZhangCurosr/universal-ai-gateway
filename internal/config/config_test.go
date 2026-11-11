package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalYAML = `
server:
  data_dir: "./data"
  master_key_file: "./secrets/master.key"
pools:
  - name: p1
    protocol: anthropic
    base_url: "https://api.anthropic.com"
models:
  - name: m1
    pool: p1
    upstream_model: up-m1
    price: { input_tokens: "3/1M", output_tokens: "15/1M" }
    reserve: { output_tokens: 1000 }
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Server.AdminListen != "127.0.0.1:8081" {
		t.Errorf("AdminListen = %q, want default", cfg.Server.AdminListen)
	}
	p := cfg.Pools[0]
	if p.Strategy != StrategyLeastInflight || p.MaxAttempts != 3 || p.AffinityTTL != 5*time.Minute {
		t.Errorf("pool defaults not applied: %+v", p)
	}
	if p.Timeouts.NonStreamTotal != 10*time.Minute || p.PinTTL != 24*time.Hour {
		t.Errorf("NonStreamTotal = %v, PinTTL = %v; want 10m, 24h", p.Timeouts.NonStreamTotal, p.PinTTL)
	}
	if cfg.Translation.UnknownFields != "reject" {
		t.Errorf("Translation.UnknownFields = %q, want reject", cfg.Translation.UnknownFields)
	}
}

func TestUpstreamModelDefaultsToName(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(minimalYAML, "    upstream_model: up-m1\n", "", 1)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Models[0].UpstreamModel != "m1" {
		t.Errorf("UpstreamModel = %q, want m1", cfg.Models[0].UpstreamModel)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := Parse([]byte(minimalYAML + "\nbogus_field: 1\n"))
	if err == nil {
		t.Fatal("Parse() with unknown field: error = nil, want error")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{"CFG-02 admin not loopback", func(c *Config) { c.Server.AdminListen = "0.0.0.0:8081" }, "admin_listen"},
		{"CFG-03 key inside data dir", func(c *Config) { c.Server.MasterKeyFile = "./data/master.key" }, "master_key_file"},
		{"CFG-04 duplicate pool", func(c *Config) { c.Pools = append(c.Pools, c.Pools[0]) }, "duplicate"},
		{"CFG-04 bad protocol name", func(c *Config) { c.Pools[0].Protocol = "Open-AI" }, "protocol"},
		{"CFG-04 bad base_url", func(c *Config) { c.Pools[0].BaseURL = "api.anthropic.com" }, "base_url"},
		{"CFG-05 max_attempts", func(c *Config) { c.Pools[0].MaxAttempts = 9 }, "max_attempts"},
		{"CFG-06 unknown pool", func(c *Config) { c.Models[0].Pool = "nope" }, "unknown pool"},
		{"CFG-07 negative price", func(c *Config) { c.Models[0].Price["input_tokens"] = "-1" }, "price.input_tokens"},
		{"CFG-07 bad unit name", func(c *Config) { c.Models[0].Price["Pages"] = "0.01" }, "invalid unit"},
		{"CFG-07 negative reserve", func(c *Config) { c.Models[0].Reserve["output_tokens"] = -1 }, "reserve"},
		{"CFG-08 watermark", func(c *Config) { c.Ledger.AdmissionHighWatermark = 1.5 }, "watermark"},
		{"CFG-09 no models", func(c *Config) { c.Models = nil }, "models"},
		{"CFG-10 header timeout > read timeout", func(c *Config) { c.Server.ReadHeaderTimeout = 2 * time.Minute }, "read_header_timeout"},
		{"CFG-11 bad trusted proxy", func(c *Config) { c.Server.TrustedProxies = []string{"10.0.0.1"} }, "trusted_proxies[0]"},
		{"CFG-12 body budget too small", func(c *Config) { c.Limits.MaxBufferedBodyBytes = 1 }, "max_buffered_body_bytes"},
		{"CFG-14 retention_days zero", func(c *Config) { c.Audit.RetentionDays = 0 }, "audit.retention_days"},
		{"CFG-14 retention_days negative", func(c *Config) { c.Audit.RetentionDays = -1 }, "audit.retention_days"},
		{"CFG-15 unknown_fields invalid", func(c *Config) { c.Translation.UnknownFields = "ignore" }, "translation.unknown_fields"},
		{"CFG-15 unknown_fields empty", func(c *Config) { c.Translation.UnknownFields = "" }, "translation.unknown_fields"},
		{"CFG-16 admin host with port", func(c *Config) { c.Server.AdminHosts = []string{"gw.local:8081"} }, "admin_hosts[0]"},
		{"CFG-16 admin host wildcard", func(c *Config) { c.Server.AdminHosts = []string{"ok.local", "*.local"} }, "admin_hosts[1]"},
		{"CFG-16 admin host empty", func(c *Config) { c.Server.AdminHosts = []string{""} }, "admin_hosts[0]"},
		{"CFG-16 admin host bad ipv6", func(c *Config) { c.Server.AdminHosts = []string{"fe80::zz"} }, "admin_hosts[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(minimalYAML))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			tt.mutate(cfg)
			err = cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestTranslationUnknownFieldsDropValid(t *testing.T) {
	yamlStr := minimalYAML + "\ntranslation:\n  unknown_fields: drop\n"
	cfg, err := Parse([]byte(yamlStr))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Translation.UnknownFields != "drop" {
		t.Errorf("Translation.UnknownFields = %q, want drop", cfg.Translation.UnknownFields)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "gateway.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("gateway.example.yaml is invalid: %v", err)
	}
}

func TestAdminHostsValid(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	cfg.Server.AdminHosts = []string{"gw.local", "Admin-1.example.com", "10.0.0.5", "::1", "[fe80::1]"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

// toolsYAML 是带接口声明的配置（CFG-17）；%s 处插入工具列表。
const toolsYAML = `
server:
  data_dir: "./data"
  master_key_file: "./secrets/master.key"
pools:
  - name: rp
    protocol: rest
    base_url: "https://api.example.com"
models:
  - name: svc
    pool: rp
    price: { requests: "0.001" }
    tools:
%s
`

const goodTool = `      - name: svc_get
        description: "get an item"
        method: get
        path: /items/{id}
        input_schema:
          type: object
          properties:
            id: { type: string }
          required: [id]
`

func toolCfg(tools string) string { return strings.Replace(toolsYAML, "%s", tools, 1) }

func TestToolConfigDefaults(t *testing.T) {
	cfg, err := Parse([]byte(toolCfg(goodTool + `      - name: svc_post
        description: "create"
        method: POST
        path: /items
`)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	tools := cfg.Models[0].Tools
	if tools[0].Method != "GET" || !tools[0].IsReadOnly() {
		t.Errorf("tool 0: method %q read_only %v, want GET true", tools[0].Method, tools[0].IsReadOnly())
	}
	if tools[1].IsReadOnly() {
		t.Error("POST tool must default to read_only false")
	}
	if b, err := tools[1].SchemaJSON(); err != nil || string(b) != DefaultToolSchema {
		t.Errorf("default schema = %s, %v", b, err)
	}
	b, err := tools[0].SchemaJSON()
	if err != nil || !strings.Contains(string(b), `"required":["id"]`) {
		t.Errorf("schema JSON = %s, %v", b, err)
	}
	if cfg.MCP.ToolMode != ToolModeAuto || cfg.MCP.AutoThreshold != 8 {
		t.Errorf("mcp defaults = %q / %d", cfg.MCP.ToolMode, cfg.MCP.AutoThreshold)
	}
}

func TestToolConfigReadOnlyExplicit(t *testing.T) {
	cfg, err := Parse([]byte(toolCfg(`      - name: svc_q
        description: "query"
        method: POST
        path: /q
        idempotent: true
        read_only: true
`)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if tool := cfg.Models[0].Tools[0]; !tool.IsReadOnly() || !tool.Idempotent {
		t.Errorf("tool = %+v", tool)
	}
}

func TestToolConfigRejects(t *testing.T) {
	base := func(name, method, path, extra string) string {
		return "      - name: " + name + "\n        description: \"d\"\n        method: " + method + "\n        path: \"" + path + "\"\n" + extra
	}
	schemaID := "        input_schema:\n          type: object\n          properties: { id: { type: string } }\n          required: [id]\n"
	tests := []struct {
		name, tools, want string
	}{
		{"bad name", base("bad name", "GET", "/x", ""), "tools[0].name"},
		{"duplicate", base("a", "GET", "/x", "") + base("a", "GET", "/y", ""), "duplicate"},
		{"empty description", "      - name: a\n        description: \" \"\n        method: GET\n        path: /x\n", "description"},
		{"bad method", base("a", "OPTIONS", "/x", ""), "method"},
		{"no leading slash", base("a", "GET", "x", ""), "path"},
		{"query", base("a", "GET", "/x?y=1", ""), "path"},
		{"fragment", base("a", "GET", "/x#y", ""), "path"},
		{"space", base("a", "GET", "/x y", ""), "path"},
		{"dot dot", base("a", "GET", "/a/../b", ""), "path"},
		{"encoded dot dot", base("a", "GET", "/a/%2e%2e/b", ""), "path"},
		{"bad escape", base("a", "GET", "/a%zz", ""), "path"},
		{"wildcard placeholder", base("a", "GET", "/a/{id...}", schemaID), "path"},
		{"unbalanced", base("a", "GET", "/a/{id", ""), "path"},
		{"repeated placeholder", base("a", "GET", "/{id}/{id}", schemaID), "path"},
		{"placeholder without schema", base("a", "GET", "/a/{id}", ""), "input_schema"},
		{"placeholder not in properties", base("a", "GET", "/a/{id}", "        input_schema:\n          type: object\n          required: [id]\n"), "properties"},
		{"placeholder not required", base("a", "GET", "/a/{id}", "        input_schema:\n          type: object\n          properties: { id: { type: string } }\n"), "required"},
		{"schema not object", base("a", "GET", "/x", "        input_schema:\n          type: array\n"), "input_schema.type"},
		{"non-string schema key", base("a", "GET", "/x", "        input_schema:\n          type: object\n          properties:\n            ? [a, b]\n            : { type: string }\n"), "invalid map key"}, // yaml.v3 在解析阶段就拒绝非标量键
		{"idempotent GET", base("a", "GET", "/x", "        idempotent: true\n"), "idempotent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(toolCfg(tt.tools)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestMCPToolModeValidation(t *testing.T) {
	for _, extra := range []string{"\nmcp:\n  tool_mode: lazy\n", "\nmcp:\n  auto_threshold: 0\n"} {
		if _, err := Parse([]byte(minimalYAML + extra)); err == nil {
			t.Errorf("accepted %q", extra)
		}
	}
	cfg, err := Parse([]byte(minimalYAML + "\nmcp:\n  tool_mode: catalog\n  pinned_tools: [a, b]\n"))
	if err != nil || cfg.MCP.ToolMode != ToolModeCatalog || len(cfg.MCP.PinnedTools) != 2 {
		t.Fatalf("Parse() = %+v, %v", cfg, err)
	}
}

func TestToolPathPlaceholders(t *testing.T) {
	names, err := ToolPathPlaceholders("/repos/{owner}/{repo}/files/{id}.json")
	if err != nil || strings.Join(names, ",") != "owner,repo,id" {
		t.Fatalf("names = %v, %v", names, err)
	}
	if names, err := ToolPathPlaceholders("/"); err != nil || len(names) != 0 {
		t.Fatalf("root: %v, %v", names, err)
	}
}
