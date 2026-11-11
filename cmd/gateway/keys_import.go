package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// gateway keys import（spec/14 CLI-09，任务卡 T56）：在本机读取账号文件，逐个调用管理 API 添加上游 Key。
// 密钥只在本进程与网关之间传递；任何输出都不包含密钥或文件中的其他字段值，
// 因此可以放心地让 AI 运行这条命令。

const maxImportFileBytes = 16 << 20

type whereFlags []string

func (w *whereFlags) String() string     { return strings.Join(*w, ",") }
func (w *whereFlags) Set(v string) error { *w = append(*w, v); return nil }

type importStats struct{ imported, duplicate, skipped, failed int }

func cmdKeys(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "import" {
		fmt.Fprintln(stderr, "usage: gateway keys import --pool P --file F [--format jsonl|lines] [--field api_key] [--where k=v]... [--secret-prefix S] [--name-prefix N] [--weight W] [--max-in-flight M] [--dry-run]")
		return 2
	}
	fs := flag.NewFlagSet("keys import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pool := fs.String("pool", "", "pool to add the keys to (required)")
	file := fs.String("file", "", "account file to read (required)")
	format := fs.String("format", "jsonl", "jsonl (one JSON object per line) or lines (one key per line)")
	field := fs.String("field", "api_key", "jsonl: field that holds the key")
	var where whereFlags
	fs.Var(&where, "where", "jsonl: only import lines whose top-level field equals the value, k=v (repeatable)")
	prefix := fs.String("secret-prefix", "", "prepended to every key, e.g. bearer: (spec/08 §8.3.3)")
	namePrefix := fs.String("name-prefix", "", "key names are <prefix>-001, <prefix>-002, ... (default: the pool name)")
	weight := fs.Int("weight", 0, "weight for every key (0 = gateway default)")
	maxInFlight := fs.Int("max-in-flight", 0, "max in-flight requests per key (0 = gateway default)")
	dryRun := fs.Bool("dry-run", false, "only parse and count, do not add keys")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *pool == "" || *file == "" {
		fmt.Fprintln(stderr, "gateway: --pool and --file are required")
		return 2
	}
	if *format != "jsonl" && *format != "lines" {
		fmt.Fprintln(stderr, "gateway: --format must be jsonl or lines")
		return 2
	}
	conds := map[string]string{}
	for _, w := range where {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			fmt.Fprintln(stderr, "gateway: --where must look like field=value")
			return 2
		}
		conds[k] = v
	}
	if *namePrefix == "" {
		*namePrefix = *pool
	}

	token := getenv(envAdminToken)
	if token == "" && !*dryRun {
		fmt.Fprintln(stderr, "gateway: "+envAdminToken+" is required")
		return 2
	}
	base := strings.TrimRight(getenv(envAdminURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8081"
	}

	data, err := readLimited(*file, maxImportFileBytes)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		return 1
	}

	var st importStats
	client := newCLIHTTPClient()
	seq := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxImportFileBytes)
	for lineNo := 1; sc.Scan(); lineNo++ {
		secret, ok, why := extractSecret(sc.Text(), *format, *field, conds)
		if !ok {
			if why != "" {
				st.skipped++
				fmt.Fprintf(stderr, "line %d: skipped (%s)\n", lineNo, why)
			}
			continue
		}
		seq++
		if *dryRun {
			st.imported++
			continue
		}
		body := map[string]any{"pool": *pool, "name": fmt.Sprintf("%s-%03d", *namePrefix, seq), "secret": *prefix + secret}
		if *weight > 0 {
			body["weight"] = *weight
		}
		if *maxInFlight > 0 {
			body["max_in_flight"] = *maxInFlight
		}
		raw, _ := json.Marshal(body) // 只含字符串与整数，必然可以序列化
		status, resp, err := adminRequest(ctx, client, http.MethodPost, base+"/admin/api/keys", token, raw)
		switch {
		case err != nil:
			st.failed++
			fmt.Fprintf(stderr, "line %d: failed (%s)\n", lineNo, redactToken(err.Error(), token))
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return 1
			}
		case status == http.StatusCreated:
			st.imported++
		case status == http.StatusConflict:
			st.duplicate++
		default:
			st.failed++
			fmt.Fprintf(stderr, "line %d: failed (HTTP %d: %s)\n", lineNo, status, apiErrorMessage(resp))
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(stderr, "gateway: read file:", err)
		return 1
	}
	verb := "imported"
	if *dryRun {
		verb = "would import"
	}
	fmt.Fprintf(stdout, "%s %d, duplicate %d, skipped %d (filtered or invalid), failed %d\n", verb, st.imported, st.duplicate, st.skipped, st.failed)
	if st.failed > 0 {
		return 1
	}
	return 0
}

// extractSecret 从一行中取出密钥。ok=false 且 why 为空表示应静默忽略（空行、注释、被 --where 过滤）。
// why 只描述原因，绝不包含行内容。
func extractSecret(line, format, field string, conds map[string]string) (secret string, ok bool, why string) {
	line = strings.TrimSpace(line)
	if format == "lines" {
		if line == "" || strings.HasPrefix(line, "#") {
			return "", false, ""
		}
		return line, true, ""
	}
	if line == "" {
		return "", false, ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return "", false, "not a JSON object"
	}
	for k, want := range conds {
		raw, present := obj[k]
		if !present || scalarString(raw) != want {
			return "", false, "filtered by --where"
		}
	}
	raw, present := obj[field]
	if !present {
		return "", false, "no " + field + " field"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || strings.TrimSpace(s) == "" {
		return "", false, field + " is not a non-empty string"
	}
	return strings.TrimSpace(s), true, ""
}

// scalarString 返回 JSON 值的字符串形式：字符串取其内容，其他取原文（如 true、3）。
func scalarString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// apiErrorMessage 取管理 API 错误体中的 message（不回显请求内容）。
func apiErrorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return "unexpected response"
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close() //nolint:errcheck // 只读文件，关闭失败无可处理
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file is larger than %d MiB", limit>>20)
	}
	return data, nil
}
