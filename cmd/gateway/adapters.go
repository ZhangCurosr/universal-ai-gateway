package main

import (
	"fmt"
	"log/slog"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/gemini"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/rest"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/translate"
)

// adapterRegistrars 是全部协议适配器的注册函数。
//
// 这是整个仓库中唯一需要为新协议修改的非适配器文件（spec/08 §6 扩展指南第 4 步）：
// 新增协议 = 新建 internal/adapter/<name> 包 + 在这里加一行。
// rest 需要配置中的接口声明（RS-40），由 newRegistry 单独注册。
var adapterRegistrars = []func(*protocol.Registry) error{
	anthropic.Register,
	gemini.Register,
	mineru.Register,
	openai.Register,
	openairesp.Register,
	tavily.Register,
}

// newRegistry 注册全部适配器并封存注册表。
func newRegistry(cfg *config.Config, log *slog.Logger) (*protocol.Registry, error) {
	tools, err := restToolDefs(cfg)
	if err != nil {
		return nil, err
	}
	apis, err := restAPIDefs(cfg)
	if err != nil {
		return nil, err
	}
	opts := translate.Options{}
	if cfg != nil {
		opts.DropUnknownFields = (cfg.Translation.UnknownFields == "drop")
		opts.Logger = log
	}
	reg := protocol.NewRegistry()
	for _, register := range adapterRegistrars {
		if err := register(reg); err != nil {
			return nil, err
		}
	}
	if err := rest.RegisterWithOptions(reg, rest.Options{Logger: log, Tools: tools, APIs: apis}); err != nil {
		return nil, err
	}
	if err := translate.Registrar(opts)(reg); err != nil {
		return nil, err
	}
	if err := reg.Seal(); err != nil {
		return nil, err
	}
	// CAT-01、CFG-19：目录工具名保留；置顶工具必须存在
	var pinned []string
	if cfg != nil {
		pinned = cfg.MCP.PinnedTools
	}
	if err := mcp.CheckToolConfig(reg, pinned); err != nil {
		return nil, err
	}
	return reg, nil
}

// restToolDefs 把配置中的接口声明转换为 rest.ToolDef（CFG-17），并检查 CFG-18：
// 只有 rest 池上的服务可以声明接口。
func restToolDefs(cfg *config.Config) ([]rest.ToolDef, error) {
	if cfg == nil {
		return nil, nil
	}
	poolProto := make(map[string]string, len(cfg.Pools))
	for _, p := range cfg.Pools {
		poolProto[p.Name] = p.Protocol
	}
	var defs []rest.ToolDef
	for i, m := range cfg.Models {
		if len(m.Tools) == 0 {
			continue
		}
		if proto := poolProto[m.Pool]; proto != string(rest.Protocol) {
			return nil, fmt.Errorf("config: models[%d].tools: pool %q uses protocol %q, which does not support declared tools (only %q does)", i, m.Pool, proto, rest.Protocol)
		}
		for j, t := range m.Tools {
			schema, err := t.SchemaJSON()
			if err != nil {
				return nil, fmt.Errorf("config: models[%d].tools[%d].input_schema: %w", i, j, err)
			}
			def := rest.ToolDef{
				Service:     m.Name,
				Name:        t.Name,
				Title:       t.Title,
				Description: t.Description,
				Method:      t.Method,
				Path:        t.Path,
				InputSchema: schema,
				Idempotent:  t.Idempotent,
				ReadOnly:    t.IsReadOnly(),
			}
			if t.File != nil { // T69，FS-30
				def.FileArg, def.FileNameQuery = t.File.Arg, t.File.NameQuery
			}
			defs = append(defs, def)
		}
	}
	return defs, nil
}

// editableProtocols 是网页端新建池时可以选择的协议（T72，PE-01）。
var editableProtocols = []protocol.Protocol{anthropic.Protocol, openai.Protocol, openairesp.Protocol, gemini.Protocol, tavily.Protocol, mineru.Protocol, rest.Protocol}

// defaultBaseURLs 是各协议常用的上游地址（PE-01，只用作表单的缺省值）。
var defaultBaseURLs = map[protocol.Protocol]string{
	anthropic.Protocol: "https://api.anthropic.com",
	openai.Protocol:    "https://api.openai.com",
	gemini.Protocol:    "https://generativelanguage.googleapis.com",
	tavily.Protocol:    "https://api.tavily.com",
	mineru.Protocol:    "https://mineru.net",
}
