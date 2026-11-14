package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/admin"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// configEntryEditor 实现网页端管理池与服务的写入（T72，PE-02 ~ PE-06）。
type configEntryEditor struct {
	rl *configReloader
	mu sync.Mutex
}

func (e *configEntryEditor) deps() *admin.ConfigEditDeps {
	if e.rl.path == "" {
		return nil
	}
	return &admin.ConfigEditDeps{Load: e.load, Apply: e.apply, Protocols: e.protocols}
}

func (e *configEntryEditor) load() ([]byte, bool, error) {
	data, err := readConfigFile(e.rl.path)
	if err != nil {
		return nil, false, err
	}
	return data, (&shareEditor{rl: e.rl}).writable(), nil
}

func readConfigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, config.MaxConfigFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > config.MaxConfigFileBytes {
		return nil, fmt.Errorf("the config file is larger than %d bytes", config.MaxConfigFileBytes)
	}
	return data, nil
}

// protocols 列出可选的协议及其计量单位（rest 的单位包含当前配置中 api 声明用到的单位）。
func (e *configEntryEditor) protocols() []admin.ProtocolInfo {
	g := e.rl.gen.Load()
	var out []admin.ProtocolInfo
	for _, p := range editableProtocols {
		info := admin.ProtocolInfo{Name: string(p), Units: []string{}, DefaultBaseURL: defaultBaseURLs[p]}
		if g != nil {
			if eg, ok := g.registry.Egress(p); ok {
				info.Units = append(info.Units, eg.Units()...)
			}
		}
		sort.Strings(info.Units)
		out = append(out, info)
	}
	return out
}

func (e *configEntryEditor) apply(ctx context.Context, actor int64, version string, edit func([]byte) ([]byte, error), check bool, what string) (int, any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path := e.rl.path
	if !(&shareEditor{rl: e.rl}).writable() {
		return shareError(http.StatusConflict, "conflict", "the config file cannot be edited from the web page")
	}
	data, err := readConfigFile(path)
	if err != nil {
		return shareError(http.StatusInternalServerError, "internal", "could not read the config file")
	}
	if admin.ConfigVersion(data) != version { // PE-02
		return shareError(http.StatusConflict, "conflict", "the config file was changed since this page loaded it; reload the page and try again")
	}
	next, err := edit(data)
	if err != nil {
		return shareError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	cfg, err := config.Parse(next) // PE-03：配置规则
	if err != nil {
		return shareError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if err := validateGeneration(cfg, e.rl.gd, e.rl.gen.Load()); err != nil { // PE-03：与启动、重载相同的完整检查（T76）
		return shareError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if check {
		return http.StatusOK, map[string]any{"ok": true, "version": version}
	}
	if err := config.WriteFileAtomic(path+".bak", data); err != nil {
		return shareError(http.StatusInternalServerError, "internal", "could not back up the config file")
	}
	if err := config.WriteFileAtomic(path, next); err != nil {
		return shareError(http.StatusInternalServerError, "internal", "could not write the config file")
	}
	e.rl.logger.Info("config edited by admin", "change", what, "actor", actor) // PE-06
	status, body := e.rl.Reload(ctx, actor, false)
	if status >= http.StatusBadRequest {
		if rerr := config.WriteFileAtomic(path, data); rerr != nil {
			e.rl.logger.Error("restore config file after a failed reload", "err", rerr)
		} else {
			e.rl.logger.Warn("config file restored after a failed reload", "change", what)
		}
		return status, body
	}
	return status, map[string]any{"version": admin.ConfigVersion(next), "reload": body}
}
