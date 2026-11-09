package translate

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

func TestRegistrarAndRegister(t *testing.T) {
	// Register 使用默认 Options{}
	r1 := protocol.NewRegistry()
	if err := Register(r1); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	tr1, found1 := r1.Translator(protoOpenAIChat, protoAnthropic)
	if !found1 || tr1 == nil {
		t.Fatal("expected ChatToAnthropic translator")
	}
	chatTr1, ok := tr1.(ChatToAnthropic)
	if !ok || chatTr1.Opts.DropUnknownFields {
		t.Fatalf("expected ChatToAnthropic with DropUnknownFields=false, got %+v", tr1)
	}

	// Registrar 传递自定义 Options
	r2 := protocol.NewRegistry()
	if err := Registrar(Options{DropUnknownFields: true})(r2); err != nil {
		t.Fatalf("Registrar() error = %v", err)
	}
	tr2, found2 := r2.Translator(protoOpenAIChat, protoAnthropic)
	if !found2 || tr2 == nil {
		t.Fatal("expected ChatToAnthropic translator")
	}
	chatTr2, ok := tr2.(ChatToAnthropic)
	if !ok || !chatTr2.Opts.DropUnknownFields {
		t.Fatalf("expected ChatToAnthropic with DropUnknownFields=true, got %+v", tr2)
	}
}

type recordHandler struct {
	records []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(name string) slog.Handler       { return h }

func TestWarnOnceLogging(t *testing.T) {
	resetDropLog()
	defer resetDropLog()

	handler := &recordHandler{}
	logger := slog.New(handler)
	opts := Options{DropUnknownFields: true, Logger: logger}

	// 第一次记录
	logDroppedField(opts, "from1", "to1", "fieldA")
	if len(handler.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(handler.records))
	}
	r0 := handler.records[0]
	if r0.Message != "translation dropped unknown field" {
		t.Errorf("expected msg 'translation dropped unknown field', got %q", r0.Message)
	}
	attrs := map[string]string{}
	r0.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	if attrs["from"] != "from1" || attrs["to"] != "to1" || attrs["field"] != "fieldA" {
		t.Errorf("unexpected attrs: %+v", attrs)
	}

	// 相同 (from, to, field) 重复调用：不应重复输出
	logDroppedField(opts, "from1", "to1", "fieldA")
	if len(handler.records) != 1 {
		t.Fatalf("expected still 1 record, got %d", len(handler.records))
	}

	// 不同 field
	logDroppedField(opts, "from1", "to1", "fieldB")
	if len(handler.records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(handler.records))
	}

	// 不同 from
	logDroppedField(opts, "from2", "to1", "fieldA")
	if len(handler.records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(handler.records))
	}

	// resetDropLog 清理后可再次输出
	resetDropLog()
	logDroppedField(opts, "from1", "to1", "fieldA")
	if len(handler.records) != 4 {
		t.Fatalf("expected 4 records after reset, got %d", len(handler.records))
	}
}

func TestDropUnknownFieldsIntegration(t *testing.T) {
	resetDropLog()
	defer resetDropLog()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	dropOpts := Options{DropUnknownFields: true, Logger: logger}
	rejectOpts := Options{DropUnknownFields: false, Logger: logger}

	// 1. ChatToAnthropic: 顶层与消息级未知字段
	t.Run("ChatToAnthropic", func(t *testing.T) {
		body := []byte(`{
			"model": "m",
			"unknown_top": "secret_value_123",
			"messages": [
				{"role": "user", "content": "hi", "unknown_msg": "secret_value_456"}
			]
		}`)
		env := &protocol.Envelope{Op: opChatCompletions, Model: "m", Body: body}

		// reject 模式下 400
		trReject := ChatToAnthropic{Opts: rejectOpts}
		if _, err := trReject.Request(env, "claude-3"); err == nil {
			t.Fatal("expected reject mode to fail")
		}

		// drop 模式下成功
		trDrop := ChatToAnthropic{Opts: dropOpts}
		call, err := trDrop.Request(env, "claude-3")
		if err != nil {
			t.Fatalf("drop mode failed: %v", err)
		}
		if call == nil || call.Model != "claude-3" {
			t.Fatalf("unexpected call: %+v", call)
		}
		// 验证日志不包含字段值
		logStr := buf.String()
		if strings.Contains(logStr, "secret_value_123") || strings.Contains(logStr, "secret_value_456") {
			t.Fatalf("log contains sensitive field value: %s", logStr)
		}
		if !strings.Contains(logStr, `"field":"unknown_top"`) {
			t.Errorf("log missing unknown_top field: %s", logStr)
		}
		if !strings.Contains(logStr, `"field":"messages[].unknown_msg"`) {
			t.Errorf("log missing messages[].unknown_msg field: %s", logStr)
		}

		// 明确拒绝字段即使在 drop 模式依然 400
		rejectBody := []byte(`{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`)
		if _, err := trDrop.Request(&protocol.Envelope{Op: opChatCompletions, Model: "m", Body: rejectBody}, "claude-3"); err == nil {
			t.Fatal("expected response_format to be rejected in drop mode")
		}
	})

	// 2. AnthropicToChat: 顶层与消息级未知字段
	t.Run("AnthropicToChat", func(t *testing.T) {
		body := []byte(`{
			"model": "m",
			"max_tokens": 100,
			"unknown_top": "secret_val",
			"messages": [
				{"role": "user", "content": [{"type":"text","text":"hi"}], "unknown_msg": 1}
			]
		}`)
		env := &protocol.Envelope{Op: opAnthropicCreate, Model: "m", Body: body}

		trReject := AnthropicToChat{Opts: rejectOpts}
		if _, err := trReject.Request(env, "gpt-4"); err == nil {
			t.Fatal("expected reject mode to fail")
		}

		trDrop := AnthropicToChat{Opts: dropOpts}
		call, err := trDrop.Request(env, "gpt-4")
		if err != nil {
			t.Fatalf("drop mode failed: %v", err)
		}
		if call == nil {
			t.Fatal("expected call")
		}

		// 明确拒绝字段（mcp_servers, container）在 drop 模式依然 400
		containerBody := []byte(`{"model":"m","max_tokens":100,"container":"c1","messages":[{"role":"user","content":"hi"}]}`)
		if _, err := trDrop.Request(&protocol.Envelope{Op: opAnthropicCreate, Model: "m", Body: containerBody}, "gpt-4"); err == nil {
			t.Fatal("expected container to be rejected in drop mode")
		}
	})

	// 3. ResponsesToAnthropic: 顶层与输入项级未知字段
	t.Run("ResponsesToAnthropic", func(t *testing.T) {
		body := []byte(`{
			"model": "m",
			"unknown_top": "secret_val",
			"input": [
				{"type": "message", "role": "user", "content": "hi", "unknown_input_key": "x"}
			]
		}`)
		env := &protocol.Envelope{Op: opResponsesCreate, Model: "m", Body: body}

		trReject := ResponsesToAnthropic{Opts: rejectOpts}
		if _, err := trReject.Request(env, "claude-3"); err == nil {
			t.Fatal("expected reject mode to fail")
		}

		trDrop := ResponsesToAnthropic{Opts: dropOpts}
		call, err := trDrop.Request(env, "claude-3")
		if err != nil {
			t.Fatalf("drop mode failed: %v", err)
		}
		if call == nil {
			t.Fatal("expected call")
		}

		// 明确拒绝的有状态特性（previous_response_id, background）在 drop 模式依然 400
		prevBody := []byte(`{"model":"m","previous_response_id":"resp_123","input":"hi"}`)
		if _, err := trDrop.Request(&protocol.Envelope{Op: opResponsesCreate, Model: "m", Body: prevBody}, "claude-3"); err == nil {
			t.Fatal("expected previous_response_id to be rejected in drop mode")
		}
	})
}
