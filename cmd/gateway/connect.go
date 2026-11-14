package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

// `gateway connect <cli>`（T73，spec/14 §7，CN-01 ~ CN-08）：输出或写入各 AI CLI 的模型后端、MCP 配置，
// 并安装 use-gateway 技能。Token 永远不写进文件，只引用环境变量 UAIG_TOKEN。

//go:embed skills/use-gateway
var skillFS embed.FS

const (
	connectProvider = "uaig"
	tomlBegin       = "# >>> uaig (gateway connect) >>>"
	tomlEnd         = "# <<< uaig <<<"
	modelHolder     = "<服务名>"
)

var connectCLIs = []string{"claude", "codex", "opencode", "pi"}

// connectEnv 是 connect 运行时的环境（测试注入）。
type connectEnv struct {
	getenv  func(string) string
	home    string
	cwd     string
	models  func(ctx context.Context, base, token string) ([]string, error) // nil = 不查询
	version func() string                                                   // opencode 版本；nil = 不检测
}

// connectFile 是要写入的一个文件。
type connectFile struct {
	path  string
	kind  string         // "json" | "toml"
	set   map[string]any // json：键路径（用 . 分隔）→ 值
	order []string       // json：set 的键按此顺序处理
	block string         // toml：标记块内容
	note  string         // 说明
}

type connectPlan struct {
	files  []connectFile
	skill  string   // 技能安装目录（use-gateway 的父目录）
	notes  []string // 只打印、不写入的步骤
	verify []string // 验证命令
}

func cmdConnect(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "gateway: cannot find the home directory:", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		return 1
	}
	return runConnect(ctx, args, connectEnv{getenv: getenv, home: home, cwd: cwd, models: fetchModels, version: opencodeVersion}, stdout, stderr)
}

func runConnect(ctx context.Context, args []string, env connectEnv, stdout, stderr io.Writer) int {
	usage := "usage: gateway connect <claude|codex|opencode|pi|list> [--scope project|user] [--write] [--model NAME] [--opencode-schema v1|v2] [--pi-api openai-completions|anthropic-messages]"
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cli := args[0]
	if cli == "list" {
		for _, c := range connectCLIs {
			fmt.Fprintf(stdout, "%-9s %s\n", c, connectSummary(c))
		}
		return 0
	}
	if !contains(connectCLIs, cli) {
		fmt.Fprintf(stderr, "gateway: unknown CLI %q (claude, codex, opencode, pi)\n", cli)
		return 2
	}
	fset := flag.NewFlagSet("connect", flag.ContinueOnError)
	fset.SetOutput(stderr)
	scope := fset.String("scope", "project", "project (this directory) or user (your home directory)")
	write := fset.Bool("write", false, "write the files and install the skill (default: only print)")
	model := fset.String("model", "", "default service name (default: the first one the gateway lists)")
	ocSchema := fset.String("opencode-schema", "", "opencode config schema: v1 or v2 (default: detect)")
	piAPI := fset.String("pi-api", "openai-completions", "pi API type: openai-completions or anthropic-messages")
	if err := fset.Parse(args[1:]); err != nil || fset.NArg() > 0 {
		if err == nil {
			fmt.Fprintln(stderr, usage)
		}
		return 2
	}
	if *scope != "project" && *scope != "user" {
		fmt.Fprintln(stderr, "gateway: --scope must be project or user")
		return 2
	}
	if *piAPI != "openai-completions" && *piAPI != "anthropic-messages" {
		fmt.Fprintln(stderr, "gateway: --pi-api must be openai-completions or anthropic-messages")
		return 2
	}
	base := strings.TrimRight(strings.TrimSpace(env.getenv(envURL)), "/")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	token := strings.TrimSpace(env.getenv(envToken))
	var models []string
	if env.models != nil && token != "" {
		ms, err := env.models(ctx, base, token)
		if err != nil {
			fmt.Fprintln(stderr, "gateway: could not list services:", redactToken(err.Error(), token))
		}
		models = ms
	}
	if token == "" {
		fmt.Fprintln(stderr, "gateway: "+envToken+" is not set; using a placeholder for the service names")
	}
	if len(models) == 0 {
		models = []string{modelHolder}
	}
	def := *model
	if def == "" {
		def = models[0]
	}
	schema := *ocSchema
	if cli == "opencode" && schema == "" {
		schema = "v2"
		if env.version != nil {
			if v := env.version(); strings.HasPrefix(v, "0.") || strings.HasPrefix(v, "1.") {
				schema = "v1"
			}
		}
	}
	if schema != "" && schema != "v1" && schema != "v2" {
		fmt.Fprintln(stderr, "gateway: --opencode-schema must be v1 or v2")
		return 2
	}

	plan := buildConnectPlan(cli, *scope, base, models, def, schema, *piAPI, env)
	if err := applyConnectPlan(plan, *write, stdout); err != nil {
		fmt.Fprintln(stderr, "gateway:", redactToken(err.Error(), token))
		return 1
	}
	return 0
}

func connectSummary(cli string) string {
	switch cli {
	case "claude":
		return "model backend: environment variables; MCP: .mcp.json; skill: .claude/skills"
	case "codex":
		return "model provider + MCP: .codex/config.toml; skill: .agents/skills"
	case "opencode":
		return "provider + MCP: opencode.json; skill: .agents/skills"
	default:
		return "provider: ~/.pi/agent/models.json; no MCP (use gateway call); skill: .agents/skills"
	}
}

// buildConnectPlan 实现 CN-02 ~ CN-05。
func buildConnectPlan(cli, scope, base string, models []string, def, ocSchema, piAPI string, env connectEnv) connectPlan {
	root := env.cwd
	if scope == "user" {
		root = env.home
	}
	agentsSkills := filepath.Join(root, ".agents", "skills")
	var p connectPlan
	modelMap := func() map[string]any {
		m := map[string]any{}
		for _, id := range models {
			m[id] = map[string]any{"name": id}
		}
		return m
	}
	switch cli {
	case "claude":
		p.skill = filepath.Join(root, ".claude", "skills")
		if scope == "user" {
			p.skill = filepath.Join(userDir(env, "CLAUDE_CONFIG_DIR", ".claude"), "skills")
		}
		mcpEntry := map[string]any{"type": "http", "url": base + "/mcp", "headers": map[string]any{"Authorization": "Bearer ${" + envToken + "}"}}
		if scope == "project" {
			p.files = append(p.files, connectFile{path: filepath.Join(root, ".mcp.json"), kind: "json",
				set: map[string]any{"mcpServers.uaig": mcpEntry}, order: []string{"mcpServers.uaig"}, note: "MCP server (the token comes from " + envToken + ")"})
		} else {
			p.notes = append(p.notes, "Add the MCP server for every project (single quotes keep ${"+envToken+"} unexpanded):\n  claude mcp add --scope user --transport http uaig "+base+"/mcp --header 'Authorization: Bearer ${"+envToken+"}'")
		}
		p.notes = append(p.notes, "Use the gateway as Claude Code's model backend (environment variables; never put the token in settings.json):\n"+
			setEnvLines(map[string]string{"ANTHROPIC_BASE_URL": base, "ANTHROPIC_MODEL": def}, "ANTHROPIC_AUTH_TOKEN"))
		p.verify = append(p.verify, "claude -p \"ping\"   (uses the service "+def+")")
	case "codex":
		p.skill = agentsSkills
		dir := filepath.Join(root, ".codex")
		if scope == "user" {
			dir = userDir(env, "CODEX_HOME", ".codex")
		}
		block := strings.Join([]string{
			"[model_providers.uaig]",
			`name = "Universal AI Gateway"`,
			`base_url = "` + base + `/v1"`,
			`env_key = "` + envToken + `"`,
			`wire_api = "responses"`,
			"",
			"[mcp_servers.uaig]",
			`url = "` + base + `/mcp"`,
			`bearer_token_env_var = "` + envToken + `"`,
		}, "\n")
		p.files = append(p.files, connectFile{path: filepath.Join(dir, "config.toml"), kind: "toml", block: block, note: "model provider and MCP server"})
		p.notes = append(p.notes, "Select the provider: put these two lines at the TOP of "+filepath.Join(dir, "config.toml")+" (before any [table]):\n  model_provider = \"uaig\"\n  model = \""+def+"\"\nor run: codex -c model_provider=uaig -m "+def)
		p.verify = append(p.verify, "codex exec -c model_provider=uaig -m "+def+" \"ping\"")
	case "opencode":
		p.skill = agentsSkills
		path := filepath.Join(root, "opencode.json")
		if scope == "user" {
			path = filepath.Join(userDir(env, "XDG_CONFIG_HOME", ".config"), "opencode", "opencode.json")
		}
		mcpEntry := map[string]any{"type": "remote", "url": base + "/mcp", "oauth": false, "enabled": true,
			"headers": map[string]any{"Authorization": "Bearer {env:" + envToken + "}"}}
		f := connectFile{path: path, kind: "json", note: "provider \"uaig\" and MCP server (schema " + ocSchema + ")"}
		if ocSchema == "v1" {
			f.set = map[string]any{
				"$schema": "https://opencode.ai/config.json",
				"provider.uaig": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "Universal AI Gateway",
					"options": map[string]any{"baseURL": base + "/v1", "apiKey": "{env:" + envToken + "}"}, "models": modelMap()},
				"mcp.uaig": mcpEntry,
			}
			f.order = []string{"$schema", "provider.uaig", "mcp.uaig"}
		} else {
			f.set = map[string]any{
				"$schema": "https://opencode.ai/config.json",
				"providers.uaig": map[string]any{"name": "Universal AI Gateway", "env": []any{envToken},
					"package":  "@opencode/ai/providers/openai-compatible",
					"settings": map[string]any{"baseURL": base + "/v1", "apiKey": "{env:" + envToken + "}"}, "models": modelMap()},
				"mcp.servers.uaig": mcpEntry,
			}
			f.order = []string{"$schema", "providers.uaig", "mcp.servers.uaig"}
		}
		p.files = append(p.files, f)
		p.notes = append(p.notes, "Pick the model in opencode as uaig/"+def+" (or set \"model\": \"uaig/"+def+"\" in "+path+")")
		p.verify = append(p.verify, "opencode run -m uaig/"+def+" \"ping\"")
	case "pi":
		p.skill = agentsSkills
		pbase := base + "/v1"
		if piAPI == "anthropic-messages" {
			pbase = base
		}
		var list []any
		for _, id := range models {
			list = append(list, map[string]any{"id": id})
		}
		p.files = append(p.files, connectFile{path: filepath.Join(userDir(env, "PI_CODING_AGENT_DIR", filepath.Join(".pi", "agent")), "models.json"), kind: "json",
			set:   map[string]any{"providers.uaig": map[string]any{"baseUrl": pbase, "api": piAPI, "apiKey": "$" + envToken, "models": list}},
			order: []string{"providers.uaig"}, note: "provider \"uaig\" (pi reads models.json from its agent directory: PI_CODING_AGENT_DIR or ~/.pi/agent)"})
		p.notes = append(p.notes, "pi has no built-in MCP: the use-gateway skill tells it to use `gateway tools` and `gateway call` instead")
		p.verify = append(p.verify, "pi: /model → uaig/"+def+", then ask \"ping\"")
	}
	p.verify = append([]string{"curl -s " + base + "/v1/models -H \"Authorization: Bearer $" + envToken + "\""}, p.verify...)
	return p
}

// userDir 返回 CLI 的用户级配置目录：环境变量 name 有值时用它（各 CLI 自己也按它查找，CN-09），否则为 <home>/<def>。
func userDir(env connectEnv, name, def string) string {
	if v := strings.TrimSpace(env.getenv(name)); v != "" {
		return v
	}
	return filepath.Join(env.home, def)
}

// setEnvLines 给出设置环境变量的命令（CN-01：Token 只引用 UAIG_TOKEN）。
func setEnvLines(vals map[string]string, tokenVar string) string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	if runtime.GOOS == "windows" {
		for _, k := range keys {
			fmt.Fprintf(&b, "  setx %s \"%s\"\n", k, vals[k])
		}
		fmt.Fprintf(&b, "  setx %s \"%%%s%%\"      (run in cmd.exe after setting %s; reopen the terminal afterwards)", tokenVar, envToken, envToken)
	} else {
		for _, k := range keys {
			fmt.Fprintf(&b, "  export %s=%q\n", k, vals[k])
		}
		fmt.Fprintf(&b, "  export %s=\"$%s\"      (add these lines to your shell profile)", tokenVar, envToken)
	}
	return b.String()
}

// applyConnectPlan 打印计划；write 时写入文件并安装技能（CN-06、CN-07）。
func applyConnectPlan(p connectPlan, write bool, out io.Writer) error {
	for _, f := range p.files {
		old, err := os.ReadFile(f.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		exists := err == nil
		var next []byte
		var why string
		switch f.kind {
		case "toml":
			next = []byte(replaceTOMLBlock(string(old), f.block))
		default:
			next, why = mergeJSON(old, exists, f.set, f.order)
		}
		fmt.Fprintf(out, "== %s  (%s)\n", f.path, f.note)
		if next == nil { // JSONC 或无法解析：只打印片段
			fmt.Fprintf(out, "not changed: %s; merge this by hand:\n%s\n\n", why, snippetJSON(f.set, f.order))
			continue
		}
		if f.kind == "toml" {
			fmt.Fprintf(out, "%s\n%s\n%s\n\n", tomlBegin, f.block, tomlEnd)
		} else {
			fmt.Fprintf(out, "%s\n\n", snippetJSON(f.set, f.order))
		}
		if !write {
			continue
		}
		if exists && bytes.Equal(old, next) {
			fmt.Fprintln(out, "unchanged")
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return err
		}
		if exists {
			if err := config.WriteFileAtomic(f.path+".bak", old); err != nil {
				return fmt.Errorf("back up %s: %w", f.path, err)
			}
		}
		if err := config.WriteFileAtomic(f.path, next); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s%s\n\n", f.path, map[bool]string{true: " (backup: .bak)", false: ""}[exists])
	}
	if p.skill != "" {
		dst := filepath.Join(p.skill, "use-gateway")
		if write {
			if err := installSkill(dst); err != nil {
				return fmt.Errorf("install skill: %w", err)
			}
			fmt.Fprintf(out, "== installed skill use-gateway → %s\n\n", dst)
		} else {
			fmt.Fprintf(out, "== skill use-gateway → %s  (installed with --write)\n\n", dst)
		}
	}
	for _, n := range p.notes {
		fmt.Fprintf(out, "-- %s\n\n", n)
	}
	fmt.Fprintf(out, "-- Set %s for your user once (never written to any file):\n", envToken)
	if runtime.GOOS == "windows" {
		fmt.Fprintf(out, "  setx %s \"uai_live_...\"   (then reopen the terminal)\n\n", envToken)
	} else {
		fmt.Fprintf(out, "  export %s=uai_live_...   (in your shell profile)\n\n", envToken)
	}
	fmt.Fprintln(out, "-- Check:")
	for _, v := range p.verify {
		fmt.Fprintln(out, "  "+v)
	}
	if !write {
		fmt.Fprintln(out, "\n(nothing was written; run again with --write)")
	}
	return nil
}

// installSkill 用内置的 use-gateway 整目录覆盖 dst（CN-07），并在 SKILL.md 末尾记录版本。
func installSkill(dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return fs.WalkDir(skillFS, "skills/use-gateway", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "skills/use-gateway"), "/")
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := skillFS.ReadFile(p)
		if err != nil {
			return err
		}
		if rel == "SKILL.md" {
			b = append(bytes.TrimRight(b, "\n"), []byte("\n\n<!-- installed by `gateway connect` (gateway "+version+"); run it again to update -->\n")...)
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// replaceTOMLBlock 用标记块替换 s 中已有的块，没有就追加到末尾（CN-03、CN-06）。
func replaceTOMLBlock(s, block string) string {
	full := tomlBegin + "\n" + block + "\n" + tomlEnd + "\n"
	if i := strings.Index(s, tomlBegin); i >= 0 {
		if j := strings.Index(s[i:], tomlEnd); j >= 0 {
			end := i + j + len(tomlEnd)
			if end < len(s) && s[end] == '\n' {
				end++
			} else if end+1 < len(s) && s[end] == '\r' && s[end+1] == '\n' {
				end += 2
			}
			return s[:i] + full + s[end:]
		}
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if s != "" {
		s += "\n"
	}
	return s + full
}

// ---------------------------------------------------------------- 保留键顺序的 JSON

type jsonKV struct {
	key string
	val any
}

// jsonObj 是保持键顺序的 JSON 对象。
type jsonObj struct{ kv []jsonKV }

func (o *jsonObj) get(k string) (any, bool) {
	for _, e := range o.kv {
		if e.key == k {
			return e.val, true
		}
	}
	return nil, false
}

func (o *jsonObj) set(k string, v any) {
	for i, e := range o.kv {
		if e.key == k {
			o.kv[i].val = v
			return
		}
	}
	o.kv = append(o.kv, jsonKV{k, v})
}

func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &jsonObj{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string) // 对象的键必然是字符串
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				o.kv = append(o.kv, jsonKV{k, v})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			var arr []any
			for dec.More() {
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			if arr == nil {
				arr = []any{}
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return tok, nil // string、json.Number、bool、nil
	}
}

func encodeOrdered(b *bytes.Buffer, v any, indent string) {
	switch t := v.(type) {
	case *jsonObj:
		if len(t.kv) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, e := range t.kv {
			k, _ := json.Marshal(e.key) // 字符串必然可以序列化
			b.WriteString(indent + "  " + string(k) + ": ")
			encodeOrdered(b, e.val, indent+"  ")
			if i < len(t.kv)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "}")
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range t {
			b.WriteString(indent + "  ")
			encodeOrdered(b, e, indent+"  ")
			if i < len(t)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "]")
	case map[string]any: // 生成的值：按键名排序
		o := &jsonObj{}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o.kv = append(o.kv, jsonKV{k, t[k]})
		}
		encodeOrdered(b, o, indent)
	case json.Number:
		b.WriteString(t.String())
	default:
		raw, _ := json.Marshal(t) // string、bool、nil、float：必然可以序列化
		b.Write(raw)
	}
}

// mergeJSON 把 set（键路径 → 值）写进 old（CN-06）：只替换这些路径上的值，其他内容与键顺序保持不变。
// old 不是标准 JSON（含注释的 JSONC 或无法解析）时返回 nil 与原因。
func mergeJSON(old []byte, exists bool, set map[string]any, order []string) ([]byte, string) {
	root := &jsonObj{}
	if exists && len(bytes.TrimSpace(old)) > 0 {
		if !json.Valid(old) {
			return nil, "the file is not plain JSON (comments or a syntax error)"
		}
		dec := json.NewDecoder(bytes.NewReader(old))
		dec.UseNumber()
		v, err := decodeOrdered(dec)
		if err != nil {
			return nil, "the file cannot be parsed"
		}
		o, ok := v.(*jsonObj)
		if !ok {
			return nil, "the top level is not an object"
		}
		root = o
	}
	for _, path := range order {
		keys := strings.Split(path, ".")
		if path == "$schema" {
			keys = []string{"$schema"}
			if _, ok := root.get("$schema"); ok {
				continue // 已有的 $schema 不覆盖
			}
		}
		cur := root
		for _, k := range keys[:len(keys)-1] {
			next, ok := cur.get(k)
			o, isObj := next.(*jsonObj)
			if !ok || !isObj {
				o = &jsonObj{}
				cur.set(k, o)
			}
			cur = o
		}
		cur.set(keys[len(keys)-1], set[path])
	}
	var b bytes.Buffer
	encodeOrdered(&b, root, "")
	b.WriteString("\n")
	return b.Bytes(), ""
}

// snippetJSON 是要合并的内容（用于打印）。
func snippetJSON(set map[string]any, order []string) string {
	out, _ := mergeJSON(nil, false, set, order)
	return strings.TrimRight(string(out), "\n")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 查询服务名

var availableModels = regexp.MustCompile(`(?m)Available models: (.+)$`)

// fetchModels 返回可以当作大模型使用的服务名：优先取 ask_model 工具说明中的「Available models」，
// 取不到时用 /v1/models 的全部服务名（CN-02 ~ CN-05 的模型列表）。
func fetchModels(ctx context.Context, base, token string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := &mcp.Client{URL: base + "/mcp", Token: token, HTTP: newCLIHTTPClient()}
	if tools, err := c.ListTools(ctx); err == nil {
		for _, t := range tools {
			if t.Name == "ask_model" {
				if m := availableModels.FindStringSubmatch(t.Description); m != nil {
					return splitModels(m[1]), nil
				}
			}
		}
		for _, t := range tools {
			if t.Name == mcp.CatalogSearchTool { // 目录模式：查看 ask_model 的完整说明
				res, err := c.CallTool(ctx, "gateway_describe_tool", json.RawMessage(`{"name":"ask_model"}`))
				if err == nil && !res.IsError && len(res.Content) > 0 {
					if m := availableModels.FindStringSubmatch(res.Content[0].Text); m != nil {
						return splitModels(strings.TrimSuffix(strings.TrimSpace(m[1]), `"`)), nil
					}
				}
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := newCLIHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: HTTP %d", resp.StatusCode)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range v.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

func splitModels(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// opencodeVersion 返回 `opencode --version` 的输出；没有安装时为空。
func opencodeVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "opencode", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
}
