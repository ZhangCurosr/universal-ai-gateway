package openai

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

var chatRoute = protocol.Route{Method: http.MethodPost, Path: "/v1/chat/completions", Kind: protocol.RouteCall, Op: OpChatCompletions}

func parse(t *testing.T, body string) (*protocol.Envelope, error) {
	t.Helper()
	return Ingress{}.Parse(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatRoute, []byte(body))
}

// AFH-01 ~ AFH-04：prompt_cache_key > user > 前缀
func TestAffinityHint(t *testing.T) {
	env, _ := parse(t, `{"model":"m","prompt_cache_key":"pk","user":"u","messages":[]}`)
	if env.Affinity.Source != protocol.AffinityUser || string(env.Affinity.Value) != "pk" {
		t.Fatalf("prompt_cache_key hint = %+v", env.Affinity)
	}
	env, _ = parse(t, `{"model":"m","user":"u","messages":[]}`)
	if env.Affinity.Source != protocol.AffinityUser || string(env.Affinity.Value) != "u" {
		t.Fatalf("user hint = %+v", env.Affinity)
	}
	a, _ := parse(t, `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"q"}]}`)
	b, _ := parse(t, `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"q"},{"role":"assistant","content":"a"}]}`)
	if a.Affinity.Source != protocol.AffinityPrefix || !bytes.Equal(a.Affinity.Value, b.Affinity.Value) {
		t.Fatalf("prefix hint changed when the conversation grew: %q vs %q", a.Affinity.Value, b.Affinity.Value)
	}
	none, _ := parse(t, `{"model":"m"}`)
	if none.Affinity.Source != "" {
		t.Fatalf("no-hint affinity = %+v", none.Affinity)
	}
}

// IN-L04：max_completion_tokens 优先；非正整数拒绝
func TestParseOutputLimit(t *testing.T) {
	env, err := parse(t, `{"model":"m","max_tokens":20}`)
	if err != nil || env.Estimate.Get("output_tokens") != 20 {
		t.Fatalf("max_tokens: %+v, %v", env, err)
	}
	for _, body := range []string{`{"model":"m","max_tokens":0}`, `{"model":"m","max_completion_tokens":"5"}`, `{"model":"m","stream":1}`} {
		if _, err := parse(t, body); apperr.CodeOf(err) != apperr.CodeInvalidRequest {
			t.Errorf("Parse(%s) error = %v", body, err)
		}
	}
}

func TestStreamErrorAndCatalog(t *testing.T) {
	ev := Ingress{}.StreamError(apperr.New(apperr.CodeRateLimited, "slow"))
	var v struct {
		Error struct{ Message, Type, Code string }
	}
	if ev.Name != "" || json.Unmarshal(ev.Data, &v) != nil || v.Error.Type != "rate_limit_error" || v.Error.Code != "rate_limited" {
		t.Fatalf("StreamError() = %q %s", ev.Name, ev.Data)
	}
	rec := httptest.NewRecorder()
	Ingress{}.WriteCatalog(rec, []protocol.ModelInfo{{ID: "gpt"}})
	var c struct {
		Object string
		Data   []struct{ ID, Object, OwnedBy string }
	}
	if json.Unmarshal(rec.Body.Bytes(), &c) != nil || c.Object != "list" || len(c.Data) != 1 || c.Data[0].Object != "model" {
		t.Fatalf("catalog = %s", rec.Body.String())
	}
}
