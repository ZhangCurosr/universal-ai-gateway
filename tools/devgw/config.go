package main

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// rewritten 是改写后的配置与从中读出的地址。
type rewritten struct {
	Data      []byte
	DataAddr  string // 127.0.0.1:端口
	AdminAddr string
	FakePools []string // base_url 换成了假上游的池（按配置中的顺序）
}

// rewriteConfig 改写配置文本的副本（T77 §2.1 ~ 2.2）：data_dir、master_key_file 指向 dir；
// 两个监听地址只保留端口、主机改为 127.0.0.1；base_url 为 fake:<名字> 的池换成 fakes[名字]。
// 只替换这几处文本，其余内容（含注释）不变。
func rewriteConfig(data []byte, dir string, fakes map[string]string) (*rewritten, error) {
	items, err := config.ListItems(data, "pools")
	if err != nil {
		return nil, err
	}
	var fakePools []string
	for _, it := range items {
		f, err := config.ReadEntryFields("pools", it.Text)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", it.Name, err)
		}
		base, _ := f["base_url"].(string) // ReadEntryFields 总是给出字符串
		name, ok := strings.CutPrefix(base, "fake:")
		if !ok {
			continue
		}
		want, known := fakeProtocols[name]
		if !known {
			return nil, fmt.Errorf("pool %q: unknown fake upstream %q (want openai, anthropic or responses)", it.Name, base)
		}
		if p, _ := f["protocol"].(string); p != want {
			return nil, fmt.Errorf("pool %q: base_url %s needs protocol %s, not %q", it.Name, base, want, p)
		}
		url, started := fakes[name]
		if !started {
			return nil, fmt.Errorf("pool %q uses %s: add --fake %s", it.Name, base, name)
		}
		text, err := config.ApplyEntryFields("pools", it.Text, config.EntryFields{"base_url": url})
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", it.Name, err)
		}
		if data, err = config.ReplaceListItem(data, "pools", it.Name, text); err != nil {
			return nil, err
		}
		fakePools = append(fakePools, it.Name)
	}

	cfg, err := config.Parse(data) // 带默认值，得到监听地址
	if err != nil {
		return nil, err
	}
	out := &rewritten{}
	if out.DataAddr, err = loopback(cfg.Server.DataListen); err != nil {
		return nil, fmt.Errorf("server.data_listen: %w", err)
	}
	if out.AdminAddr, err = loopback(cfg.Server.AdminListen); err != nil {
		return nil, fmt.Errorf("server.admin_listen: %w", err)
	}
	set := []struct {
		key, value string
	}{
		{"data_listen", out.DataAddr},
		{"admin_listen", out.AdminAddr},
		{"data_dir", filepath.ToSlash(filepath.Join(dir, "data"))},
		{"master_key_file", filepath.ToSlash(filepath.Join(dir, "secrets", "master.key"))},
	}
	for _, s := range set {
		if data, _, err = config.SetScalar(data, []string{"server", s.key}, s.value); err != nil {
			return nil, fmt.Errorf("server.%s: %w", s.key, err)
		}
	}
	if _, err := config.Parse(data); err != nil {
		return nil, fmt.Errorf("rewritten config: %w", err)
	}
	out.FakePools = fakePools
	out.Data = data
	return out, nil
}

// loopback 把监听地址的主机换成 127.0.0.1（devgw 只在本机监听）。
func loopback(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort("127.0.0.1", port), nil
}
