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
	"strconv"
	"strings"
	"time"
)

// gateway keys import（spec/14 CLI-09、CLI-11，任务卡 T56、T62）：在本机读取账号文件，逐个调用管理 API 添加上游 Key。
// 密钥只在本进程与网关之间传递；任何输出都不包含密钥或文件中的其他字段值，
// 因此可以放心地让 AI 运行这条命令。

const maxImportFileBytes = 16 << 20

// maxImportLabelBytes 与管理 API 的标签上限一致（T62）。
const maxImportLabelBytes = 64

type whereFlags []string

func (w *whereFlags) String() string     { return strings.Join(*w, ",") }
func (w *whereFlags) Set(v string) error { *w = append(*w, v); return nil }

type importStats struct{ imported, updated, duplicate, expired, skipped, failed int }

// importMeta 是一行的元数据（T62）。setExpiry / setLabel 表示该字段由参数指定，需要写入。
type importMeta struct {
	expiresAt time.Time // 零值 = 不过期
	label     string
	setExpiry bool
	setLabel  bool
}

func cmdKeys(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "import" {
		fmt.Fprintln(stderr, "usage: gateway keys import --pool P --file F [--format jsonl|lines] [--field api_key] [--where k=v]... [--secret-prefix S] [--name-prefix N] [--weight W] [--max-in-flight M] [--expires-field F] [--tz Z] [--include-expired] [--label L | --label-field F] [--update] [--dry-run]")
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
	expiresField := fs.String("expires-field", "", "jsonl: field that holds the expiry time (RFC 3339, Unix seconds, YYYY-MM-DD HH:MM:SS or YYYY-MM-DD); empty value = never expires")
	tz := fs.String("tz", "", "time zone for expiry times without one, e.g. +08:00 or UTC (default: this machine's time zone)")
	includeExpired := fs.Bool("include-expired", false, "also import accounts that have already expired (default: skip them)")
	label := fs.String("label", "", "source label for every imported key, e.g. signup-2026-11")
	labelField := fs.String("label-field", "", "jsonl: field whose value becomes the key's label")
	update := fs.Bool("update", false, "for keys that already exist, update their expiry and label instead of counting them as duplicates")
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
	if *format == "lines" && (*expiresField != "" || *labelField != "") {
		fmt.Fprintln(stderr, "gateway: --expires-field and --label-field need --format jsonl")
		return 2
	}
	if *label != "" && *labelField != "" {
		fmt.Fprintln(stderr, "gateway: use either --label or --label-field, not both")
		return 2
	}
	if len(*label) > maxImportLabelBytes {
		fmt.Fprintf(stderr, "gateway: --label must be at most %d bytes\n", maxImportLabelBytes)
		return 2
	}
	if *update && *expiresField == "" && *label == "" && *labelField == "" {
		fmt.Fprintln(stderr, "gateway: --update needs --expires-field, --label or --label-field (there is nothing else to update)")
		return 2
	}
	loc, err := parseTZ(*tz)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
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
	now := time.Now()
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
		meta, why := extractMeta(sc.Text(), *expiresField, *label, *labelField, loc)
		if why != "" {
			st.skipped++
			fmt.Fprintf(stderr, "line %d: skipped (%s)\n", lineNo, why)
			continue
		}
		if !meta.expiresAt.IsZero() && !now.Before(meta.expiresAt) && !*includeExpired {
			st.expired++
			fmt.Fprintf(stderr, "line %d: skipped (expired)\n", lineNo)
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
		if !meta.expiresAt.IsZero() {
			body["expires_at"] = meta.expiresAt.Format(time.RFC3339)
		}
		if meta.label != "" {
			body["label"] = meta.label
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
		case status == http.StatusConflict && *update:
			if why := updateExisting(ctx, client, base, token, resp, meta); why != "" {
				st.failed++
				fmt.Fprintf(stderr, "line %d: failed (%s)\n", lineNo, redactToken(why, token))
				if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return 1
				}
			} else {
				st.updated++
			}
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
	fmt.Fprintf(stdout, "%s %d, duplicate %d, skipped %d (filtered or invalid), failed %d, updated %d, expired %d\n",
		verb, st.imported, st.duplicate, st.skipped, st.failed, st.updated, st.expired)
	if st.failed > 0 {
		return 1
	}
	return 0
}

// updateExisting 用 409 响应中的 key_id 更新已存在 Key 的过期时间与标签（T62 --update）。
// 成功返回空串；失败返回原因（不含请求内容）。
func updateExisting(ctx context.Context, client *http.Client, base, token string, conflict []byte, meta importMeta) string {
	var e struct {
		Error struct {
			KeyID int64 `json:"key_id"`
		} `json:"error"`
	}
	if json.Unmarshal(conflict, &e) != nil || e.Error.KeyID < 1 {
		return "duplicate key, but the gateway did not return its id"
	}
	patch := map[string]any{}
	if meta.setExpiry {
		if meta.expiresAt.IsZero() {
			patch["expires_at"] = nil // 文件中没有过期时间：清除
		} else {
			patch["expires_at"] = meta.expiresAt.Format(time.RFC3339)
		}
	}
	if meta.setLabel {
		patch["label"] = meta.label
	}
	raw, _ := json.Marshal(patch) // 只含字符串与 null，必然可以序列化
	status, resp, err := adminRequest(ctx, client, http.MethodPatch, base+"/admin/api/keys/"+strconv.FormatInt(e.Error.KeyID, 10), token, raw)
	if err != nil {
		return err.Error()
	}
	if status != http.StatusOK {
		return fmt.Sprintf("update HTTP %d: %s", status, apiErrorMessage(resp))
	}
	return ""
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

// extractMeta 读取一行的过期时间与标签（T62）。调用前 extractSecret 已确认该行是 JSON 对象（jsonl）。
// why 非空表示跳过该行；why 只描述原因，绝不包含行内容。
func extractMeta(line, expiresField, label, labelField string, loc *time.Location) (importMeta, string) {
	m := importMeta{label: label, setLabel: label != "" || labelField != "", setExpiry: expiresField != ""}
	if expiresField == "" && labelField == "" {
		return m, ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &obj); err != nil {
		return m, "not a JSON object"
	}
	if expiresField != "" {
		if raw, ok := obj[expiresField]; ok {
			t, err := parseExpiry(scalarString(raw), loc)
			if err != nil {
				return m, "bad " + expiresField
			}
			m.expiresAt = t
		}
	}
	if labelField != "" {
		if raw, ok := obj[labelField]; ok && string(bytes.TrimSpace(raw)) != "null" {
			m.label = strings.TrimSpace(scalarString(raw))
		}
		if len(m.label) > maxImportLabelBytes {
			return m, fmt.Sprintf("%s longer than %d bytes", labelField, maxImportLabelBytes)
		}
	}
	return m, ""
}

// parseExpiry 解析过期时间（T62）：RFC 3339、Unix 秒（≥ 10^11 视为毫秒）、
// 「YYYY-MM-DD HH:MM:SS」「YYYY-MM-DDTHH:MM:SS」「YYYY-MM-DD」（当天 0 点，偏保守）。
// 不带时区的按 loc 解释。空串与 null 表示不过期（返回零值）。
func parseExpiry(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return time.Time{}, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}, errors.New("non-positive Unix time")
		}
		if n >= 1e11 {
			return time.UnixMilli(n), nil
		}
		return time.Unix(n, 0), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("unrecognized time format")
}

// parseTZ 解析 --tz：空串为本机时区；UTC、Local；±HH:MM；或 IANA 名称（需要本机有时区数据库）。
func parseTZ(s string) (*time.Location, error) {
	switch s {
	case "", "Local", "local":
		return time.Local, nil
	case "UTC", "utc", "Z":
		return time.UTC, nil
	}
	if len(s) == 6 && (s[0] == '+' || s[0] == '-') && s[3] == ':' {
		h, err1 := strconv.Atoi(s[1:3])
		m, err2 := strconv.Atoi(s[4:6])
		if err1 == nil && err2 == nil && h <= 14 && m < 60 {
			off := h*3600 + m*60
			if s[0] == '-' {
				off = -off
			}
			return time.FixedZone("UTC"+s, off), nil
		}
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		return nil, fmt.Errorf("--tz: unknown time zone %q (use e.g. +08:00 or UTC)", s)
	}
	return loc, nil
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
