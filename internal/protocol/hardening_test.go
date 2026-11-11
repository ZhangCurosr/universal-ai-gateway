package protocol

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

type budgetedTestBody struct {
	closed, called bool
	limit          int64
	data           []byte
	err            error
}

func (*budgetedTestBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *budgetedTestBody) Close() error           { b.closed = true; return nil }
func (b *budgetedTestBody) ReadBuffered(limit int64, _ func(func())) ([]byte, error) {
	b.called = true
	b.limit = limit
	return b.data, b.err
}

func TestReadBodyUsesSharedBudget(t *testing.T) {
	for _, tt := range []struct {
		name     string
		data     string
		err      error
		wantCode apperr.Code
	}{
		{name: "success", data: "abc"},
		{name: "overloaded", err: apperr.New(apperr.CodeOverloaded, "budget exhausted"), wantCode: apperr.CodeOverloaded},
		{name: "network error", err: io.ErrUnexpectedEOF},
		{name: "per response limit", data: "abcdef"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := &budgetedTestBody{data: []byte(tt.data), err: tt.err}
			body, err := ReadBody(Upstream{MaxBufferedBytes: 5}, &http.Response{StatusCode: 200, Body: b})
			if !b.called || !b.closed || b.limit != 6 {
				t.Fatalf("body lifecycle = %+v, want budgeted read(6) and close", b)
			}
			switch tt.name {
			case "success":
				if err != nil || string(body) != tt.data {
					t.Fatalf("ReadBody = %q, %v", body, err)
				}
			case "overloaded":
				if apperr.CodeOf(err) != tt.wantCode {
					t.Fatalf("budget exhaustion = %v", err)
				}
			case "network error":
				var ae *AttemptError
				if !errors.As(err, &ae) || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("network error = %v", err)
				}
			case "per response limit":
				if !errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("size error = %v", err)
				}
			}
		})
	}
}
