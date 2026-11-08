// Package obs 提供日志初始化与凭据脱敏（spec/02 §7、spec/05 §6）。
package obs

import (
	"io"
	"log/slog"
	"strings"
)

// NewLogger 创建输出 JSON 到 w 的 logger。level 取 debug|info|warn|error，未知值按 info。
func NewLogger(w io.Writer, level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}

// Discard 返回丢弃所有输出的 logger，供测试使用。
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// MaskSecret 对凭据脱敏：长度 <= 8 返回 "****"，否则保留前 4 与后 4 个字符。
func MaskSecret(s string) string {
	r := []rune(s)
	if len(r) <= 8 {
		return "****"
	}
	return string(r[:4]) + "****" + string(r[len(r)-4:])
}
