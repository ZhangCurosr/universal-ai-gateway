package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/audit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/httpapi"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ratelimit"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
)

// validateGeneration 是启动前的完整检查（T76，CK-01）：构造一代（注册表、价目单位、接口声明、内核）
// 并检查告警推送地址文件。serve 启动、配置重载之外的 check-config 与网页编辑（T72）共用它。
func validateGeneration(cfg *config.Config, gd genDeps, prev *generation) error {
	if err := checkWebhookFile(cfg); err != nil {
		return err
	}
	_, err := buildGeneration(cfg, gd, prev)
	return err
}

// memoryGenDeps 返回只在内存中的依赖（CK-02）：不打开数据库、不读主密钥、不监听端口。
func memoryGenDeps(cfg *config.Config) genDeps {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return genDeps{
		mem:       ledger.NewMemory(ledger.MemoryConfig{EventQueueCapacity: cfg.Ledger.EventQueueCapacity, AdmissionHighWatermark: cfg.Ledger.AdmissionHighWatermark}),
		sched:     scheduler.New(scheduler.PoolsFromConfig(cfg.Pools), scheduler.Options{Logger: logger}),
		auditPipe: audit.NewPipeline(cfg.Audit.QueueCapacity),
		index:     auth.NewIndex(),
		pepper:    make([]byte, 32),
		limiter:   ratelimit.New(time.Now),
		ipLimiter: ratelimit.New(time.Now),
		overload:  httpapi.NewOverloadGuard(cfg.Limits.MaxInFlightRequests, cfg.Limits.MaxBufferedBodyBytes, cfg.Limits.MaxRequestBodyBytes),
		budget:    &responseBudget{limit: cfg.Limits.MaxBufferedBodyBytes},
		logger:    logger,
	}
}

// cmdCheckConfig 实现 `gateway check-config --config <path> [--json]`（CK-01 ~ CK-04）。
func cmdCheckConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "gateway.yaml", "path to config file")
	asJSON := fs.Bool("json", false, `print {"ok": bool, "error": "..."}`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	err := checkConfigFile(*path)
	if *asJSON {
		out := map[string]any{"ok": err == nil}
		if err != nil {
			out["error"] = err.Error()
		}
		b, _ := json.Marshal(out) // 固定结构，必然可以序列化
		fmt.Fprintln(stdout, string(b))
	} else if err != nil {
		fmt.Fprintln(stderr, err)
	} else {
		fmt.Fprintln(stdout, "config ok")
	}
	if err != nil {
		return 1
	}
	return 0
}

func checkConfigFile(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	return validateGeneration(cfg, memoryGenDeps(cfg), nil)
}
