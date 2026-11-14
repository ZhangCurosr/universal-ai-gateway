package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
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

const keysUsage = "usage: gateway keys import|sync --pool P --file F [--format jsonl|lines|csv] [--field api_key] [--where k=v]... [--secret-prefix S] [--name-prefix N] [--weight W] [--max-in-flight M] [--expires-field F] [--tz Z] [--include-expired] [--label L | --label-field F] [--update] [--dry-run]\n" +
	"       sync also takes: --label L (required) [--force] [--watch 10m]"

// importOpts 是 keys import / keys sync 的参数（CLI-09、CLI-11 ~ CLI-15）。
type importOpts struct {
	sync                                  bool
	pool, file, format, field             string
	prefix, namePrefix                    string
	weight, maxInFlight                   int
	expiresField, label, labelField       string
	loc                                   *time.Location
	includeExpired, update, dryRun, force bool
	watch                                 time.Duration
	conds                                 map[string]string
	base, token                           string
}

func cmdKeys(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "import" && args[0] != "sync") {
		fmt.Fprintln(stderr, keysUsage)
		return 2
	}
	o, code := parseImportOpts(args[0] == "sync", args[1:], getenv, stderr)
	if o == nil {
		return code
	}
	if o.sync {
		return runSync(ctx, o, stdout, stderr)
	}
	st, code := runImport(ctx, o, stderr, nil)
	if st == nil {
		return code
	}
	verb := "imported"
	if o.dryRun {
		verb = "would import"
	}
	fmt.Fprintf(stdout, "%s %d, duplicate %d, skipped %d (filtered or invalid), failed %d, updated %d, expired %d\n",
		verb, st.imported, st.duplicate, st.skipped, st.failed, st.updated, st.expired)
	return code
}

// parseImportOpts 解析参数；返回 nil 时第二个值是退出码。
func parseImportOpts(sync bool, args []string, getenv func(string) string, stderr io.Writer) (*importOpts, int) {
	name := "keys import"
	if sync {
		name = "keys sync"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	pool := fs.String("pool", "", "pool to add the keys to (required)")
	file := fs.String("file", "", "account file to read (required)")
	format := fs.String("format", "jsonl", "jsonl (one JSON object per line), lines (one key per line) or csv (first row is the header)")
	field := fs.String("field", "api_key", "jsonl / csv: field that holds the key")
	var where whereFlags
	fs.Var(&where, "where", "jsonl / csv: only import rows whose field equals the value, k=v (repeatable)")
	prefix := fs.String("secret-prefix", "", "prepended to every key, e.g. bearer: (spec/08 §8.3.3)")
	namePrefix := fs.String("name-prefix", "", "key names are <prefix>-001, <prefix>-002, ... (default: the pool name)")
	weight := fs.Int("weight", 0, "weight for every key (0 = gateway default)")
	maxInFlight := fs.Int("max-in-flight", 0, "max in-flight requests per key (0 = gateway default)")
	expiresField := fs.String("expires-field", "", "jsonl / csv: field that holds the expiry time (RFC 3339, Unix seconds, YYYY-MM-DD HH:MM:SS or YYYY-MM-DD); empty value = never expires")
	tz := fs.String("tz", "", "time zone for expiry times without one, e.g. +08:00 or UTC (default: this machine's time zone)")
	includeExpired := fs.Bool("include-expired", false, "also import accounts that have already expired (default: skip them)")
	label := fs.String("label", "", "source label for every imported key, e.g. signup-2026-11 (sync: required; only keys with this label are disabled or restored)")
	labelField := fs.String("label-field", "", "jsonl / csv: field whose value becomes the key's label")
	update := fs.Bool("update", false, "for keys that already exist, update their expiry and label instead of counting them as duplicates (sync: always on)")
	dryRun := fs.Bool("dry-run", false, "only parse and count, do not change anything")
	force := fs.Bool("force", false, "sync: disable keys even if more than half of the source's keys would be disabled")
	watch := fs.Duration("watch", 0, "sync: keep running and sync whenever the file changes, checking at this interval (>= 1m)")
	if err := fs.Parse(args); err != nil {
		return nil, 2
	}
	o := &importOpts{sync: sync, pool: *pool, file: *file, format: *format, field: *field, prefix: *prefix, namePrefix: *namePrefix,
		weight: *weight, maxInFlight: *maxInFlight, expiresField: *expiresField, label: *label, labelField: *labelField,
		includeExpired: *includeExpired, update: *update || sync, dryRun: *dryRun, force: *force, watch: *watch, conds: map[string]string{}}
	fail := func(msg string) (*importOpts, int) {
		fmt.Fprintln(stderr, "gateway: "+msg)
		return nil, 2
	}
	switch {
	case o.pool == "" || o.file == "":
		return fail("--pool and --file are required")
	case o.format != "jsonl" && o.format != "lines" && o.format != "csv":
		return fail("--format must be jsonl, lines or csv")
	case o.format == "lines" && (o.expiresField != "" || o.labelField != "" || len(where) > 0):
		return fail("--expires-field, --label-field and --where need --format jsonl or csv")
	case o.label != "" && o.labelField != "":
		return fail("use either --label or --label-field, not both")
	case len(o.label) > maxImportLabelBytes:
		return fail(fmt.Sprintf("--label must be at most %d bytes", maxImportLabelBytes))
	case !sync && o.update && o.expiresField == "" && o.label == "" && o.labelField == "":
		return fail("--update needs --expires-field, --label or --label-field (there is nothing else to update)")
	case sync && o.label == "":
		return fail("keys sync needs --label: only keys with this label are disabled or restored")
	case !sync && (o.force || o.watch != 0):
		return fail("--force and --watch are only for keys sync")
	case o.watch != 0 && o.watch < time.Minute:
		return fail("--watch must be at least 1m")
	}
	loc, err := parseTZ(*tz)
	if err != nil {
		return fail(err.Error())
	}
	o.loc = loc
	for _, w := range where {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return fail("--where must look like field=value")
		}
		o.conds[k] = v
	}
	if o.namePrefix == "" {
		o.namePrefix = o.pool
	}
	o.token = getenv(envAdminToken)
	if o.token == "" && !o.dryRun {
		return fail(envAdminToken + " is required")
	}
	o.base = strings.TrimRight(getenv(envAdminURL), "/")
	if o.base == "" {
		o.base = "http://127.0.0.1:8081"
	}
	return o, 0
}

// sourceRow 是账号文件中的一行（CSV 已转换为等价的 JSON 对象行）；why 非空表示该行无法读取。
type sourceRow struct {
	line int
	text string
	why  string
}

// loadRows 读取账号文件。CSV（CLI-12）的每个数据行转换为以表头为键的 JSON 对象，之后与 JSONL 走同一条路径。
func loadRows(path, format string) ([]sourceRow, error) {
	data, err := readLimited(path, maxImportFileBytes)
	if err != nil {
		return nil, err
	}
	if format == "csv" {
		return csvRows(data)
	}
	var rows []sourceRow
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxImportFileBytes)
	for n := 1; sc.Scan(); n++ {
		rows = append(rows, sourceRow{line: n, text: sc.Text()})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return rows, nil
}

// csvRows 实现 CLI-12：第一行是表头；第一行含制表符且不含逗号时用制表符分隔；列数与表头不同的行标记为无法读取。
func csvRows(data []byte) ([]sourceRow, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	first, _, _ := bytes.Cut(data, []byte("\n"))
	r := csv.NewReader(bytes.NewReader(data))
	if bytes.Contains(first, []byte("\t")) && !bytes.Contains(first, []byte(",")) {
		r.Comma = '\t'
	}
	r.FieldsPerRecord = -1 // 列数由下面自己检查，便于只跳过该行
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read csv header: %w", err)
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i])
	}
	var rows []sourceRow
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) { // 引号不匹配等：跳过这一行（不回显内容）
				rows = append(rows, sourceRow{line: pe.StartLine, why: "malformed CSV row"})
				continue
			}
			return nil, fmt.Errorf("read csv: %w", err)
		}
		line, _ := r.FieldPos(0)
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		if len(rec) != len(header) {
			rows = append(rows, sourceRow{line: line, why: fmt.Sprintf("has %d columns, header has %d", len(rec), len(header))})
			continue
		}
		obj := make(map[string]string, len(header))
		for i, name := range header {
			if name != "" {
				obj[name] = rec[i]
			}
		}
		b, _ := json.Marshal(obj) // map[string]string 必然可以序列化
		rows = append(rows, sourceRow{line: line, text: string(b)})
	}
}

// runImport 逐行导入（CLI-09、CLI-11）。onKey 非 nil 时，每个在源中的行（新增、重复或已更新）对应的 Key ID 都会传给它（CLI-13）。
// 返回 nil 统计时第二个值是退出码；否则有失败时退出码为 1。
func runImport(ctx context.Context, o *importOpts, stderr io.Writer, onKey func(id int64)) (*importStats, int) {
	rows, err := loadRows(o.file, o.format)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		return nil, 1
	}
	format := o.format
	if format == "csv" {
		format = "jsonl"
	}
	var st importStats
	client := newCLIHTTPClient()
	now := time.Now()
	seq := 0
	if !o.dryRun { // 名称编号接在池中同前缀已有的最大编号之后，重复运行不会撞名（与 ADM-23 相同）
		keys, err := listSyncKeys(ctx, o)
		if err != nil {
			fmt.Fprintln(stderr, "gateway: list keys:", redactToken(err.Error(), o.token))
			return nil, 1
		}
		for _, k := range keys {
			if n, ok := keyNameSeq(k.Name, o.namePrefix); ok && k.Pool == o.pool && n > seq {
				seq = n
			}
		}
	}
	for _, row := range rows {
		if row.why != "" {
			st.skipped++
			fmt.Fprintf(stderr, "line %d: skipped (%s)\n", row.line, row.why)
			continue
		}
		secret, ok, why := extractSecret(row.text, format, o.field, o.conds)
		if !ok {
			if why != "" {
				st.skipped++
				fmt.Fprintf(stderr, "line %d: skipped (%s)\n", row.line, why)
			}
			continue
		}
		meta, why := extractMeta(row.text, o.expiresField, o.label, o.labelField, o.loc)
		if why != "" {
			st.skipped++
			fmt.Fprintf(stderr, "line %d: skipped (%s)\n", row.line, why)
			continue
		}
		if !meta.expiresAt.IsZero() && !now.Before(meta.expiresAt) && !o.includeExpired {
			st.expired++
			fmt.Fprintf(stderr, "line %d: skipped (expired)\n", row.line)
			continue
		}
		seq++
		if o.dryRun {
			st.imported++
			continue
		}
		body := map[string]any{"pool": o.pool, "name": fmt.Sprintf("%s-%03d", o.namePrefix, seq), "secret": o.prefix + secret}
		if o.weight > 0 {
			body["weight"] = o.weight
		}
		if o.maxInFlight > 0 {
			body["max_in_flight"] = o.maxInFlight
		}
		if !meta.expiresAt.IsZero() {
			body["expires_at"] = meta.expiresAt.Format(time.RFC3339)
		}
		if meta.label != "" {
			body["label"] = meta.label
		}
		raw, _ := json.Marshal(body) // 只含字符串与整数，必然可以序列化
		status, resp, err := adminRequest(ctx, client, http.MethodPost, o.base+"/admin/api/keys", o.token, raw)
		switch {
		case err != nil:
			st.failed++
			fmt.Fprintf(stderr, "line %d: failed (%s)\n", row.line, redactToken(err.Error(), o.token))
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return &st, 1
			}
		case status == http.StatusCreated:
			st.imported++
			if onKey != nil {
				onKey(createdKeyID(resp))
			}
		case status == http.StatusConflict && o.update:
			if why := updateExisting(ctx, client, o.base, o.token, resp, meta); why != "" {
				st.failed++
				fmt.Fprintf(stderr, "line %d: failed (%s)\n", row.line, redactToken(why, o.token))
				if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return &st, 1
				}
			} else {
				st.updated++
				if onKey != nil {
					onKey(conflictKeyID(resp))
				}
			}
		case status == http.StatusConflict:
			st.duplicate++
			if onKey != nil {
				onKey(conflictKeyID(resp))
			}
		default:
			st.failed++
			fmt.Fprintf(stderr, "line %d: failed (HTTP %d: %s)\n", row.line, status, apiErrorMessage(resp))
		}
	}
	if st.failed > 0 {
		return &st, 1
	}
	return &st, 0
}

// keyNameSeq 解析 "<prefix>-<数字>" 形式的名称中的编号。
func keyNameSeq(name, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(name, prefix+"-")
	if !ok || rest == "" || strings.Trim(rest, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// createdKeyID 取 201 响应中的 key.id；解析失败返回 0。
func createdKeyID(body []byte) int64 {
	var r struct {
		Key struct {
			ID int64 `json:"id"`
		} `json:"key"`
	}
	_ = json.Unmarshal(body, &r) // 失败时为 0，调用方忽略
	return r.Key.ID
}

// conflictKeyID 取 409 响应中已存在 Key 的 id（T62）；没有时返回 0。
func conflictKeyID(body []byte) int64 {
	var e struct {
		Error struct {
			KeyID int64 `json:"key_id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e) // 失败时为 0，调用方忽略
	return e.Error.KeyID
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
