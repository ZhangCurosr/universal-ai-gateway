package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

// `gateway doctor [--cli claude|codex|opencode|pi|all] [--json]`（T78，spec/14 §8，DR-01 ~ DR-07）：
// 按顺序诊断「为什么接不上」。只读：不改文件、不调用会计费的接口；输出中不出现 Token 的值。

const (
	drPass = "pass"
	drWarn = "warn"
	drFail = "fail"
	drSkip = "skip"
)

type drCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// doctorEnv 是 doctor 运行时的环境（测试注入）。
type doctorEnv struct {
	getenv func(string) string
	home   string
	cwd    string
	http   *http.Client
}

func cmdDoctor(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
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
	return runDoctor(ctx, args, doctorEnv{getenv: getenv, home: home, cwd: cwd, http: newCLIHTTPClient()}, stdout, stderr)
}

func runDoctor(ctx context.Context, args []string, env doctorEnv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cli := fs.String("cli", "", "also check an AI CLI's config: claude, codex, opencode, pi or all")
	asJSON := fs.Bool("json", false, `print {"checks": [...], "ok": bool}`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gateway doctor [--cli claude|codex|opencode|pi|all] [--json]")
		return 2
	}
	var clis []string
	switch {
	case *cli == "":
	case *cli == "all":
		clis = connectCLIs
	case contains(connectCLIs, *cli):
		clis = []string{*cli}
	default:
		fmt.Fprintf(stderr, "gateway: unknown CLI %q (claude, codex, opencode, pi, all)\n", *cli)
		return 2
	}

	token := strings.TrimSpace(env.getenv(envToken))
	checks := doctorChecks(ctx, env, clis)
	ok := true
	for i := range checks {
		checks[i].Message = redactToken(checks[i].Message, token)
		checks[i].Fix = redactToken(checks[i].Fix, token)
		if checks[i].Status == drFail {
			ok = false
		}
	}
	if *asJSON {
		b, err := json.Marshal(map[string]any{"checks": checks, "ok": ok})
		if err != nil {
			fmt.Fprintln(stderr, "gateway:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(b))
	} else {
		for _, c := range checks {
			parts := strings.Split(c.Message, "; ") // 一项有多条发现时（如 DR-05）每条一行
			fmt.Fprintf(stdout, "%-4s %-13s %s\n", strings.ToUpper(c.Status), c.ID, parts[0])
			for _, p := range parts[1:] {
				fmt.Fprintf(stdout, "     %-13s %s\n", "", p)
			}
			if c.Fix != "" {
				fmt.Fprintf(stdout, "     %-13s fix:\n", "")
				for _, l := range strings.Split(c.Fix, "\n") {
					fmt.Fprintf(stdout, "     %-13s   %s\n", "", l)
				}
			}
		}
	}
	if !ok {
		return 1 // DR-07：有失败为 1，只有警告为 0
	}
	return 0
}

// doctorChecks 依次运行 DR-01 ~ DR-06；失败后依赖它的检查标为 skip。
func doctorChecks(ctx context.Context, env doctorEnv, clis []string) []drCheck {
	var out []drCheck
	base, token, c1 := checkEnv(env)
	out = append(out, c1)
	urlOK := base != ""
	tokenOK := strings.HasPrefix(token, "uai_live_")

	reach := drCheck{ID: "DR-02", Status: drSkip, Message: "skipped: UAIG_URL is not usable"}
	if urlOK {
		reach = checkHealth(ctx, env, base)
	}
	out = append(out, reach)

	models := drCheck{ID: "DR-03", Status: drSkip, Message: "skipped: the gateway is not reachable"}
	switch {
	case reach.Status == drFail || reach.Status == drSkip:
	case !tokenOK:
		models.Message = "skipped: UAIG_TOKEN is not usable"
	default:
		models = checkModels(ctx, env, base, token)
	}
	out = append(out, models)

	tools := drCheck{ID: "DR-04", Status: drSkip, Message: "skipped: DR-03 did not pass"}
	if models.Status == drPass || models.Status == drWarn {
		tools = checkMCP(ctx, env, base, token)
	}
	out = append(out, tools)

	cmpBase := base
	if cmpBase == "" {
		cmpBase = "http://127.0.0.1:8080"
	}
	var hard []string
	for _, c := range clis {
		chk, h := checkCLI(env, c, cmpBase, token)
		out = append(out, chk)
		hard = append(hard, h...)
	}
	if len(clis) > 0 {
		if len(hard) == 0 {
			out = append(out, drCheck{ID: "DR-06", Status: drPass, Message: "no token values written in the CLI config files"})
		} else {
			out = append(out, drCheck{ID: "DR-06", Status: drWarn,
				Message: "a token value is written in: " + strings.Join(hard, ", "),
				Fix:     "replace it with a reference to " + envToken + " (gateway connect <cli> --write writes the reference), then rotate the token"})
		}
	}
	return out
}

// setVarFix 是设置环境变量的提示（DR-01）。
func setVarFix(name, example string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("setx %s \"%s\"   (then open a new terminal)", name, example)
	}
	return fmt.Sprintf("export %s=\"%s\"   (add it to ~/.bashrc or ~/.zshrc to keep it)", name, example)
}

// checkEnv 实现 DR-01。返回可用的地址（不可用时为空）与 Token。
func checkEnv(env doctorEnv) (base, token string, c drCheck) {
	c = drCheck{ID: "DR-01", Status: drPass}
	raw := strings.TrimRight(strings.TrimSpace(env.getenv(envURL)), "/")
	token = strings.TrimSpace(env.getenv(envToken))
	var msgs, fixes []string
	switch {
	case raw == "":
		base = "http://127.0.0.1:8080"
		c.Status = drWarn
		msgs = append(msgs, envURL+" is not set (using "+base+")")
		fixes = append(fixes, setVarFix(envURL, "http://<gateway-host>:8080"))
	default:
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			c.Status = drFail
			msgs = append(msgs, envURL+" must look like http://host:port, got "+raw)
			fixes = append(fixes, setVarFix(envURL, "http://<gateway-host>:8080"))
		} else {
			base = raw
			msgs = append(msgs, envURL+"="+raw)
		}
	}
	switch {
	case token == "":
		c.Status = drFail
		msgs = append(msgs, envToken+" is not set")
		fixes = append(fixes, setVarFix(envToken, "uai_live_..."), "ask the gateway admin for a client token")
	case !strings.HasPrefix(token, "uai_live_"):
		c.Status = drFail
		msgs = append(msgs, envToken+" does not start with uai_live_ (is it an upstream key or the admin token file path?)")
		fixes = append(fixes, setVarFix(envToken, "uai_live_..."))
	default:
		msgs = append(msgs, envToken+" is set")
	}
	c.Message = strings.Join(msgs, "; ")
	c.Fix = strings.Join(fixes, "\n")
	return base, token, c
}

// checkHealth 实现 DR-02：GET /healthz（5 秒），区分拒绝连接、超时、DNS 失败。
func checkHealth(ctx context.Context, env doctorEnv, base string) drCheck {
	c := drCheck{ID: "DR-02"}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return drCheck{ID: "DR-02", Status: drFail, Message: err.Error()}
	}
	resp, err := env.http.Do(req)
	if err != nil {
		c.Status = drFail
		var dns *net.DNSError
		var ne net.Error
		switch {
		case errors.As(err, &dns):
			c.Message = "cannot resolve the host name in " + base
			c.Fix = "check the spelling of " + envURL + ", or use the gateway's IP address"
		case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused"):
			c.Message = "connection refused by " + base + ": nothing is listening there"
			c.Fix = "start the gateway (gateway serve --config <file>) or check the port in " + envURL
		case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout():
			c.Message = "no answer from " + base + " within 5s"
			c.Fix = "check that the address is right and that a firewall is not dropping the connection"
		default:
			c.Message = "cannot connect to " + base + ": " + err.Error()
		}
		if lanHost(base) {
			c.Fix += "\non the gateway machine: turn on LAN sharing (admin UI → LAN sharing) and allow the port through the firewall"
		}
		return c
	}
	defer resp.Body.Close() //nolint:errcheck // 只看状态码
	if resp.StatusCode != http.StatusOK {
		return drCheck{ID: "DR-02", Status: drFail, Message: fmt.Sprintf("%s/healthz returned HTTP %d: is this the gateway's data address?", base, resp.StatusCode),
			Fix: envURL + " must be the data address (data_listen), not the admin address"}
	}
	return drCheck{ID: "DR-02", Status: drPass, Message: "the gateway answers at " + base}
}

// lanHost 报告地址的主机是否为局域网 IP。
func lanHost(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// checkModels 实现 DR-03：GET /v1/models（不计费）。
func checkModels(ctx context.Context, env doctorEnv, base, token string) drCheck {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return drCheck{ID: "DR-03", Status: drFail, Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := env.http.Do(req)
	if err != nil {
		return drCheck{ID: "DR-03", Status: drFail, Message: "GET /v1/models: " + err.Error()}
	}
	defer resp.Body.Close() //nolint:errcheck // 只读
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return drCheck{ID: "DR-03", Status: drFail, Message: fmt.Sprintf("the gateway rejected the token (HTTP %d): it is wrong, rotated or disabled", resp.StatusCode),
			Fix: "ask the gateway admin to check this client (admin UI → Clients) or issue a new token"}
	default:
		return drCheck{ID: "DR-03", Status: drFail, Message: fmt.Sprintf("GET /v1/models returned HTTP %d", resp.StatusCode)}
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return drCheck{ID: "DR-03", Status: drFail, Message: "GET /v1/models: unexpected response body"}
	}
	if len(body.Data) == 0 {
		return drCheck{ID: "DR-03", Status: drFail, Message: "the token works but has no services (allowed_models is empty or lists nothing that exists)",
			Fix: "ask the gateway admin to allow services for this client"}
	}
	return drCheck{ID: "DR-03", Status: drPass, Message: fmt.Sprintf("the token is accepted; %d service(s) available", len(body.Data))}
}

// checkMCP 实现 DR-04：initialize + tools/list。
func checkMCP(ctx context.Context, env doctorEnv, base, token string) drCheck {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := &mcp.Client{URL: base + "/mcp", Token: token, HTTP: env.http}
	tools, err := c.ListTools(ctx)
	if err != nil {
		return drCheck{ID: "DR-04", Status: drFail, Message: "MCP " + base + "/mcp: " + err.Error()}
	}
	mode := "direct mode"
	for _, t := range tools {
		if t.Name == mcp.CatalogSearchTool {
			mode = "catalog mode: find tools with " + mcp.CatalogSearchTool
		}
	}
	if len(tools) == 0 {
		return drCheck{ID: "DR-04", Status: drWarn, Message: "MCP works but lists no tools for this token"}
	}
	return drCheck{ID: "DR-04", Status: drPass, Message: fmt.Sprintf("MCP works: %d tool(s), %s", len(tools), mode)}
}

var (
	tokenLiteral   = regexp.MustCompile(`uai_live_[A-Za-z0-9_\-]{8,}`)
	urlLiteral     = regexp.MustCompile(`https?://[A-Za-z0-9.\-\[\]:]+`)
	codexProvider  = regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"uaig"`)
	skillInstalled = regexp.MustCompile(`installed by ` + "`gateway connect`" + ` \(gateway ([^)]+)\)`)
)

// checkCLI 实现 DR-05（一个 CLI），并返回写死了 Token 的文件（DR-06）。
func checkCLI(env doctorEnv, cli, base, token string) (drCheck, []string) {
	id := "DR-05 " + cli
	fix := "gateway connect " + cli + " --write   (add --scope user for every project)"
	cenv := connectEnv{getenv: env.getenv, home: env.home, cwd: env.cwd}
	var files []string
	var skills []string
	for _, scope := range []string{"project", "user"} {
		p := buildConnectPlan(cli, scope, base, []string{modelHolder}, modelHolder, "v1", "openai-completions", cenv)
		for _, f := range p.files {
			if !contains(files, f.path) {
				files = append(files, f.path)
			}
		}
		if p.skill != "" && !contains(skills, p.skill) {
			skills = append(skills, p.skill)
		}
	}
	status := drPass
	worse := func(s string) {
		rank := map[string]int{drPass: 0, drWarn: 1, drFail: 2}
		if rank[s] > rank[status] {
			status = s
		}
	}
	var msgs, hard []string
	configured := 0
	for _, path := range files {
		b, err := readSmall(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			worse(drWarn)
			msgs = append(msgs, "cannot read "+path+": "+err.Error())
			continue
		}
		text := string(b)
		if tokenLiteral.MatchString(text) || token != "" && strings.Contains(text, token) {
			hard = append(hard, path)
		}
		if !strings.Contains(text, connectProvider) {
			continue // 文件存在但没有 uaig 条目
		}
		configured++
		switch {
		case strings.Contains(text, base):
			msgs = append(msgs, path+": address matches "+envURL)
		default:
			worse(drFail)
			var seen []string
			for _, u := range urlLiteral.FindAllString(text, -1) {
				if !strings.Contains(u, "opencode.ai") && !contains(seen, u) {
					seen = append(seen, u)
				}
			}
			msgs = append(msgs, path+": address "+strings.Join(seen, ", ")+" differs from "+envURL+"="+base)
		}
		if !strings.Contains(text, envToken) {
			worse(drWarn)
			msgs = append(msgs, path+": does not reference "+envToken)
		}
		if cli == "codex" && !codexProvider.MatchString(text) {
			worse(drWarn)
			msgs = append(msgs, path+`: model_provider = "uaig" is not set at the top (or pass -c model_provider=uaig)`)
		}
	}

	if cli == "claude" {
		envOK := true
		if v := strings.TrimRight(strings.TrimSpace(env.getenv("ANTHROPIC_BASE_URL")), "/"); v == "" {
			envOK = false
			msgs = append(msgs, "ANTHROPIC_BASE_URL is not set (Claude Code will not use the gateway as its model backend)")
		} else if v != base {
			worse(drFail)
			msgs = append(msgs, "ANTHROPIC_BASE_URL="+v+" differs from "+envURL+"="+base)
		} else {
			msgs = append(msgs, "ANTHROPIC_BASE_URL matches "+envURL)
		}
		switch at := strings.TrimSpace(env.getenv("ANTHROPIC_AUTH_TOKEN")); {
		case at == "":
			envOK = false
			msgs = append(msgs, "ANTHROPIC_AUTH_TOKEN is not set")
		case token != "" && at != token:
			worse(drWarn)
			msgs = append(msgs, "ANTHROPIC_AUTH_TOKEN differs from "+envToken)
		}
		if !envOK {
			if configured == 0 {
				worse(drFail)
			} else {
				worse(drWarn)
			}
		} else {
			configured++
		}
	}
	if configured == 0 {
		worse(drFail)
		msgs = append(msgs, "no gateway config found (looked at "+strings.Join(files, ", ")+")")
	}

	// 技能（CN-07 记录的版本）
	var skillMsg string
	for _, dir := range skills {
		b, err := readSmall(filepath.Join(dir, "use-gateway", "SKILL.md"))
		if err != nil {
			continue
		}
		m := skillInstalled.FindSubmatch(b)
		switch {
		case m == nil:
			skillMsg = "use-gateway skill found in " + dir
		case string(m[1]) != version:
			worse(drWarn)
			skillMsg = "use-gateway skill in " + dir + " is from gateway " + string(m[1]) + " (this gateway is " + version + ")"
		default:
			skillMsg = "use-gateway skill is installed (" + dir + ")"
		}
		break
	}
	if skillMsg == "" {
		worse(drWarn)
		skillMsg = "use-gateway skill is not installed (looked in " + strings.Join(skills, ", ") + ")"
	}
	msgs = append(msgs, skillMsg)

	c := drCheck{ID: id, Status: status, Message: strings.Join(msgs, "; ")}
	if status != drPass {
		c.Fix = fix
		if cli == "codex" {
			c.Fix += "\nselect the provider: put model_provider = \"uaig\" and model = \"<service>\" at the top of config.toml, or run codex -c model_provider=uaig"
		}
		if cli == "claude" {
			c.Fix += "\nthen set ANTHROPIC_BASE_URL to " + envURL + " and ANTHROPIC_AUTH_TOKEN to the value of " + envToken + " (gateway connect claude prints the commands)"
		}
	}
	return c, hard
}

// readSmall 读取一个不超过 1 MiB 的配置文件。
func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // 只读文件，关闭失败无可处理
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("file is larger than 1 MiB")
	}
	return b, nil
}
