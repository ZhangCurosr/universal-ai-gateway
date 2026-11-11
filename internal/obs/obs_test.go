package obs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestMaskSecret(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "****"},
		{"12345678", "****"},
		{"sk-ant-api03-abcdefgh", "sk-a****efgh"},
	}
	for _, tt := range tests {
		if got := MaskSecret(tt.in); got != tt.want {
			t.Errorf("MaskSecret(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "Warn": slog.LevelWarn, "error": slog.LevelError, "bogus": slog.LevelInfo} {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewLoggerVarChangesLevel(t *testing.T) {
	var buf bytes.Buffer
	var lv slog.LevelVar
	lv.Set(slog.LevelWarn)
	log := NewLoggerVar(&buf, &lv)
	log.Info("hidden")
	lv.Set(slog.LevelDebug)
	log.Debug("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Fatalf("log output = %s", buf.String())
	}
}
