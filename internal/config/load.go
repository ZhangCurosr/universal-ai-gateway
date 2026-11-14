package config

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
)

// Load 读取配置文件：Default → YAML 覆盖 → Normalize → Validate。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse 解析 YAML 内容，未知字段视为错误（防止拼写错误被静默忽略）。
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate 按 spec/03 §4 校验，返回第一个错误。
func (c *Config) Validate() error {
	// CFG-01
	if _, _, err := net.SplitHostPort(c.Server.DataListen); err != nil {
		return fieldErr("server.data_listen", "%v", err)
	}
	host, _, err := net.SplitHostPort(c.Server.AdminListen)
	if err != nil {
		return fieldErr("server.admin_listen", "%v", err)
	}
	// CFG-02
	if !isLoopbackHost(host) {
		return fieldErr("server.admin_listen", "must be a loopback address in M1, got %q", host)
	}
	// CFG-03
	if err := checkKeyOutsideDataDir(c.Server.MasterKeyFile, c.Server.DataDir); err != nil {
		return err
	}
	switch c.Server.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fieldErr("server.log_level", "unknown level %q", c.Server.LogLevel)
	}

	// CFG-10
	s := c.Server
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 {
		return fieldErr("server", "read_header_timeout, read_timeout, idle_timeout, max_header_bytes must be > 0")
	}
	if s.ReadHeaderTimeout > s.ReadTimeout {
		return fieldErr("server.read_header_timeout", "must be <= read_timeout")
	}
	// CFG-11
	for i, cidr := range s.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fieldErr(fmt.Sprintf("server.trusted_proxies[%d]", i), "invalid CIDR %q", cidr)
		}
	}
	// CFG-16
	for i, h := range s.AdminHosts {
		if !validAdminHost(h) {
			return fieldErr(fmt.Sprintf("server.admin_hosts[%d]", i), "must be a host name or IP address without port, got %q", h)
		}
	}
	// CFG-12
	if c.Limits.MaxInFlightRequests <= 0 || c.Limits.PreAuthIPRPM < 0 {
		return fieldErr("limits", "max_in_flight_requests must be > 0 and preauth_ip_rpm >= 0")
	}
	if c.Limits.MaxBufferedBodyBytes < c.Limits.MaxRequestBodyBytes {
		return fieldErr("limits.max_buffered_body_bytes", "must be >= max_request_body_bytes")
	}

	// CFG-08
	if c.Limits.MaxRequestBodyBytes <= 0 {
		return fieldErr("limits.max_request_body_bytes", "must be > 0")
	}
	if c.Limits.MaxStreamBodyBytes <= 0 {
		return fieldErr("limits.max_stream_body_bytes", "must be > 0")
	}
	if c.MCP.MaxResultBytes <= 0 {
		return fieldErr("mcp.max_result_bytes", "must be > 0")
	}
	// CFG-19（工具名是否存在由装配代码在注册表封存后检查）
	switch c.MCP.ToolMode {
	case ToolModeAuto, ToolModeDirect, ToolModeCatalog:
	default:
		return fieldErr("mcp.tool_mode", "must be auto, direct or catalog, got %q", c.MCP.ToolMode)
	}
	if c.MCP.AutoThreshold < 1 {
		return fieldErr("mcp.auto_threshold", "must be >= 1, got %d", c.MCP.AutoThreshold)
	}
	if c.Ledger.FlushInterval <= 0 || c.Ledger.EventQueueCapacity <= 0 {
		return fieldErr("ledger", "flush_interval and event_queue_capacity must be > 0")
	}
	if w := c.Ledger.AdmissionHighWatermark; w <= 0 || w > 1 {
		return fieldErr("ledger.admission_high_watermark", "must be in (0, 1], got %v", w)
	}
	if c.Audit.QueueCapacity <= 0 || c.Audit.BatchSize <= 0 || c.Audit.FlushInterval <= 0 {
		return fieldErr("audit", "queue_capacity, batch_size, and flush_interval must be > 0")
	}
	// CFG-14
	if c.Audit.RetentionDays < 1 {
		return fieldErr("audit.retention_days", "must be >= 1, got %d", c.Audit.RetentionDays)
	}
	// CFG-15
	if c.Translation.UnknownFields != "reject" && c.Translation.UnknownFields != "drop" {
		return fieldErr("translation.unknown_fields", "must be \"reject\" or \"drop\", got %q", c.Translation.UnknownFields)
	}

	// CFG-09
	if len(c.Pools) == 0 {
		return fieldErr("pools", "at least one pool is required")
	}
	if len(c.Models) == 0 {
		return fieldErr("models", "at least one model is required")
	}

	seenPool := map[string]bool{}
	for i, p := range c.Pools {
		path := fmt.Sprintf("pools[%d]", i)
		// CFG-04
		if p.Name == "" {
			return fieldErr(path+".name", "must not be empty")
		}
		if seenPool[p.Name] {
			return fieldErr(path+".name", "duplicate %q", p.Name)
		}
		seenPool[p.Name] = true
		// 协议名是否有对应的适配器由 core.New 检查（CORE-01）；这里只校验格式。
		if !protocolName.MatchString(p.Protocol) {
			return fieldErr(path+".protocol", "must match [a-z][a-z0-9_]*, got %q", p.Protocol)
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fieldErr(path+".base_url", "must be an absolute http(s) URL, got %q", p.BaseURL)
		}
		// CFG-05
		if p.Strategy != StrategyLeastInflight && p.Strategy != StrategyWeightedRoundRobin {
			return fieldErr(path+".strategy", "unknown strategy %q", p.Strategy)
		}
		if p.MaxAttempts < 1 || p.MaxAttempts > 5 {
			return fieldErr(path+".max_attempts", "must be in [1, 5], got %d", p.MaxAttempts)
		}
		// CFG-08
		if p.Breaker.FailureRatio <= 0 || p.Breaker.FailureRatio > 1 {
			return fieldErr(path+".breaker.failure_ratio", "must be in (0, 1]")
		}
		if p.Cooldown.Default > p.Cooldown.Max {
			return fieldErr(path+".cooldown", "default must be <= max")
		}
		if p.Breaker.OpenDuration > p.Breaker.MaxOpenDuration {
			return fieldErr(path+".breaker", "open_duration must be <= max_open_duration")
		}
	}

	seenModel := map[string]bool{}
	seenTool := map[string]bool{}
	for i, m := range c.Models {
		path := fmt.Sprintf("models[%d]", i)
		// CFG-06
		if m.Name == "" {
			return fieldErr(path+".name", "must not be empty")
		}
		if seenModel[m.Name] {
			return fieldErr(path+".name", "duplicate %q", m.Name)
		}
		seenModel[m.Name] = true
		if !seenPool[m.Pool] {
			return fieldErr(path+".pool", "unknown pool %q", m.Pool)
		}
		if m.UpstreamModel == "" {
			return fieldErr(path+".upstream_model", "must not be empty")
		}
		// CFG-07：单位名合法、价格可解析、预留量非负。价目表是否覆盖出站适配器的全部单位由 core.New 检查（CORE-05）。
		for unit, v := range m.Price {
			if !pricing.ValidUnit(unit) {
				return fieldErr(path+".price", "invalid unit name %q", unit)
			}
			if _, err := pricing.ParseRate(v); err != nil {
				return fieldErr(path+".price."+unit, "%v", err)
			}
		}
		for unit, v := range m.Reserve {
			if !pricing.ValidUnit(unit) || v < 0 {
				return fieldErr(path+".reserve", "unit %q: name must be valid and value >= 0", unit)
			}
		}
		// CFG-17
		for j, t := range m.Tools {
			tp := fmt.Sprintf("%s.tools[%d]", path, j)
			if err := validateTool(tp, t); err != nil {
				return err
			}
			if seenTool[t.Name] {
				return fieldErr(tp+".name", "duplicate %q", t.Name)
			}
			seenTool[t.Name] = true
		}
		// CFG-20
		if m.API != nil {
			if err := validateAPI(path, m); err != nil {
				return err
			}
		}
	}
	if err := c.validateQuotas(); err != nil { // CFG-21：需要先知道各池服务的计量单位
		return err
	}
	return c.validateAlerts() // CFG-22
}

var (
	toolName        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	toolPlaceholder = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	toolMethods     = map[string]bool{"GET": true, "DELETE": true, "POST": true, "PUT": true, "PATCH": true}
	toolIdemMethods = map[string]bool{"POST": true, "PATCH": true}
)

// validateTool 实现 CFG-17 对单个接口声明的校验。
func validateTool(tp string, t ToolConfig) error {
	if !toolName.MatchString(t.Name) {
		return fieldErr(tp+".name", "must match ^[A-Za-z0-9_.-]{1,64}$, got %q", t.Name)
	}
	if strings.TrimSpace(t.Description) == "" {
		return fieldErr(tp+".description", "must not be empty")
	}
	if !toolMethods[t.Method] {
		return fieldErr(tp+".method", "must be GET, DELETE, POST, PUT or PATCH, got %q", t.Method)
	}
	if t.Idempotent && !toolIdemMethods[t.Method] {
		return fieldErr(tp+".idempotent", "only allowed for POST and PATCH")
	}
	names, err := ToolPathPlaceholders(t.Path)
	if err != nil {
		return fieldErr(tp+".path", "%v", err)
	}
	if t.InputSchema == nil {
		if len(names) > 0 {
			return fieldErr(tp+".input_schema", "must declare path placeholder %q", names[0])
		}
		return nil
	}
	if _, err := t.SchemaJSON(); err != nil {
		return fieldErr(tp+".input_schema", "must be a mapping with string keys: %v", err)
	}
	if typ, _ := t.InputSchema["type"].(string); typ != "object" {
		return fieldErr(tp+".input_schema.type", "must be \"object\"")
	}
	props, _ := t.InputSchema["properties"].(map[string]any)
	required := map[string]bool{}
	if req, ok := t.InputSchema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	for _, n := range names {
		if _, ok := props[n]; !ok {
			return fieldErr(tp+".input_schema.properties", "must declare path placeholder %q", n)
		}
		if !required[n] {
			return fieldErr(tp+".input_schema.required", "must include path placeholder %q", n)
		}
	}
	return nil
}

// ToolPathPlaceholders 校验接口声明的 path（CFG-17），返回其中占位符的名字（按出现顺序）。
// path 以 / 开头；不含 ?、#、\、空白与控制字符；百分号编码合法；没有 . 或 .. 段；
// {name} 的名字合法且不重复，不允许 {name...}，花括号必须成对。
func ToolPathPlaceholders(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("must start with /, got %q", path)
	}
	for _, c := range path {
		if c == '?' || c == '#' || c == '\\' || c <= ' ' || c == 0x7f {
			return nil, fmt.Errorf(`must not contain ?, #, \, whitespace or control characters`)
		}
	}
	var names []string
	seen := map[string]bool{}
	rest := path
	var literal strings.Builder
	for {
		i := strings.IndexAny(rest, "{}")
		if i < 0 {
			literal.WriteString(rest)
			break
		}
		if rest[i] == '}' {
			return nil, fmt.Errorf("unbalanced braces")
		}
		j := strings.IndexAny(rest[i+1:], "{}")
		if j < 0 || rest[i+1+j] != '}' {
			return nil, fmt.Errorf("unbalanced braces")
		}
		name := rest[i+1 : i+1+j]
		if !toolPlaceholder.MatchString(name) {
			return nil, fmt.Errorf("invalid placeholder {%s}", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("placeholder {%s} appears more than once", name)
		}
		seen[name] = true
		names = append(names, name)
		literal.WriteString(rest[:i])
		literal.WriteString("x") // 占位符在段检查中视为普通字符
		rest = rest[i+2+j:]
	}
	for _, seg := range strings.Split(literal.String()[1:], "/") {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return nil, fmt.Errorf("invalid percent-encoding in segment %q", seg)
		}
		if dec == "." || dec == ".." {
			return nil, fmt.Errorf("must not contain . or .. segments")
		}
	}
	return names, nil
}

var protocolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// adminHostName 是 server.admin_hosts 的合法取值（CFG-16）：主机名或 IP，不带端口与通配符。
var adminHostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// validAdminHost：含 ":" 的只能是 IPv6 地址（去掉方括号），否则必须匹配 adminHostName。
func validAdminHost(h string) bool {
	if strings.Contains(h, ":") {
		addr, err := netip.ParseAddr(strings.Trim(h, "[]"))
		return err == nil && addr.Is6()
	}
	return adminHostName.MatchString(h)
}

func fieldErr(path, format string, args ...any) error {
	return fmt.Errorf("config: %s: %s", path, fmt.Sprintf(format, args...))
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkKeyOutsideDataDir 实现 CFG-03。
func checkKeyOutsideDataDir(keyFile, dataDir string) error {
	keyAbs, err := filepath.Abs(keyFile)
	if err != nil {
		return fieldErr("server.master_key_file", "%v", err)
	}
	dirAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return fieldErr("server.data_dir", "%v", err)
	}
	rel, err := filepath.Rel(dirAbs, keyAbs)
	if err != nil {
		return nil // 不同盘符等情况：必然不在数据目录下
	}
	outside := rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
	if !outside {
		return fieldErr("server.master_key_file", "must not be inside data_dir (%s)", dataDir)
	}
	return nil
}
