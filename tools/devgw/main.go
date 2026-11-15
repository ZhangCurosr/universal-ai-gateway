// Command devgw 启动一个一次性的测试网关（T77）：编译 ./cmd/gateway 到临时目录、改写配置副本、
// 按需启动假上游、等网关就绪、把引导管理员 Token 与一个测试客户端的 Token 写进 0600 文件；
// Ctrl+C 时停止网关与假上游并删除目录。只给开发与实测用，不进入 gateway 程序与发布包。
//
//	go run ./tools/devgw --config <yaml> [--fake openai,anthropic,responses] [--dir <临时目录>] [--keep]
//
// 配置中 base_url 写成 fake:openai、fake:anthropic、fake:responses 的池指向对应的假上游。
// 终端只打印地址与 Token 文件路径，不打印 Token。
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
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const bootstrapPrefix = "BOOTSTRAP ADMIN TOKEN (shown once): "

type options struct {
	Config string
	Fakes  []string
	Dir    string
	Keep   bool
	Root   string // 仓库根目录（编译 ./cmd/gateway 的位置）
	// Ready 在网关就绪、Token 写好后调用（测试用）；之后照常等待 ctx 结束或停止文件。
	Ready func(s *session)
}

// session 是一次运行的结果，供 Ready 使用。
type session struct {
	Dir, DataURL, AdminURL string
	Fakes                  map[string]string
}

func main() {
	fs := flag.NewFlagSet("devgw", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.Config, "config", "", "gateway config to copy (required)")
	fakes := fs.String("fake", "", "fake upstreams to start: openai,anthropic,responses")
	fs.StringVar(&o.Dir, "dir", "", "working directory (must be empty or absent; default: a new temp dir)")
	fs.BoolVar(&o.Keep, "keep", false, "keep the directory on exit (logs, tokens)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if o.Config == "" {
		fmt.Fprintln(os.Stderr, "devgw: --config is required")
		os.Exit(2)
	}
	o.Fakes = splitList(*fakes)
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "devgw:", err)
		os.Exit(1)
	}
	o.Root = root
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "devgw:", err)
		stop()
		os.Exit(1)
	}
}

func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// prepareDir 返回工作目录：--dir 必须不存在或为空（退出时整个删除，不能误删别的文件）。
func prepareDir(dir string) (string, error) {
	if dir == "" {
		return os.MkdirTemp("", "devgw-")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return abs, os.MkdirAll(abs, 0o700)
	case err != nil:
		return "", err
	case len(entries) > 0:
		return "", fmt.Errorf("--dir %s is not empty (it is deleted on exit)", abs)
	}
	return abs, nil
}

func run(ctx context.Context, o options, stdout io.Writer) (err error) {
	orig, err := os.ReadFile(o.Config)
	if err != nil {
		return err
	}
	dir, err := prepareDir(o.Dir)
	if err != nil {
		return err
	}
	defer func() {
		if o.Keep {
			fmt.Fprintln(stdout, "devgw: kept", dir)
			return
		}
		if rmErr := removeAll(dir); rmErr != nil && err == nil {
			err = rmErr
		}
	}()

	// 假上游
	fakeLogFile, err := os.OpenFile(filepath.Join(dir, "fake.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer fakeLogFile.Close() //nolint:errcheck // 只写日志
	flog := &fakeLog{w: fakeLogFile}
	fakes := map[string]string{}
	for _, name := range o.Fakes {
		h, err := newFakeHandler(name, flog)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "devgw: fake", name+":", err)
			}
		}()
		defer srv.Close() //nolint:errcheck // 退出时关闭
		fakes[name] = "http://" + ln.Addr().String()
	}

	rw, err := rewriteConfig(orig, dir, fakes)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(cfgPath, rw.Data, 0o600); err != nil {
		return err
	}

	exe := filepath.Join(dir, "gateway")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	fmt.Fprintln(stdout, "devgw: building ./cmd/gateway ...")
	build := exec.CommandContext(ctx, "go", "build", "-o", exe, "./cmd/gateway")
	build.Dir = o.Root
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, out)
	}

	gw, err := startGateway(exe, cfgPath, dir)
	if err != nil {
		return err
	}
	defer gw.stop()

	adminToken, err := gw.waitReady(ctx, rw.DataAddr)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, tail(filepath.Join(dir, "gateway.log"), 20))
	}
	adminURL := "http://" + rw.AdminAddr
	clientToken, err := createClient(ctx, adminURL, adminToken)
	if err != nil {
		return err
	}
	// 池里没有 Key 时网关不会转发：给每个指向假上游的池加一个占位 Key（假上游不校验）。
	for _, pool := range rw.FakePools {
		body, err := json.Marshal(map[string]string{"pool": pool, "name": "devgw-fake-" + pool, "secret": "devgw-fake-key-" + pool})
		if err != nil {
			return err
		}
		if _, err := adminPost(ctx, adminURL, adminToken, "/admin/api/keys", body); err != nil {
			return fmt.Errorf("add key to pool %q: %w", pool, err)
		}
	}
	adminFile, clientFile := filepath.Join(dir, "admin.token"), filepath.Join(dir, "client.token")
	for p, tok := range map[string]string{adminFile: adminToken, clientFile: clientToken} {
		if err := os.WriteFile(p, []byte(tok), 0o600); err != nil {
			return err
		}
	}

	s := &session{Dir: dir, DataURL: "http://" + rw.DataAddr, AdminURL: adminURL, Fakes: fakes}
	printSummary(stdout, s, adminFile, clientFile)
	if o.Ready != nil {
		o.Ready(s)
	}
	// 后台运行时（AI 代理、Windows 上无法发送 Ctrl+C）用停止文件结束。
	stopFile := filepath.Join(dir, "stop")
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stdout, "\ndevgw: stopping")
			return nil
		case <-gw.done:
			return fmt.Errorf("gateway exited: %v\n%s", gw.waitErr, tail(filepath.Join(dir, "gateway.log"), 20))
		case <-poll.C:
			if _, err := os.Stat(stopFile); err == nil {
				fmt.Fprintln(stdout, "devgw: stop file found, stopping")
				return nil
			}
		}
	}
}

func printSummary(w io.Writer, s *session, adminFile, clientFile string) {
	q := func(p string) string { return "'" + filepath.ToSlash(p) + "'" }
	fmt.Fprintf(w, "devgw: gateway ready in %s\n", s.Dir)
	fmt.Fprintf(w, "  data plane:  %s\n  admin UI:    %s/admin/ui/\n", s.DataURL, s.AdminURL)
	names := make([]string, 0, len(s.Fakes))
	for n := range s.Fakes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  fake %-10s %s\n", n+":", s.Fakes[n])
	}
	fmt.Fprintf(w, "  admin token: %s\n  client token: %s\n  logs:        %s, %s\n",
		filepath.ToSlash(adminFile), filepath.ToSlash(clientFile),
		filepath.ToSlash(filepath.Join(s.Dir, "gateway.log")), filepath.ToSlash(filepath.Join(s.Dir, "fake.log")))
	fmt.Fprintln(w, "bash:")
	fmt.Fprintf(w, "  export UAIG_URL=%s UAIG_TOKEN=\"$(cat %s)\" UAIG_ADMIN_URL=%s UAIG_ADMIN_TOKEN=\"$(cat %s)\"\n", s.DataURL, q(clientFile), s.AdminURL, q(adminFile))
	fmt.Fprintln(w, "PowerShell:")
	fmt.Fprintf(w, "  $env:UAIG_URL='%s'; $env:UAIG_TOKEN=Get-Content %s; $env:UAIG_ADMIN_URL='%s'; $env:UAIG_ADMIN_TOKEN=Get-Content %s\n", s.DataURL, q(clientFile), s.AdminURL, q(adminFile))
	fmt.Fprintf(w, "Ctrl+C (or creating %s) stops the gateway and deletes the directory.\n", filepath.ToSlash(filepath.Join(s.Dir, "stop")))
}

// ---------------------------------------------------------------- 网关进程

type gateway struct {
	cmd     *exec.Cmd
	log     *os.File
	token   chan string
	done    chan struct{}
	waitErr error
	copied  sync.WaitGroup
}

// startGateway 启动 serve；输出写进 gateway.log，引导 Token 那一行换成提示（Token 只进 admin.token）。
func startGateway(exe, cfgPath, dir string) (*gateway, error) {
	logFile, err := os.OpenFile(filepath.Join(dir, "gateway.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	g := &gateway{log: logFile, token: make(chan string, 1), done: make(chan struct{})}
	cmd := exec.Command(exe, "serve", "--config", cfgPath)
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close() //nolint:errcheck // 出错路径的清理
		return nil, err
	}
	cmd.Stderr = cmd.Stdout // 两路合并，按行过滤
	if err := cmd.Start(); err != nil {
		logFile.Close() //nolint:errcheck // 出错路径的清理
		return nil, err
	}
	g.cmd = cmd
	g.copied.Add(1)
	go g.copyLog(out)
	go func() {
		g.copied.Wait()
		g.waitErr = cmd.Wait()
		close(g.done)
	}()
	return g, nil
}

func (g *gateway) copyLog(r io.Reader) {
	defer g.copied.Done()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if tok, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), bootstrapPrefix); ok {
			select {
			case g.token <- tok:
			default:
			}
			line = bootstrapPrefix + "<written to admin.token>\n"
		}
		if line != "" {
			if _, werr := g.log.WriteString(line); werr != nil {
				fmt.Fprintln(os.Stderr, "devgw: write gateway.log:", werr)
			}
		}
		if err != nil {
			return // EOF：进程退出
		}
	}
}

// waitReady 等引导 Token 与 /healthz 200（60 秒）。
func (g *gateway) waitReady(ctx context.Context, dataAddr string) (string, error) {
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	var token string
	client := &http.Client{Timeout: 2 * time.Second}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", errors.New("gateway not ready after 60s")
		case <-g.done:
			return "", fmt.Errorf("gateway exited: %v", g.waitErr)
		case token = <-g.token:
		case <-tick.C:
			if token == "" {
				continue
			}
			resp, err := client.Get("http://" + dataAddr + "/healthz")
			if err != nil {
				continue
			}
			resp.Body.Close() //nolint:errcheck // 只看状态码
			if resp.StatusCode == http.StatusOK {
				return token, nil
			}
		}
	}
}

// stop 停止网关：类 Unix 先发 SIGINT 等 5 秒；Windows 没有可用的信号，直接终止。
func (g *gateway) stop() {
	if runtime.GOOS != "windows" {
		if err := g.cmd.Process.Signal(os.Interrupt); err == nil {
			select {
			case <-g.done:
			case <-time.After(5 * time.Second):
			}
		}
	}
	select {
	case <-g.done:
	default:
		if err := g.cmd.Process.Kill(); err != nil {
			fmt.Fprintln(os.Stderr, "devgw: kill gateway:", err)
		}
		<-g.done
	}
	g.log.Close() //nolint:errcheck // 进程已退出
}

// createClient 创建一个不限额、不限速的测试客户端，返回其 Token。
func createClient(ctx context.Context, adminURL, adminToken string) (string, error) {
	raw, err := adminPost(ctx, adminURL, adminToken, "/admin/api/clients",
		[]byte(`{"name":"devgw-client","role":"user","quota_unlimited":true,"rpm_limit":0}`))
	if err != nil {
		return "", fmt.Errorf("create test client: %w", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", errors.New("create test client: no token in response")
	}
	return out.Token, nil
}

// adminPost 调用管理面接口，返回响应体（最多 1 MiB）；错误中不含响应体（可能带 Token）。
func adminPost(ctx context.Context, adminURL, adminToken, path string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, adminURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // 只读
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return raw, nil
}

// removeAll 删除目录；Windows 上进程刚退出时文件可能仍被占用，重试几次。
func removeAll(dir string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

// tail 返回日志最后 n 行（启动失败时帮助定位；引导 Token 已在写入时替换）。
func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
