// Package obs 提供日志初始化与凭据脱敏（spec/02 §7、spec/05 §6）。
package obs

import (
	"io"
	"log/slog"
	"strings"
)

// NewLogger 创建输出 JSON 到 w 的 logger。level 取 debug|info|warn|error，未知值按 info。
func NewLogger(w io.Writer, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)}))
}

// NewLoggerVar 与 NewLogger 相同，但级别取自 lv：之后调用 lv.Set 即可在运行中改变级别（配置热重载）。
func NewLoggerVar(w io.Writer, lv *slog.LevelVar) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}

// ParseLevel 把 debug|info|warn|error 转换为 slog.Level（不区分大小写），未知值按 info。
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
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
