// Command gateway 是 Universal AI Gateway 的进程入口（spec/03 §1）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// version 在构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain 解析子命令并返回退出码，便于测试。
func realMain(args []string, stdout, stderr io.Writer) int {
	return realMainIO(args, os.Stdin, stdout, stderr, os.Getenv)
}

// realMainIO 与 realMain 相同，但标准输入与环境变量也可注入（客户端子命令的测试使用）。
func realMainIO(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "check-config":
		cfg, _, code := loadConfig(args[1:], stderr)
		if cfg == nil {
			return code
		}
		fmt.Fprintln(stdout, "config ok")
		return 0
	case "tools", "call", "mcp":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		switch args[0] {
		case "tools":
			return cmdTools(ctx, args[1:], getenv, stdout, stderr)
		case "call":
			return cmdCall(ctx, args[1:], getenv, stdin, stdout, stderr)
		default:
			return cmdMCP(ctx, getenv, stdin, stdout, stderr)
		}
	case "reload":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return cmdReload(ctx, args[1:], getenv, stdout, stderr)
	case "serve":
		cfg, path, code := loadConfig(args[1:], stderr)
		if cfg == nil {
			return code
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := serveLoop(ctx, cfg, runOptions{Stderr: stderr, ConfigPath: path}); err != nil {
			fmt.Fprintln(stderr, "gateway:", err)
			return 1
		}
		return 0
	default:
		usage(stderr)
		return 2
	}
}

// loadConfig 解析 --config 并加载配置，返回配置、文件的绝对路径（供重载重新读取，HR-01）与退出码。
func loadConfig(args []string, stderr io.Writer) (*config.Config, string, int) {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "gateway.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return nil, "", 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, "", 1
	}
	abs, err := filepath.Abs(*path)
	if err != nil {
		abs = *path // 取绝对路径失败时沿用原路径：只影响重载，不影响启动
	}
	return cfg, abs, 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  gateway serve        --config <path>   start the gateway
  gateway check-config --config <path>   validate the config file
  gateway version                        print version

client commands (talk to a running gateway; env UAIG_URL, UAIG_TOKEN):
  gateway tools [--json]                       list the tools available to this token
  gateway call <tool> [--args JSON|-] [--json] call a tool and print its result
  gateway mcp                                  run an MCP server on stdio (for Claude Code, Cursor, ...)

admin commands (env UAIG_ADMIN_URL, UAIG_ADMIN_TOKEN):
  gateway reload [--dry-run] [--json]          re-read the config file of the running gateway`)
}
