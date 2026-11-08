package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// IngressOptions 是注册入站适配器时的选项。
type IngressOptions struct {
	// Priority 决定路由冲突时 Claims 的调用顺序：数值小的先问。
	Priority int
	// Fallback 表示路由冲突时所有 Claims 都返回 false 的请求归它处理，
	// 同时它的 WriteError 用于无法确定协议的错误（如 404、panic）。必须恰好有一个。
	Fallback bool
}

type ingressEntry struct {
	in   Ingress
	opts IngressOptions
}

// Registry 保存全部适配器。只在启动阶段（单 goroutine）写入，之后只读，因此无需加锁。
type Registry struct {
	ingress     map[Protocol]ingressEntry
	egress      map[Protocol]Egress
	translators map[[2]Protocol]Translator
	tools       map[string]ToolEntry // Seal 时由实现了 ToolSource 的入站适配器生成
	sealed      bool
}

// ToolEntry 是注册表中的一个工具及其所属的入站适配器。
type ToolEntry struct {
	Tool    Tool
	Ingress Ingress
	Source  ToolSource
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{
		ingress:     map[Protocol]ingressEntry{},
		egress:      map[Protocol]Egress{},
		translators: map[[2]Protocol]Translator{},
		tools:       map[string]ToolEntry{},
	}
}

// ErrSealed 表示注册表已封存，不能再注册。
var ErrSealed = errors.New("protocol: registry is sealed")

// RegisterIngress 注册入站适配器；协议名为空或重复返回错误。
func (r *Registry) RegisterIngress(in Ingress, opts IngressOptions) error {
	if r.sealed {
		return ErrSealed
	}
	p := in.Protocol()
	if p == "" {
		return errors.New("protocol: ingress with empty protocol name")
	}
	if _, dup := r.ingress[p]; dup {
		return fmt.Errorf("protocol: duplicate ingress %q", p)
	}
	r.ingress[p] = ingressEntry{in: in, opts: opts}
	return nil
}

// RegisterEgress 注册出站适配器。
func (r *Registry) RegisterEgress(e Egress) error {
	if r.sealed {
		return ErrSealed
	}
	p := e.Protocol()
	if p == "" {
		return errors.New("protocol: egress with empty protocol name")
	}
	if _, dup := r.egress[p]; dup {
		return fmt.Errorf("protocol: duplicate egress %q", p)
	}
	r.egress[p] = e
	return nil
}

// RegisterTranslator 注册跨协议转换器。From == To 视为错误（同协议必须透传）。
func (r *Registry) RegisterTranslator(t Translator) error {
	if r.sealed {
		return ErrSealed
	}
	if t.From() == t.To() {
		return fmt.Errorf("protocol: translator %q -> %q: same protocol must use passthrough", t.From(), t.To())
	}
	key := [2]Protocol{t.From(), t.To()}
	if _, dup := r.translators[key]; dup {
		return fmt.Errorf("protocol: duplicate translator %q -> %q", t.From(), t.To())
	}
	r.translators[key] = t
	return nil
}

// Seal 校验注册表并禁止后续注册。校验规则（REG-01 ~ REG-04）：
// 至少一个入站适配器；恰好一个 Fallback；每个入站适配器至少 1 个 RouteCall、至多 1 个 RouteCatalog，
// RouteCall 的 Op 非空且在本协议内唯一；同一协议内不得有重复的 (Method, Path)；
// 冲突的路由只允许发生在不同协议之间。
func (r *Registry) Seal() error {
	if len(r.ingress) == 0 {
		return errors.New("protocol: no ingress registered")
	}
	fallbacks := 0
	type routeKey struct{ method, path string }
	for p, e := range r.ingress {
		if e.opts.Fallback {
			fallbacks++
		}
		calls, catalogs := 0, 0
		seen := map[routeKey]bool{}
		ops := map[string]bool{}
		for _, rt := range e.in.Routes() {
			if rt.Method == "" || !strings.HasPrefix(rt.Path, "/") || rt.MaxBodyBytes < 0 {
				return fmt.Errorf("protocol: ingress %q: invalid route %+v", p, rt)
			}
			k := routeKey{rt.Method, rt.Path}
			if seen[k] {
				return fmt.Errorf("protocol: ingress %q: duplicate route %s %s", p, rt.Method, rt.Path)
			}
			seen[k] = true
			if rt.Body != BodyBuffered && rt.Body != BodyStream && rt.Body != BodyNone {
				return fmt.Errorf("protocol: ingress %q: route %s %s: unknown body mode %d", p, rt.Method, rt.Path, rt.Body)
			}
			switch rt.Kind {
			case RouteCall:
				calls++
				if rt.Op == "" || ops[rt.Op] {
					return fmt.Errorf("protocol: ingress %q: route %s %s: op must be non-empty and unique, got %q", p, rt.Method, rt.Path, rt.Op)
				}
				ops[rt.Op] = true
			case RouteCatalog:
				catalogs++
			default:
				return fmt.Errorf("protocol: ingress %q: unknown route kind %d", p, rt.Kind)
			}
		}
		if calls < 1 || catalogs > 1 {
			return fmt.Errorf("protocol: ingress %q must declare at least 1 call route and at most 1 catalog route", p)
		}
	}
	if fallbacks != 1 {
		return fmt.Errorf("protocol: exactly one fallback ingress required, got %d", fallbacks)
	}
	if err := r.indexTools(); err != nil {
		return err
	}
	r.sealed = true
	return nil
}

// indexTools 实现 REG-05：工具名合法且全局唯一；InputSchema 是 type 为 "object" 的 JSON Schema。
func (r *Registry) indexTools() error {
	for p, e := range r.ingress {
		src, ok := e.in.(ToolSource)
		if !ok {
			continue
		}
		for _, t := range src.Tools() {
			if !ToolNamePattern.MatchString(t.Name) {
				return fmt.Errorf("protocol: ingress %q: invalid tool name %q", p, t.Name)
			}
			if prev, dup := r.tools[t.Name]; dup {
				return fmt.Errorf("protocol: tool %q declared by both %q and %q", t.Name, prev.Ingress.Protocol(), p)
			}
			var schema struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil || schema.Type != "object" {
				return fmt.Errorf("protocol: tool %q: input schema must be a JSON Schema object with \"type\": \"object\"", t.Name)
			}
			if t.Description == "" {
				return fmt.Errorf("protocol: tool %q: description must not be empty", t.Name)
			}
			r.tools[t.Name] = ToolEntry{Tool: t, Ingress: e.in, Source: src}
		}
	}
	return nil
}

// Tools 返回全部工具，按名称升序。
func (r *Registry) Tools() []ToolEntry {
	out := make([]ToolEntry, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool.Name < out[j].Tool.Name })
	return out
}

// Tool 按名称查找工具。
func (r *Registry) Tool(name string) (ToolEntry, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Ingress 按协议名查找入站适配器。
func (r *Registry) Ingress(p Protocol) (Ingress, bool) {
	e, ok := r.ingress[p]
	return e.in, ok
}

// Ingresses 返回全部入站适配器，按 Priority 升序、同优先级按协议名升序。
func (r *Registry) Ingresses() []Ingress {
	entries := make([]ingressEntry, 0, len(r.ingress))
	for _, e := range r.ingress {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].opts.Priority != entries[j].opts.Priority {
			return entries[i].opts.Priority < entries[j].opts.Priority
		}
		return entries[i].in.Protocol() < entries[j].in.Protocol()
	})
	out := make([]Ingress, len(entries))
	for i, e := range entries {
		out[i] = e.in
	}
	return out
}

// Fallback 返回兜底入站适配器（Seal 之后必定存在）。
func (r *Registry) Fallback() Ingress {
	for _, e := range r.ingress {
		if e.opts.Fallback {
			return e.in
		}
	}
	return nil
}

// Egress 按协议名查找出站适配器。
func (r *Registry) Egress(p Protocol) (Egress, bool) {
	e, ok := r.egress[p]
	return e, ok
}

// Translator 查找 from → to 的转换器。
func (r *Registry) Translator(from, to Protocol) (Translator, bool) {
	t, ok := r.translators[[2]Protocol{from, to}]
	return t, ok
}
