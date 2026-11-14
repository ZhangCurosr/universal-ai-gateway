package config

import (
	"strings"
	"testing"
	"time"
)

const filesYAML = `
server:
  data_dir: "./data"
  master_key_file: "./secrets/master.key"
files:
%s
pools:
  - name: mu
    protocol: rest
    base_url: "https://mineru.net"
models:
  - name: mu-api
    pool: mu
    price: { requests: "0" }
    tools:
      - name: parse_file
        description: "parse an uploaded file"
        method: %s
        path: /parse/file
%s
`

func filesCfg(files, method, tool string) string {
	s := strings.Replace(filesYAML, "%s", files, 1)
	s = strings.Replace(s, "%s", method, 1)
	return strings.Replace(s, "%s", tool, 1)
}

const fileTool = `        file: { arg: file, name_query: name }
        input_schema:
          type: object
          properties: { file: { type: string } }
          required: [file]`

// CFG-23：缺省值与 FS-30 的声明
func TestFilesConfigParses(t *testing.T) {
	cfg, err := Parse([]byte(filesCfg("  enabled: true", "post", fileTool)))
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.Files
	if !f.Enabled || f.MaxFileBytes != 200<<20 || f.MaxFilesPerClient != 100 || f.MaxBytesPerClient != 2<<30 ||
		f.MaxTotalBytes != 10<<30 || f.DefaultTTL != 24*time.Hour || f.MaxTTL != 168*time.Hour || f.Dir != "" {
		t.Fatalf("files = %+v", f)
	}
	if tf := cfg.Models[0].Tools[0].File; tf == nil || tf.Arg != "file" || tf.NameQuery != "name" {
		t.Fatalf("tool file = %+v", tf)
	}
	// 不写 files 段也合法（没有文件参数时）
	if _, err := Parse([]byte(filesCfg("  dir: x", "post", ""))); err != nil {
		t.Fatal(err)
	}
}

func TestFilesConfigRejects(t *testing.T) {
	cases := map[string][3]string{
		"must be <= limits.max_stream_body_bytes": {"  enabled: true\n  max_file_bytes: 999999999999", "post", fileTool},
		"files.max_files_per_client":              {"  enabled: true\n  max_files_per_client: -1", "post", fileTool},
		"files.max_bytes_per_client":              {"  enabled: true\n  max_file_bytes: 1000\n  max_bytes_per_client: 10", "post", fileTool},
		"files.max_total_bytes":                   {"  enabled: true\n  max_total_bytes: 10", "post", fileTool},
		"files.default_ttl":                       {"  enabled: true\n  default_ttl: 30s", "post", fileTool},
		"files.max_ttl":                           {"  enabled: true\n  max_ttl: 1h", "post", fileTool},
		"files.enabled is false":                  {"  enabled: false", "post", fileTool},
		"only allowed for POST and PUT":           {"  enabled: true", "get", fileTool},
		".file.arg":                               {"  enabled: true", "post", "        file: { arg: \"9x\" }"},
		".file.name_query":                        {"  enabled: true", "post", "        file: { arg: file, name_query: \"a b\" }"},
		"also a path placeholder":                 {"  enabled: true", "put", "        file: { arg: id }"},
	}
	for want, c := range cases {
		y := filesCfg(c[0], c[1], c[2])
		if want == "also a path placeholder" {
			y = strings.Replace(y, "path: /parse/file", "path: /parse/{id}", 1) +
				"        input_schema: { type: object, properties: { id: { type: string } }, required: [id] }\n"
		}
		_, err := Parse([]byte(y))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}
