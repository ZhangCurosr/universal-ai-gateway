package gemini

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

var modelsRoute = protocol.Route{
	Method: http.MethodPost,
	Path:   "/v1beta/models/{target}",
	Kind:   protocol.RouteCall,
	Op:     OpModelsCall,
	Body:   protocol.BodyBuffered,
}

func parseReq(t *testing.T, target, query, body string) (*protocol.Envelope, error) {
	t.Helper()
	path := "/v1beta/models/" + target
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.SetPathValue("target", target)
	return Ingress{}.Parse(req, modelsRoute, []byte(body))
}

// 路径拆分测试（IN-G-02 ~ IN-G-05）
func TestPathSplitting(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		query      string
		body       string
		wantModel  string
		wantMethod string
		wantStream bool
		wantErr    string
	}{
		{
			name:       "hyphens and dots in model",
			target:     "gemini-2.5-flash.lite:generateContent",
			body:       `{"contents":[]}`,
			wantModel:  "gemini-2.5-flash.lite",
			wantMethod: "generateContent",
		},
		{
			name:       "multiple colons splits at last colon",
			target:     "tunedModels/custom:v1:generateContent",
			body:       `{"contents":[]}`,
			wantModel:  "tunedModels/custom:v1",
			wantMethod: "generateContent",
		},
		{
			name:       "models prefix is stripped",
			target:     "models/gemini-2.0-flash:countTokens",
			body:       `{"contents":[]}`,
			wantModel:  "gemini-2.0-flash",
			wantMethod: "countTokens",
		},
		{
			name:       "streamGenerateContent with alt=sse",
			target:     "gemini-1.5-pro:streamGenerateContent",
			query:      "alt=sse",
			body:       `{"contents":[]}`,
			wantModel:  "gemini-1.5-pro",
			wantMethod: "streamGenerateContent",
			wantStream: true,
		},
		{
			name:    "no colon in target",
			target:  "gemini-1.5-pro",
			body:    `{"contents":[]}`,
			wantErr: "path must be /v1beta/models/{model}:{method}",
		},
		{
			name:    "empty method",
			target:  "gemini-1.5-pro:",
			body:    `{"contents":[]}`,
			wantErr: "path must be /v1beta/models/{model}:{method}",
		},
		{
			name:    "empty model",
			target:  ":generateContent",
			body:    `{"contents":[]}`,
			wantErr: "path must be /v1beta/models/{model}:{method}",
		},
		{
			name:    "unsupported method",
			target:  "gemini-1.5-pro:batchGenerate",
			body:    `{"contents":[]}`,
			wantErr: `method "batchGenerate" is not supported`,
		},
		{
			name:    "streamGenerateContent missing alt=sse",
			target:  "gemini-1.5-pro:streamGenerateContent",
			body:    `{"contents":[]}`,
			wantErr: "streamGenerateContent requires alt=sse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := parseReq(t, tt.target, tt.query, tt.body)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if apperr.CodeOf(err) != apperr.CodeInvalidRequest {
					t.Errorf("error code = %v, want invalid_request", apperr.CodeOf(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if env.Model != tt.wantModel {
				t.Errorf("Model = %q, want %q", env.Model, tt.wantModel)
			}
			if env.Params["method"] != tt.wantMethod {
				t.Errorf("Params[method] = %q, want %q", env.Params["method"], tt.wantMethod)
			}
			if env.Stream != tt.wantStream {
				t.Errorf("Stream = %v, want %v", env.Stream, tt.wantStream)
			}
		})
	}
}

// IN-G-08 亲和线索测试
func TestAffinityHint(t *testing.T) {
	// 完整前缀线索：systemInstruction + tools + contents[0]
	body1 := `{"systemInstruction":{"parts":[{"text":"sys"}]},"tools":[{"functionDeclarations":[{"name":"f"}]}],"contents":[{"parts":[{"text":"q1"}]}]}`
	env1, err := parseReq(t, "gemini-pro:generateContent", "", body1)
	if err != nil {
		t.Fatal(err)
	}
	if env1.Affinity.Source != protocol.AffinityPrefix || len(env1.Affinity.Value) == 0 {
		t.Fatalf("affinity = %+v", env1.Affinity)
	}

	// 对话增长：追加 contents[1]，亲和键保持不变
	body2 := `{"systemInstruction":{"parts":[{"text":"sys"}]},"tools":[{"functionDeclarations":[{"name":"f"}]}],"contents":[{"parts":[{"text":"q1"}]},{"parts":[{"text":"a1"}]}]}`
	env2, err := parseReq(t, "gemini-pro:generateContent", "", body2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(env1.Affinity.Value, env2.Affinity.Value) {
		t.Fatalf("affinity changed on conversation growth: %q vs %q", env1.Affinity.Value, env2.Affinity.Value)
	}

	// 无线索字段时，Affinity 为零值
	bodyEmpty := `{"contents":[]}`
	envEmpty, err := parseReq(t, "gemini-pro:generateContent", "", bodyEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if envEmpty.Affinity.Source != "" {
		t.Fatalf("expected empty affinity, got %+v", envEmpty.Affinity)
	}

	// countTokens 不设亲和
	envCount, err := parseReq(t, "gemini-pro:countTokens", "", body1)
	if err != nil {
		t.Fatal(err)
	}
	if envCount.Affinity.Source != "" {
		t.Fatalf("countTokens should have no affinity, got %+v", envCount.Affinity)
	}
}

// IN-G-07 估算与配置测试
func TestParseEstimate(t *testing.T) {
	// 正确解析 maxOutputTokens
	env, err := parseReq(t, "gemini-pro:generateContent", "", `{"contents":[],"generationConfig":{"maxOutputTokens":1024}}`)
	if err != nil {
		t.Fatal(err)
	}
	if env.Estimate.Get("output_tokens") != 1024 {
		t.Fatalf("output_tokens = %d, want 1024", env.Estimate.Get("output_tokens"))
	}

	// 错误样例
	invalids := []string{
		`[1, 2]`,
		`{"contents":[],"generationConfig":123}`,
		`{"contents":[],"generationConfig":{"maxOutputTokens":0}}`,
		`{"contents":[],"generationConfig":{"maxOutputTokens":-10}}`,
		`{"contents":[],"generationConfig":{"maxOutputTokens":"1024"}}`,
	}
	for _, inv := range invalids {
		if _, err := parseReq(t, "gemini-pro:generateContent", "", inv); apperr.CodeOf(err) != apperr.CodeInvalidRequest {
			t.Errorf("Parse(%s) error = %v, want invalid_request", inv, err)
		}
	}
}

// 错误格式与流错误事件（spec/08 §10.2）
func TestWriteErrorAndStreamError(t *testing.T) {
	in := Ingress{}
	rec := httptest.NewRecorder()
	in.WriteError(rec, "req_123", apperr.New(apperr.CodeRateLimited, "quota exceeded").WithRetryAfter(5*time.Second))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "5" {
		t.Errorf("Retry-After = %q, want 5", rec.Header().Get("Retry-After"))
	}

	var errResp struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v", err)
	}
	if errResp.Error.Code != 429 || errResp.Error.Status != "RESOURCE_EXHAUSTED" || errResp.Error.Message != "quota exceeded" {
		t.Errorf("unexpected error body: %+v", errResp)
	}

	// StreamError
	ev := in.StreamError(apperr.New(apperr.CodeOverloaded, "service overloaded"))
	if ev.Name != "" {
		t.Errorf("event Name = %q, want empty", ev.Name)
	}
	var streamErr struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(ev.Data, &streamErr); err != nil {
		t.Fatalf("unmarshal stream error: %v", err)
	}
	if streamErr.Error.Code != 503 || streamErr.Error.Status != "UNAVAILABLE" {
		t.Errorf("unexpected stream error: %+v", streamErr)
	}
}

// WriteCatalog 模型列表测试
func TestWriteCatalog(t *testing.T) {
	in := Ingress{}
	rec := httptest.NewRecorder()
	in.WriteCatalog(rec, []protocol.ModelInfo{
		{ID: "models/gemini-2.5-pro"},
		{ID: "gemini-2.5-flash"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var cat struct {
		Models []catalogModel `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil {
		t.Fatalf("unmarshal catalog: %v", err)
	}
	if len(cat.Models) != 2 {
		t.Fatalf("models count = %d, want 2", len(cat.Models))
	}
	if cat.Models[0].Name != "models/gemini-2.5-pro" || cat.Models[0].DisplayName != "gemini-2.5-pro" {
		t.Errorf("model 0 = %+v", cat.Models[0])
	}
	if cat.Models[1].Name != "models/gemini-2.5-flash" || cat.Models[1].DisplayName != "gemini-2.5-flash" {
		t.Errorf("model 1 = %+v", cat.Models[1])
	}
}
