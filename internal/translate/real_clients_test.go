package translate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// TestRealClientFixtures 验证真实客户端（Claude Code、Codex CLI 等）脱敏请求用例的转译行为（T41 §5.3）。
// 当前行为为 ok 时断言解析与合成均成功；为 error 时断言返回 invalid_request 且包含目标错误子串。
func TestRealClientFixtures(t *testing.T) {
	t.Run("claude_code", func(t *testing.T) {
		baseDir := filepath.Join("testdata", "real_clients", "claude_code")
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			t.Fatalf("read claude_code fixtures dir: %v", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			t.Run(e.Name(), func(t *testing.T) {
				caseDir := filepath.Join(baseDir, e.Name())
				inputBytes, err := os.ReadFile(filepath.Join(caseDir, "input.json"))
				if err != nil {
					t.Fatalf("read input.json: %v", err)
				}
				expectRaw, err := os.ReadFile(filepath.Join(caseDir, "expect.txt"))
				if err != nil {
					t.Fatalf("read expect.txt: %v", err)
				}
				expect := strings.TrimSpace(string(expectRaw))

				irReq, lowerErr := lowerAnthropic(inputBytes)
				if expect == "ok" {
					if lowerErr != nil {
						t.Fatalf("expected ok, but lowerAnthropic failed: %v", lowerErr)
					}
					if _, synthErr := synthesizeOpenAIChat(irReq); synthErr != nil {
						t.Fatalf("expected ok, but synthesizeOpenAIChat failed: %v", synthErr)
					}
				} else if strings.HasPrefix(expect, "error: ") {
					substr := strings.TrimPrefix(expect, "error: ")
					if lowerErr == nil {
						_, synthErr := synthesizeOpenAIChat(irReq)
						if synthErr == nil {
							t.Fatalf("expected error containing %q, but both lower and synthesize succeeded", substr)
						}
						assertInvalidRequestContains(t, synthErr, substr)
					} else {
						assertInvalidRequestContains(t, lowerErr, substr)
					}
				} else {
					t.Fatalf("unknown expect.txt format %q in %s", expect, caseDir)
				}
			})
		}
	})

	t.Run("codex", func(t *testing.T) {
		baseDir := filepath.Join("testdata", "real_clients", "codex")
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			t.Fatalf("read codex fixtures dir: %v", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			t.Run(e.Name(), func(t *testing.T) {
				caseDir := filepath.Join(baseDir, e.Name())
				inputBytes, err := os.ReadFile(filepath.Join(caseDir, "input.json"))
				if err != nil {
					t.Fatalf("read input.json: %v", err)
				}
				expectRaw, err := os.ReadFile(filepath.Join(caseDir, "expect.txt"))
				if err != nil {
					t.Fatalf("read expect.txt: %v", err)
				}
				expect := strings.TrimSpace(string(expectRaw))

				irReq, lowerErr := lowerOpenAIResponses(inputBytes)
				if expect == "ok" {
					if lowerErr != nil {
						t.Fatalf("expected ok, but lowerOpenAIResponses failed: %v", lowerErr)
					}
					if _, synthErr := synthesizeAnthropic(irReq, nil); synthErr != nil {
						t.Fatalf("expected ok, but synthesizeAnthropic failed: %v", synthErr)
					}
					if _, synthErr := synthesizeOpenAIChat(irReq); synthErr != nil {
						t.Fatalf("expected ok, but synthesizeOpenAIChat failed: %v", synthErr)
					}
				} else if strings.HasPrefix(expect, "error: ") {
					substr := strings.TrimPrefix(expect, "error: ")
					if lowerErr == nil {
						_, antErr := synthesizeAnthropic(irReq, nil)
						_, chatErr := synthesizeOpenAIChat(irReq)
						if antErr != nil {
							assertInvalidRequestContains(t, antErr, substr)
						} else if chatErr != nil {
							assertInvalidRequestContains(t, chatErr, substr)
						} else {
							t.Fatalf("expected error containing %q, but both lower and synthesize succeeded", substr)
						}
					} else {
						assertInvalidRequestContains(t, lowerErr, substr)
					}
				} else {
					t.Fatalf("unknown expect.txt format %q in %s", expect, caseDir)
				}
			})
		}
	})
}

func assertInvalidRequestContains(t *testing.T, err error, substr string) {
	t.Helper()
	if code := apperr.CodeOf(err); code != apperr.CodeInvalidRequest {
		t.Fatalf("expected apperr code %q, got %q (err: %v)", apperr.CodeInvalidRequest, code, err)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error message %q does not contain expected substring %q", err.Error(), substr)
	}
}
