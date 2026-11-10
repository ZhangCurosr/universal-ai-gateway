package translate

import (
	"errors"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// TestRegistrarPairs 校验 REG-02 / REG-03：注册结果与组合表完全一致；每个方向对不支持的 Op 返回 400。
func TestRegistrarPairs(t *testing.T) {
	r := protocol.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	want := map[[2]protocol.Protocol]bool{}
	for _, p := range pairs {
		if want[p] {
			t.Fatalf("pairs contains duplicate %v", p)
		}
		want[p] = true
		if _, ok := clientSides[p[0]]; !ok {
			t.Errorf("pair %v: client side %q is not registered", p, p[0])
		}
		if _, ok := upstreamSides[p[1]]; !ok {
			t.Errorf("pair %v: upstream side %q is not registered", p, p[1])
		}
	}

	protos := map[protocol.Protocol]bool{}
	for p := range clientSides {
		protos[p] = true
	}
	for p := range upstreamSides {
		protos[p] = true
	}
	for from := range protos {
		for to := range protos {
			tr, found := r.Translator(from, to)
			if found != want[[2]protocol.Protocol{from, to}] {
				t.Errorf("Translator(%s, %s) found = %v, want %v", from, to, found, want[[2]protocol.Protocol{from, to}])
				continue
			}
			if !found {
				continue
			}
			if tr.From() != from || tr.To() != to {
				t.Errorf("Translator(%s, %s) reports %s -> %s", from, to, tr.From(), tr.To())
			}
			env := &protocol.Envelope{Protocol: from, Op: "no.such.op", Model: "m", Body: []byte(`{}`)}
			_, err := tr.Request(env, "up")
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Code != apperr.CodeInvalidRequest {
				t.Errorf("Translator(%s, %s).Request(unsupported op) error = %v, want %s", from, to, err, apperr.CodeInvalidRequest)
			}
		}
	}
}

// TestNewTranslatorComposed 校验不在 legacyTranslators 中的方向直接得到 composed，并保留 Options。
func TestNewTranslatorComposed(t *testing.T) {
	pair := [2]protocol.Protocol{"x_client", "x_upstream"}
	tr := newTranslator(pair, Options{DropUnknownFields: true})
	c, ok := tr.(composed)
	if !ok {
		t.Fatalf("newTranslator(%v) = %T, want composed", pair, tr)
	}
	if c.From() != pair[0] || c.To() != pair[1] || !c.opts.DropUnknownFields {
		t.Fatalf("newTranslator(%v) = %+v, want from/to set and DropUnknownFields", pair, c)
	}
}
