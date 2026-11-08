package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

type fakeIngress struct {
	p      Protocol
	routes []Route
}

func (f fakeIngress) Protocol() Protocol                                  { return f.p }
func (f fakeIngress) Routes() []Route                                     { return f.routes }
func (fakeIngress) Claims(*http.Request) bool                             { return false }
func (fakeIngress) Parse(*http.Request, Route, []byte) (*Envelope, error) { return &Envelope{}, nil }
func (fakeIngress) WriteError(http.ResponseWriter, string, *apperr.Error) {}
func (fakeIngress) StreamError(*apperr.Error) sse.Event                   { return sse.Event{} }
func (fakeIngress) WriteCatalog(http.ResponseWriter, []ModelInfo)         {}

type fakeEgress struct{ p Protocol }

func (f fakeEgress) Protocol() Protocol                     { return f.p }
func (fakeEgress) Units() []string                          { return nil }
func (fakeEgress) Prepare(*Envelope, string) (*Call, error) { return &Call{}, nil }
func (fakeEgress) Do(context.Context, Upstream, *Call) (*Response, error) {
	return nil, nil
}

type fakeTranslator struct{ from, to Protocol }

func (f fakeTranslator) From() Protocol                                 { return f.from }
func (f fakeTranslator) To() Protocol                                   { return f.to }
func (fakeTranslator) Request(*Envelope, string) (*Call, error)         { return nil, nil }
func (fakeTranslator) Response(*Envelope, *Response) (*Response, error) { return nil, nil }

func gen(path string) []Route {
	return []Route{{Method: "POST", Path: path, Kind: RouteCall, Op: "call"}}
}

func TestRegistrySealRules(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(r *Registry) error
		wantErr bool
	}{
		{"ok", func(r *Registry) error {
			_ = r.RegisterIngress(fakeIngress{"a", append(gen("/a"), Route{Method: "GET", Path: "/v1/models", Kind: RouteCatalog})}, IngressOptions{Fallback: true})
			return r.RegisterIngress(fakeIngress{"b", append(gen("/b"), Route{Method: "GET", Path: "/v1/models", Kind: RouteCatalog})}, IngressOptions{Priority: -1})
		}, false},
		{"REG-01 empty", func(*Registry) error { return nil }, true},
		{"REG-02 no fallback", func(r *Registry) error { return r.RegisterIngress(fakeIngress{"a", gen("/a")}, IngressOptions{}) }, true},
		{"REG-02 two fallbacks", func(r *Registry) error {
			_ = r.RegisterIngress(fakeIngress{"a", gen("/a")}, IngressOptions{Fallback: true})
			return r.RegisterIngress(fakeIngress{"b", gen("/b")}, IngressOptions{Fallback: true})
		}, true},
		{"REG-03 no call route", func(r *Registry) error {
			return r.RegisterIngress(fakeIngress{"a", []Route{{Method: "GET", Path: "/m", Kind: RouteCatalog}}}, IngressOptions{Fallback: true})
		}, true},
		{"REG-03 many call routes with distinct ops", func(r *Registry) error {
			return r.RegisterIngress(fakeIngress{"a", []Route{
				{Method: "POST", Path: "/search", Kind: RouteCall, Op: "search"},
				{Method: "POST", Path: "/extract", Kind: RouteCall, Op: "extract"},
				{Method: "POST", Path: "/tasks", Kind: RouteCall, Op: "task.submit", Body: BodyStream, MaxBodyBytes: 200 << 20},
				{Method: "GET", Path: "/tasks/{id}", Kind: RouteCall, Op: "task.get", Body: BodyNone},
			}}, IngressOptions{Fallback: true})
		}, false},
		{"REG-03 duplicate op", func(r *Registry) error {
			return r.RegisterIngress(fakeIngress{"a", []Route{
				{Method: "POST", Path: "/a", Kind: RouteCall, Op: "x"},
				{Method: "POST", Path: "/b", Kind: RouteCall, Op: "x"},
			}}, IngressOptions{Fallback: true})
		}, true},
		{"REG-03 empty op", func(r *Registry) error {
			return r.RegisterIngress(fakeIngress{"a", []Route{{Method: "POST", Path: "/a", Kind: RouteCall}}}, IngressOptions{Fallback: true})
		}, true},
		{"REG-03 two catalogs", func(r *Registry) error {
			return r.RegisterIngress(fakeIngress{"a", append(gen("/a"),
				Route{Method: "GET", Path: "/m1", Kind: RouteCatalog}, Route{Method: "GET", Path: "/m2", Kind: RouteCatalog})}, IngressOptions{Fallback: true})
		}, true},
		{"REG-03 bad body mode", func(r *Registry) error {
			return r.RegisterIngress(fakeIngress{"a", []Route{{Method: "POST", Path: "/a", Kind: RouteCall, Op: "x", Body: 9}}}, IngressOptions{Fallback: true})
		}, true},
		{"REG-03 bad path", func(r *Registry) error {
			return r.RegisterIngress(fakeIngress{"a", gen("a")}, IngressOptions{Fallback: true})
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			if err := tt.setup(r); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if err := r.Seal(); (err != nil) != tt.wantErr {
				t.Fatalf("Seal() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRegistryDuplicatesAndLookup(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterIngress(fakeIngress{"a", gen("/a")}, IngressOptions{Fallback: true, Priority: 5}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterIngress(fakeIngress{"a", gen("/a2")}, IngressOptions{}); err == nil {
		t.Fatal("duplicate ingress accepted")
	}
	_ = r.RegisterIngress(fakeIngress{"b", gen("/b")}, IngressOptions{Priority: 1})
	if err := r.RegisterEgress(fakeEgress{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterEgress(fakeEgress{"a"}); err == nil {
		t.Fatal("duplicate egress accepted")
	}
	if err := r.RegisterTranslator(fakeTranslator{"a", "a"}); err == nil {
		t.Fatal("same-protocol translator accepted")
	}
	if err := r.RegisterTranslator(fakeTranslator{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterEgress(fakeEgress{"c"}); err != ErrSealed {
		t.Fatalf("register after Seal() error = %v, want ErrSealed", err)
	}
	if ins := r.Ingresses(); len(ins) != 2 || ins[0].Protocol() != "b" { // 按 Priority 升序
		t.Fatalf("Ingresses() order wrong: %v", ins)
	}
	if r.Fallback().Protocol() != "a" {
		t.Fatal("Fallback() wrong")
	}
	if _, ok := r.Translator("a", "b"); !ok {
		t.Fatal("Translator(a,b) not found")
	}
	if _, ok := r.Egress("zzz"); ok {
		t.Fatal("unknown egress found")
	}
}

type fakeToolIngress struct {
	fakeIngress
	tools []Tool
}

func (f fakeToolIngress) Tools() []Tool { return f.tools }
func (fakeToolIngress) ToolRequest(string, json.RawMessage) (*ToolRequest, error) {
	return &ToolRequest{Op: "call"}, nil
}
func (fakeToolIngress) ToolResult(string, int, http.Header, []byte) (*ToolResult, error) {
	return TextResult("ok", false), nil
}

func tool(name string) Tool {
	return Tool{Name: name, Description: "d", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}
}

// REG-05
func TestRegistryTools(t *testing.T) {
	seal := func(ins ...Ingress) (*Registry, error) {
		r := NewRegistry()
		for i, in := range ins {
			if err := r.RegisterIngress(in, IngressOptions{Fallback: i == 0}); err != nil {
				return nil, err
			}
		}
		return r, r.Seal()
	}
	r, err := seal(fakeToolIngress{fakeIngress{"a", gen("/a")}, []Tool{tool("zeta"), tool("alpha")}}, fakeIngress{"b", gen("/b")})
	if err != nil {
		t.Fatal(err)
	}
	if ts := r.Tools(); len(ts) != 2 || ts[0].Tool.Name != "alpha" || ts[0].Ingress.Protocol() != "a" {
		t.Fatalf("Tools() = %+v", ts)
	}
	if _, ok := r.Tool("zeta"); !ok {
		t.Fatal("Tool(zeta) not found")
	}
	bad := []Tool{
		{Name: "has space", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "x", Description: "d", InputSchema: json.RawMessage(`{"type":"string"}`)},
		{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	for _, b := range bad {
		if _, err := seal(fakeToolIngress{fakeIngress{"a", gen("/a")}, []Tool{b}}); err == nil {
			t.Errorf("Seal() accepted bad tool %+v", b)
		}
	}
	_, err = seal(fakeToolIngress{fakeIngress{"a", gen("/a")}, []Tool{tool("dup")}},
		fakeToolIngress{fakeIngress{"b", gen("/b")}, []Tool{tool("dup")}})
	if err == nil {
		t.Error("Seal() accepted duplicate tool names across protocols")
	}
}
