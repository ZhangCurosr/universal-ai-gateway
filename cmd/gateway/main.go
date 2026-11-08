// Command gateway 是 Universal AI Gateway 的进程入口（spec/03 §1）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "check-config":
		cfg, code := loadConfig(args[1:], stderr)
		if cfg == nil {
			return code
		}
		fmt.Fprintln(stdout, "config ok")
		return 0
	case "serve":
		cfg, code := loadConfig(args[1:], stderr)
		if cfg == nil {
			return code
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := run(ctx, cfg, runOptions{Stderr: stderr}); err != nil {
			fmt.Fprintln(stderr, "gateway:", err)
			return 1
		}
		return 0
	default:
		usage(stderr)
		return 2
	}
}

func loadConfig(args []string, stderr io.Writer) (*config.Config, int) {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "gateway.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return nil, 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, 1
	}
	return cfg, 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  gateway serve        --config <path>   start the gateway
  gateway check-config --config <path>   validate the config file
  gateway version                        print version`)
}
