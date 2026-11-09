// Package config 定义网关配置结构、默认值、加载与校验（spec/03）。
package config

import "time"

// Config 是配置文件的根结构。
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Limits      LimitsConfig      `yaml:"limits"`
	Ledger      LedgerConfig      `yaml:"ledger"`
	Audit       AuditConfig       `yaml:"audit"`
	Translation TranslationConfig `yaml:"translation"`
	MCP         MCPConfig         `yaml:"mcp"`
	Pools       []PoolConfig      `yaml:"pools"`
	Models      []ModelConfig     `yaml:"models"`
}

type ServerConfig struct {
	DataListen    string        `yaml:"data_listen"`
	AdminListen   string        `yaml:"admin_listen"`
	DataDir       string        `yaml:"data_dir"`
	MasterKeyFile string        `yaml:"master_key_file"`
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`
	LogLevel      string        `yaml:"log_level"`

	// http.Server 超时（spec/03 §6）。WriteTimeout 固定为 0（会截断长流），不提供配置项。
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`

	// TrustedProxies 是可信反向代理的 CIDR 列表（spec/03 §7）。空 = 不信任任何 X-Forwarded-For。
	TrustedProxies []string `yaml:"trusted_proxies"`
}

type LimitsConfig struct {
	MaxRequestBodyBytes int64 `yaml:"max_request_body_bytes"` // 缓冲请求体（BodyBuffered）的上限
	MaxStreamBodyBytes  int64 `yaml:"max_stream_body_bytes"`  // 流式请求体（BodyStream，如文件上传）的上限

	// 全局过载保护（spec/09 §5）。
	MaxInFlightRequests  int   `yaml:"max_in_flight_requests"`  // 数据面模型请求的全局并发上限
	MaxBufferedBodyBytes int64 `yaml:"max_buffered_body_bytes"` // 在途请求体字节总预算
	PreAuthIPRPM         int   `yaml:"preauth_ip_rpm"`          // 鉴权前每个来源 IP 的 RPM；0 = 关闭
}

// MCPConfig 是 MCP 端点 /mcp 的配置（spec/14）。
type MCPConfig struct {
	Disabled       bool  `yaml:"disabled"`         // true = 不挂载 /mcp
	MaxResultBytes int64 `yaml:"max_result_bytes"` // 单次工具调用的端点响应上限
}

type LedgerConfig struct {
	FlushInterval          time.Duration `yaml:"flush_interval"`
	EventQueueCapacity     int           `yaml:"event_queue_capacity"`
	AdmissionHighWatermark float64       `yaml:"admission_high_watermark"`
}

type AuditConfig struct {
	QueueCapacity int           `yaml:"queue_capacity"`
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
	RetentionDays int           `yaml:"retention_days"`
}

// TranslationConfig 是转译层字段容忍配置（spec/10 §1 第 5 条，spec/03）。
type TranslationConfig struct {
	UnknownFields string `yaml:"unknown_fields"` // reject | drop（CFG-15）
}

// 池策略取值。
const (
	StrategyLeastInflight      = "least_inflight"
	StrategyWeightedRoundRobin = "weighted_round_robin"
)

type PoolConfig struct {
	Name        string         `yaml:"name"`
	Protocol    string         `yaml:"protocol"`
	BaseURL     string         `yaml:"base_url"`
	Strategy    string         `yaml:"strategy"`
	AffinityTTL time.Duration  `yaml:"affinity_ttl"`
	PinTTL      time.Duration  `yaml:"pin_ttl"` // 任务固定（Response.Pins）的有效期（spec/07 §7.1）
	MaxAttempts int            `yaml:"max_attempts"`
	Timeouts    TimeoutsConfig `yaml:"timeouts"`
	Cooldown    CooldownConfig `yaml:"cooldown"`
	Breaker     BreakerConfig  `yaml:"breaker"`
}

type TimeoutsConfig struct {
	Connect            time.Duration `yaml:"connect"`
	ResponseHeaderBase time.Duration `yaml:"response_header_base"`
	// ResponseHeaderPerMiB 是请求体每 MiB 追加的等待时长（大输入需要更久才开始响应）。
	ResponseHeaderPerMiB time.Duration `yaml:"response_header_per_mib"`
	StreamIdle           time.Duration `yaml:"stream_idle"`
	NonStreamTotal       time.Duration `yaml:"non_stream_total"`
}

type CooldownConfig struct {
	Default time.Duration `yaml:"default"`
	Max     time.Duration `yaml:"max"`
}

type BreakerConfig struct {
	Window          time.Duration `yaml:"window"`
	MinRequests     int           `yaml:"min_requests"`
	FailureRatio    float64       `yaml:"failure_ratio"`
	OpenDuration    time.Duration `yaml:"open_duration"`
	MaxOpenDuration time.Duration `yaml:"max_open_duration"`
}

// ModelConfig 是一个客户端可见的服务（大模型、搜索、文档解析……），名称沿用 "model"。
type ModelConfig struct {
	Name string `yaml:"name"`
	Pool string `yaml:"pool"`
	// UpstreamModel 是发给上游的服务名；为空时等于 Name。
	UpstreamModel string `yaml:"upstream_model"`
	// Price 是价目表：计量单位 → 价格字符串 "USD[/PER]"（spec/06 PRC-05），如 "3/1M"、"0.008"。
	Price map[string]string `yaml:"price"`
	// Reserve 是预留用量的上限兼默认值：计量单位 → 数量（spec/06 PRC-03），如 {output_tokens: 8192}。
	Reserve map[string]int64 `yaml:"reserve"`
}

// Default 返回全部顶层默认值（spec/03 §3）。池级默认值由 Normalize 填充。
func Default() Config {
	return Config{
		Server: ServerConfig{
			DataListen:        "0.0.0.0:8080",
			AdminListen:       "127.0.0.1:8081",
			DataDir:           "./data",
			MasterKeyFile:     "./secrets/master.key",
			ShutdownGrace:     30 * time.Second,
			LogLevel:          "info",
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
		},
		Limits: LimitsConfig{
			MaxRequestBodyBytes:  32 << 20,
			MaxStreamBodyBytes:   512 << 20,
			MaxInFlightRequests:  256,
			MaxBufferedBodyBytes: 512 << 20,
			PreAuthIPRPM:         600,
		},
		Ledger: LedgerConfig{
			FlushInterval:          250 * time.Millisecond,
			EventQueueCapacity:     4096,
			AdmissionHighWatermark: 0.9,
		},
		MCP: MCPConfig{MaxResultBytes: 4 << 20},
		Audit: AuditConfig{
			QueueCapacity: 8192,
			BatchSize:     256,
			FlushInterval: 250 * time.Millisecond,
			RetentionDays: 30,
		},
		Translation: TranslationConfig{
			UnknownFields: "reject",
		},
	}
}

// Normalize 为每个池填充缺省字段。Load 会自动调用。
func (c *Config) Normalize() {
	setDefault(&c.Translation.UnknownFields, "reject")
	for i := range c.Pools {
		p := &c.Pools[i]
		setDefault(&p.Strategy, StrategyLeastInflight)
		setDefault(&p.AffinityTTL, 5*time.Minute)
		setDefault(&p.PinTTL, 24*time.Hour)
		setDefault(&p.MaxAttempts, 3)
		setDefault(&p.Timeouts.Connect, 10*time.Second)
		setDefault(&p.Timeouts.ResponseHeaderBase, 60*time.Second)
		setDefault(&p.Timeouts.ResponseHeaderPerMiB, 90*time.Second)
		setDefault(&p.Timeouts.StreamIdle, 120*time.Second)
		setDefault(&p.Timeouts.NonStreamTotal, 10*time.Minute)
		setDefault(&p.Cooldown.Default, 30*time.Second)
		setDefault(&p.Cooldown.Max, 10*time.Minute)
		setDefault(&p.Breaker.Window, 60*time.Second)
		setDefault(&p.Breaker.MinRequests, 10)
		setDefault(&p.Breaker.FailureRatio, 0.5)
		setDefault(&p.Breaker.OpenDuration, 30*time.Second)
		setDefault(&p.Breaker.MaxOpenDuration, 5*time.Minute)
	}
	for i := range c.Models {
		setDefault(&c.Models[i].UpstreamModel, c.Models[i].Name)
	}
}

func setDefault[T comparable](field *T, def T) {
	var zero T
	if *field == zero {
		*field = def
	}
}

// Pool 按名称查找池。
func (c *Config) Pool(name string) (*PoolConfig, bool) {
	for i := range c.Pools {
		if c.Pools[i].Name == name {
			return &c.Pools[i], true
		}
	}
	return nil, false
}

// Model 按客户端可见的模型名查找模型。
func (c *Config) Model(name string) (*ModelConfig, bool) {
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i], true
		}
	}
	return nil, false
}
