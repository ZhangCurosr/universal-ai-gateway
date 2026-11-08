package main

import (
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/anthropic"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/mineru"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openai"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/openairesp"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/adapter/tavily"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// adapterRegistrars 是全部协议适配器的注册函数。
//
// 这是整个仓库中唯一需要为新协议修改的非适配器文件（spec/08 §6 扩展指南第 4 步）：
// 新增协议 = 新建 internal/adapter/<name> 包 + 在这里加一行。
var adapterRegistrars = []func(*protocol.Registry) error{
	anthropic.Register,
	mineru.Register,
	openai.Register,
	openairesp.Register,
	tavily.Register,
}

// newRegistry 注册全部适配器并封存注册表。
func newRegistry() (*protocol.Registry, error) {
	reg := protocol.NewRegistry()
	for _, register := range adapterRegistrars {
		if err := register(reg); err != nil {
			return nil, err
		}
	}
	if err := reg.Seal(); err != nil {
		return nil, err
	}
	return reg, nil
}
