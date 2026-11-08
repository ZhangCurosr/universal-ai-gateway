// Package ir 定义跨协议转译使用的规范化中间表示（M2，spec/10 §2）。
//
// M1 不使用本包；类型在此预先定义，供 M2 的 translate 包实现。
package ir

import (
	"encoding/json"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
)

// Role 是消息角色。system 提示放在 Request.System，不作为消息。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Vendor 标识思考内容的签发厂商；签名只能回传给同一厂商（spec/10 §1）。
type Vendor string

const (
	VendorAnthropic Vendor = "anthropic"
	VendorOpenAI    Vendor = "openai"
	VendorDeepSeek  Vendor = "deepseek"
	VendorGemini    Vendor = "gemini"
)

// Request 是协议中立的请求。
type Request struct {
	Model         string
	System        []TextPart
	Messages      []Message
	Tools         []Tool
	ToolChoice    *ToolChoice
	MaxTokens     int64 // 0 = 未指定
	Temperature   *float64
	TopP          *float64
	StopSequences []string
	Stream        bool
	User          string
}

// Message 是一条消息。
type Message struct {
	Role  Role
	Parts []Part
}

// Part 是消息内容块。实现：TextPart、ImagePart、ToolCallPart、ToolResultPart、ThinkingPart。
type Part interface{ isPart() }

// CacheControl 对应 Anthropic 的 cache_control。
type CacheControl struct {
	Type string // "ephemeral"
	TTL  string // "" | "5m" | "1h"
}

type TextPart struct {
	Text         string
	CacheControl *CacheControl
}

type ImagePart struct {
	MediaType string // 如 "image/png"
	Data      []byte // base64 解码后的字节；与 URL 二选一
	URL       string
}

type ToolCallPart struct {
	ID        string
	Name      string
	Arguments json.RawMessage // 必须是合法 JSON 对象
}

type ToolResultPart struct {
	ToolCallID string
	Content    []Part // 仅 TextPart / ImagePart
	IsError    bool
}

type ThinkingPart struct {
	Vendor       Vendor
	Text         string
	Signature    string // Anthropic / Gemini 的签名
	Redacted     bool
	RedactedData string // Anthropic redacted_thinking.data
	Encrypted    string // OpenAI reasoning.encrypted_content
}

func (TextPart) isPart()       {}
func (ImagePart) isPart()      {}
func (ToolCallPart) isPart()   {}
func (ToolResultPart) isPart() {}
func (ThinkingPart) isPart()   {}

// Tool 是工具定义。
type Tool struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	CacheControl *CacheControl
}

// ToolChoice.Mode 取值：auto | any | none | tool（tool 时 Name 必填）。
type ToolChoice struct {
	Mode string
	Name string
}

// StopReason 是规范化的结束原因。
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopMaxTokens StopReason = "max_tokens"
	StopToolUse   StopReason = "tool_use"
	StopSequence  StopReason = "stop_sequence"
	StopRefusal   StopReason = "refusal"
)

// Response 是协议中立的非流式响应。
type Response struct {
	ID         string
	Model      string
	Parts      []Part
	StopReason StopReason
	Usage      pricing.Usage
}
