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

func TestExampleConfigIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "gateway.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("gateway.example.yaml is invalid: %v", err)
	}
}
