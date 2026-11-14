package config

import (
	"fmt"
	"regexp"
	"time"
)

// FilesConfig 是文件中转的设置（T69，CFG-23，spec/03）。改动需要重启。
type FilesConfig struct {
	Enabled bool `yaml:"enabled"` // 缺省 false：不注册文件路由
	// Dir 是文件目录；为空时为 <server.data_dir>/files（由装配代码决定）。
	Dir               string        `yaml:"dir"`
	MaxFileBytes      int64         `yaml:"max_file_bytes"`       // 单个文件上限，缺省 200 MiB
	MaxFilesPerClient int           `yaml:"max_files_per_client"` // 缺省 100
	MaxBytesPerClient int64         `yaml:"max_bytes_per_client"` // 缺省 2 GiB
	MaxTotalBytes     int64         `yaml:"max_total_bytes"`      // 缺省 10 GiB
	DefaultTTL        time.Duration `yaml:"default_ttl"`          // 缺省 24h
	MaxTTL            time.Duration `yaml:"max_ttl"`              // 缺省 168h
}

// ToolFile 声明接口的文件参数（T69，FS-30）：参数 Arg 是文件 ID，文件内容作为请求体；
// NameQuery 非空时把文件名填进这个查询参数（客户端没有给出时）。
type ToolFile struct {
	Arg       string `yaml:"arg"`
	NameQuery string `yaml:"name_query"`
}

// FileNameQuery 是 FS-21 允许的查询参数名。
var FileNameQuery = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (f *FilesConfig) normalize() {
	setDefault(&f.MaxFileBytes, 200<<20)
	setDefault(&f.MaxFilesPerClient, 100)
	setDefault(&f.MaxBytesPerClient, 2<<30)
	setDefault(&f.MaxTotalBytes, 10<<30)
	setDefault(&f.DefaultTTL, 24*time.Hour)
	setDefault(&f.MaxTTL, 168*time.Hour)
}

// validateFiles 实现 CFG-23 与 FS-31。
func (c *Config) validateFiles() error {
	f := c.Files
	switch {
	case f.MaxFileBytes <= 0:
		return fieldErr("files.max_file_bytes", "must be > 0, got %d", f.MaxFileBytes)
	case f.MaxFileBytes > c.Limits.MaxStreamBodyBytes:
		return fieldErr("files.max_file_bytes", "must be <= limits.max_stream_body_bytes (%d), got %d", c.Limits.MaxStreamBodyBytes, f.MaxFileBytes)
	case f.MaxFilesPerClient <= 0:
		return fieldErr("files.max_files_per_client", "must be > 0, got %d", f.MaxFilesPerClient)
	case f.MaxBytesPerClient < f.MaxFileBytes:
		return fieldErr("files.max_bytes_per_client", "must be >= files.max_file_bytes (%d), got %d", f.MaxFileBytes, f.MaxBytesPerClient)
	case f.MaxTotalBytes < f.MaxBytesPerClient:
		return fieldErr("files.max_total_bytes", "must be >= files.max_bytes_per_client (%d), got %d", f.MaxBytesPerClient, f.MaxTotalBytes)
	case f.DefaultTTL < time.Minute:
		return fieldErr("files.default_ttl", "must be >= 1m, got %s", f.DefaultTTL)
	case f.MaxTTL < f.DefaultTTL:
		return fieldErr("files.max_ttl", "must be >= files.default_ttl (%s), got %s", f.DefaultTTL, f.MaxTTL)
	}
	for i, m := range c.Models {
		for j, t := range m.Tools {
			if t.File != nil && !f.Enabled {
				return fieldErr(fmt.Sprintf("models[%d].tools[%d].file", i, j), "tool %q takes a file, but files.enabled is false", t.Name)
			}
		}
	}
	return nil
}

// validateToolFile 实现 FS-30 的静态校验。
func validateToolFile(tp string, t ToolConfig, placeholders []string) error {
	f := t.File
	if t.Method != "POST" && t.Method != "PUT" {
		return fieldErr(tp+".file", "only allowed for POST and PUT")
	}
	if !toolPlaceholder.MatchString(f.Arg) {
		return fieldErr(tp+".file.arg", "must match ^[A-Za-z_][A-Za-z0-9_]*$, got %q", f.Arg)
	}
	for _, n := range placeholders {
		if n == f.Arg {
			return fieldErr(tp+".file.arg", "%q is also a path placeholder", f.Arg)
		}
	}
	if f.NameQuery != "" && !FileNameQuery.MatchString(f.NameQuery) {
		return fieldErr(tp+".file.name_query", "must match ^[A-Za-z0-9_.-]{1,64}$, got %q", f.NameQuery)
	}
	return nil
}
