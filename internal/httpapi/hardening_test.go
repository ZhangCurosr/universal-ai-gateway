package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type failedFlushWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *failedFlushWriter) FlushError() error { return w.err }

// STR-07：Recover 和 AccessLog 的多层包装不得遮蔽真实连接的 Flush 错误。
func TestTrackingWritersPreserveFlushError(t *testing.T) {
	want := errors.New("client write timed out")
	base := &failedFlushWriter{ResponseRecorder: httptest.NewRecorder(), err: want}
	w := &trackingWriter{ResponseWriter: &trackingWriter{ResponseWriter: base}}
	if err := http.NewResponseController(w).Flush(); !errors.Is(err, want) {
		t.Fatalf("wrapped Flush() = %v, want %v", err, want)
	}
}
