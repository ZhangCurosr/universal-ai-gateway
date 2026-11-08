// Package config 定义网关配置结构、默认值、加载与校验（spec/03）。
package config

import "time"

// Config 是配置文件的根结构。
type Config struct {
	Server ServerConfig  `yaml:"server"`
	Limits LimitsConfig  `yaml:"limits"`
	Ledger LedgerConfig  `yaml:"ledger"`
	Audit  AuditConfig   `yaml:"audit"`
	Pools  []PoolConfig  `yaml:"pools"`
	Models []ModelConfig `yaml:"models"`
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
	MaxRequestBodyBytes    int64 `yaml:"max_request_body_bytes"`
	DefaultMaxOutputTokens int64 `yaml:"default_max_output_tokens"`

	// 全局过载保护（spec/09 §5）。
	MaxInFlightRequests  int   `yaml:"max_in_flight_requests"`  // 数据面模型请求的全局并发上限
	MaxBufferedBodyBytes int64 `yaml:"max_buffered_body_bytes"` // 在途请求体字节总预算
	PreAuthIPRPM         int   `yaml:"preauth_ip_rpm"`          // 鉴权前每个来源 IP 的 RPM；0 = 关闭
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

// 协议取值（与 provider.Protocol 的字符串值一致）。
const (
	ProtocolAnthropic  = "anthropic"
	ProtocolOpenAIChat = "openai_chat"
)

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
	MaxAttempts int            `yaml:"max_attempts"`
	Timeouts    TimeoutsConfig `yaml:"timeouts"`
	Cooldown    CooldownConfig `yaml:"cooldown"`
	Breaker     BreakerConfig  `yaml:"breaker"`
}

type TimeoutsConfig struct {
	Connect                          time.Duration `yaml:"connect"`
	ResponseHeaderBase               time.Duration `yaml:"response_header_base"`
	ResponseHeaderPer100kInputTokens time.Duration `yaml:"response_header_per_100k_input_tokens"`
	StreamIdle                       time.Duration `yaml:"stream_idle"`
	NonStreamTotal                   time.Duration `yaml:"non_stream_total"`
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

type ModelConfig struct {
	Name            string      `yaml:"name"`
	Pool            string      `yaml:"pool"`
	UpstreamModel   string      `yaml:"upstream_model"`
	MaxOutputTokens int64       `yaml:"max_output_tokens"`
	Price           PriceConfig `yaml:"price"`
}

// PriceConfig 单位：USD / 百万 token。转换为整数见 pricing.FromUSDPerMTok。
type PriceConfig struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
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
			MaxRequestBodyBytes:    32 << 20,
			DefaultMaxOutputTokens: 8192,
			MaxInFlightRequests:    256,
			MaxBufferedBodyBytes:   512 << 20,
			PreAuthIPRPM:           600,
		},
		Ledger: LedgerConfig{
			FlushInterval:          250 * time.Millisecond,
			EventQueueCapacity:     4096,
			AdmissionHighWatermark: 0.9,
		},
		Audit: AuditConfig{
			QueueCapacity: 8192,
			BatchSize:     256,
			FlushInterval: 250 * time.Millisecond,
			RetentionDays: 30,
		},
	}
}

// Normalize 为每个池填充缺省字段。Load 会自动调用。
func (c *Config) Normalize() {
	for i := range c.Pools {
		p := &c.Pools[i]
		setDefault(&p.Strategy, StrategyLeastInflight)
		setDefault(&p.AffinityTTL, 5*time.Minute)
		setDefault(&p.MaxAttempts, 3)
		setDefault(&p.Timeouts.Connect, 10*time.Second)
		setDefault(&p.Timeouts.ResponseHeaderBase, 60*time.Second)
		setDefault(&p.Timeouts.ResponseHeaderPer100kInputTokens, 30*time.Second)
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
