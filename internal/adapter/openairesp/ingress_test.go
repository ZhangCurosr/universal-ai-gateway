package openairesp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestAffinitySources(t *testing.T) {
	in := Ingress{}
	rt := protocol.Route{Method: http.MethodPost, Path: "/v1/responses", Kind: protocol.RouteCall, Op: OpCreate, Body: protocol.BodyBuffered}

	// 1. previous_response_id 严格亲和
	bodyPrev := []byte(`{"model":"gpt","input":"hello","previous_response_id":"resp_prev_123"}`)
	env, err := in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyPrev)
	if err != nil {
		t.Fatalf("Parse(previous_response_id) error: %v", err)
	}
	if env.Affinity.Source != protocol.AffinityPin || !env.Affinity.Strict || string(env.Affinity.Value) != "resp_prev_123" {
		t.Fatalf("Affinity previous_response_id = %+v, want Source=j, Strict=true, Value=resp_prev_123", env.Affinity)
	}

	// 2. prompt_cache_key
	bodyCache := []byte(`{"model":"gpt","input":"hello","prompt_cache_key":"cache_key_456"}`)
	env, err = in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyCache)
	if err != nil {
		t.Fatalf("Parse(prompt_cache_key) error: %v", err)
	}
	if env.Affinity.Source != protocol.AffinityUser || env.Affinity.Strict || string(env.Affinity.Value) != "cache_key_456" {
		t.Fatalf("Affinity prompt_cache_key = %+v, want Source=u, Strict=false, Value=cache_key_456", env.Affinity)
	}

	// 3. safety_identifier
	bodySafety := []byte(`{"model":"gpt","input":"hello","safety_identifier":"safe_id_789"}`)
	env, err = in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodySafety)
	if err != nil {
		t.Fatalf("Parse(safety_identifier) error: %v", err)
	}
	if env.Affinity.Source != protocol.AffinityUser || env.Affinity.Strict || string(env.Affinity.Value) != "safe_id_789" {
		t.Fatalf("Affinity safety_identifier = %+v, want Source=u, Strict=false, Value=safe_id_789", env.Affinity)
	}

	// 4. user
	bodyUser := []byte(`{"model":"gpt","input":"hello","user":"usr_999"}`)
	env, err = in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyUser)
	if err != nil {
		t.Fatalf("Parse(user) error: %v", err)
	}
	if env.Affinity.Source != protocol.AffinityUser || env.Affinity.Strict || string(env.Affinity.Value) != "usr_999" {
		t.Fatalf("Affinity user = %+v, want Source=u, Strict=false, Value=usr_999", env.Affinity)
	}

	// 优先级覆盖：previous_response_id > prompt_cache_key > safety_identifier > user
	bodyMulti := []byte(`{"model":"gpt","input":"hello","previous_response_id":"resp_top","prompt_cache_key":"k","user":"u"}`)
	env, err = in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyMulti)
	if err != nil {
		t.Fatalf("Parse(multi) error: %v", err)
	}
	if env.Affinity.Source != protocol.AffinityPin || string(env.Affinity.Value) != "resp_top" {
		t.Fatalf("Affinity priority previous_response_id = %+v", env.Affinity)
	}

	bodyCacheUser := []byte(`{"model":"gpt","input":"hello","prompt_cache_key":"k1","safety_identifier":"s1","user":"u1"}`)
	env, err = in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyCacheUser)
	if err != nil {
		t.Fatalf("Parse(cache_user) error: %v", err)
	}
	if env.Affinity.Source != protocol.AffinityUser || string(env.Affinity.Value) != "k1" {
		t.Fatalf("Affinity priority prompt_cache_key = %+v", env.Affinity)
	}

	// 5. 前缀（string input）
	bodyPrefix := []byte(`{"model":"gpt","instructions":"sys","tools":[{"type":"web"}],"input":"hi"}`)
	env, err = in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyPrefix)
	if err != nil {
		t.Fatalf("Parse(prefix) error: %v", err)
	}
	if env.Affinity.Source != protocol.AffinityPrefix {
		t.Fatalf("Affinity prefix source = %q, want p", env.Affinity.Source)
	}
	wantPrefixVal := bytes.Join([][]byte{[]byte(`"sys"`), []byte(`[{"type":"web"}]`), []byte(`"hi"`)}, []byte{0x00})
	if !bytes.Equal(env.Affinity.Value, wantPrefixVal) {
		t.Fatalf("Affinity prefix value = %q, want %q", env.Affinity.Value, wantPrefixVal)
	}

	// 6. 前缀（array input）：只取第一个元素，追加后续元素不改变 Value
	bodyArr1 := []byte(`{"model":"gpt","instructions":"inst","input":[{"type":"message","content":"first"}]}`)
	bodyArr2 := []byte(`{"model":"gpt","instructions":"inst","input":[{"type":"message","content":"first"},{"type":"message","content":"second"}]}`)
	env1, err1 := in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyArr1)
	env2, err2 := in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyArr2)
	if err1 != nil || err2 != nil {
		t.Fatalf("Parse(array input) errors: %v, %v", err1, err2)
	}
	if env1.Affinity.Source != protocol.AffinityPrefix || env2.Affinity.Source != protocol.AffinityPrefix {
		t.Fatalf("Affinity sources: %q, %q", env1.Affinity.Source, env2.Affinity.Source)
	}
	if !bytes.Equal(env1.Affinity.Value, env2.Affinity.Value) {
		t.Fatalf("Appending subsequent items changed prefix affinity: %q != %q", env1.Affinity.Value, env2.Affinity.Value)
	}

	// 7. 所有亲和字段均不存在
	bodyNone := []byte(`{"model":"gpt"}`)
	envNone, err := in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), rt, bodyNone)
	if err != nil {
		t.Fatalf("Parse(none) error: %v", err)
	}
	if envNone.Affinity.Source != "" || len(envNone.Affinity.Value) != 0 || envNone.Affinity.Strict {
		t.Fatalf("Affinity none = %+v, want zero value", envNone.Affinity)
	}
}

func TestStreamErrorFormat(t *testing.T) {
	in := Ingress{}
	appErr := apperr.New(apperr.CodeRateLimited, "too many requests").WithRetryAfter(5)
	ev := in.StreamError(appErr)

	if ev.Name != "error" {
		t.Fatalf("ev.Name = %q, want 'error'", ev.Name)
	}

	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if data["type"] != "error" {
		t.Errorf("data[type] = %v, want 'error'", data["type"])
	}
	if data["code"] != string(apperr.CodeRateLimited) {
		t.Errorf("data[code] = %v, want %q", data["code"], apperr.CodeRateLimited)
	}
	if data["message"] != "too many requests" {
		t.Errorf("data[message] = %v, want 'too many requests'", data["message"])
	}
	if data["param"] != nil {
		t.Errorf("data[param] = %v, want nil", data["param"])
	}
}

func TestParseValidationAndRoutes(t *testing.T) {
	in := Ingress{}
	createRt := protocol.Route{Method: http.MethodPost, Path: "/v1/responses", Kind: protocol.RouteCall, Op: OpCreate, Body: protocol.BodyBuffered}

	invalidBodies := []struct {
		name string
		body []byte
	}{
		{"not json", []byte("hello")},
		{"json array", []byte(`[1, 2, 3]`)},
		{"empty object without model", []byte(`{}`)},
		{"empty model", []byte(`{"model":""}`)},
		{"stream as string", []byte(`{"model":"gpt","stream":"true"}`)},
		{"stream as int", []byte(`{"model":"gpt","stream":1}`)},
		{"negative max_output_tokens", []byte(`{"model":"gpt","max_output_tokens":-10}`)},
		{"zero max_output_tokens", []byte(`{"model":"gpt","max_output_tokens":0}`)},
		{"string max_output_tokens", []byte(`{"model":"gpt","max_output_tokens":"100"}`)},
	}

	for _, tt := range invalidBodies {
		t.Run(tt.name, func(t *testing.T) {
			_, err := in.Parse(httptest.NewRequest("POST", "/v1/responses", nil), createRt, tt.body)
			if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
				t.Fatalf("Parse(%s) err = %v, want CodeInvalidRequest", tt.name, err)
			}
		})
	}

	// 非 create 路由
	getRt := protocol.Route{Method: http.MethodGet, Path: "/v1/responses/{id}", Kind: protocol.RouteCall, Op: OpGet, Body: protocol.BodyNone}
	req := httptest.NewRequest("GET", "/v1/responses/resp_test_id?include[]=data", nil)
	req.SetPathValue("id", "resp_test_id")
	req.Header.Set("X-UAIG-Model", "custom-model")

	env, err := in.Parse(req, getRt, nil)
	if err != nil {
		t.Fatalf("Parse(OpGet) error: %v", err)
	}
	if env.Model != "custom-model" {
		t.Errorf("env.Model = %q, want custom-model", env.Model)
	}
	if env.Params["id"] != "resp_test_id" || env.Params["query"] != "include[]=data" {
		t.Errorf("env.Params = %v", env.Params)
	}
	if !env.Affinity.Strict || env.Affinity.Source != protocol.AffinityPin || string(env.Affinity.Value) != "resp_test_id" {
		t.Errorf("env.Affinity = %+v", env.Affinity)
	}
	zeroUsage := pricing.Usage{
		pricing.UnitInputTokens:     0,
		pricing.UnitOutputTokens:    0,
		pricing.UnitCacheReadTokens: 0,
	}
	if !env.Estimate.Equal(zeroUsage) {
		t.Errorf("env.Estimate = %v, want zeroUsage", env.Estimate)
	}
	if env.Body != nil {
		t.Errorf("env.Body for BodyNone route should be nil")
	}
}
