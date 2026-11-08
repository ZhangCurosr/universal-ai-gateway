package anthropic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

var messagesRoute = protocol.Route{Method: http.MethodPost, Path: "/v1/messages", Kind: protocol.RouteCall, Op: OpMessages}

func parse(t *testing.T, body string) (*protocol.Envelope, error) {
	t.Helper()
	return Ingress{}.Parse(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), messagesRoute, []byte(body))
}

// AFH-01 ~ AFH-04
func TestAffinityHint(t *testing.T) {
	env, err := parse(t, `{"model":"m","metadata":{"user_id":"u-42"},"messages":[{"role":"user","content":"hi"}]}`)
	if err != nil || env.Affinity.Source != protocol.AffinityUser || string(env.Affinity.Value) != "u-42" {
		t.Fatalf("user hint = %+v, %v", env.Affinity, err)
	}

	first := `{"model":"m","system":"be brief","tools":[{"name":"t"}],"messages":[{"role":"user","content":"hi"}]}`
	more := `{"model":"m","system":"be brief","tools":[{"name":"t"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"next"}]}`
	a, _ := parse(t, first)
	b, _ := parse(t, more)
	if a.Affinity.Source != protocol.AffinityPrefix || !bytes.Equal(a.Affinity.Value, b.Affinity.Value) {
		t.Fatalf("prefix hint changed when the conversation grew: %q vs %q", a.Affinity.Value, b.Affinity.Value)
	}
	want := `[{"name":"t"}]` + "\x00" + `"be brief"` + "\x00" + `{"role":"user","content":"hi"}`
	if string(a.Affinity.Value) != want {
		t.Fatalf("prefix value = %q, want %q", a.Affinity.Value, want)
	}

	none, _ := parse(t, `{"model":"m"}`)
	if none.Affinity.Source != "" || none.Affinity.Value != nil {
		t.Fatalf("no-hint affinity = %+v, want zero", none.Affinity)
	}
}

// IN-L03、IN-L04
func TestParseRejectsBadFields(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","max_tokens":-1}`,
		`{"model":"m","max_tokens":"100"}`,
		`{"model":"m","max_tokens":1.5}`,
		`{"model":"m","stream":"yes"}`,
		`{"model":7}`,
		`{"model":""}`,
	} {
		if _, err := parse(t, body); apperr.CodeOf(err) != apperr.CodeInvalidRequest {
			t.Errorf("Parse(%s) error = %v, want invalid_request", body, err)
		}
	}
	env, err := parse(t, `{"model":"m","stream":null,"max_tokens":null}`)
	if err != nil || env.Stream || env.Estimate.Get("output_tokens") != 0 {
		t.Fatalf("null fields: %+v, %v", env, err)
	}
}

func TestStreamErrorFormat(t *testing.T) {
	ev := Ingress{}.StreamError(apperr.New(apperr.CodeOverloaded, "busy"))
	var v struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if ev.Name != "error" || json.Unmarshal(ev.Data, &v) != nil || v.Type != "error" || v.Error.Type != "overloaded_error" || v.Error.Message != "busy" {
		t.Fatalf("StreamError() = %s %s", ev.Name, ev.Data)
	}
}

func TestWriteCatalog(t *testing.T) {
	rec := httptest.NewRecorder()
	Ingress{}.WriteCatalog(rec, nil)
	if !strings.Contains(rec.Body.String(), `"first_id":null`) || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("empty catalog = %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	Ingress{}.WriteCatalog(rec, []protocol.ModelInfo{{ID: "a"}, {ID: "b"}})
	var v struct {
		Data            []struct{ Type, ID string }
		HasMore         bool   `json:"has_more"`
		FirstID, LastID string `json:"-"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &v) != nil || len(v.Data) != 2 || v.Data[0].Type != "model" ||
		!strings.Contains(rec.Body.String(), `"first_id":"a"`) || !strings.Contains(rec.Body.String(), `"last_id":"b"`) {
		t.Fatalf("catalog = %s", rec.Body.String())
	}
}

func TestClaims(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if (Ingress{}).Claims(r) {
		t.Fatal("claimed a request without anthropic-version")
	}
	r.Header.Set("anthropic-version", "2023-06-01")
	if !(Ingress{}).Claims(r) {
		t.Fatal("did not claim a request with anthropic-version")
	}
}
