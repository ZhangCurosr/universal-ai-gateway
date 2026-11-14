// Package admin 实现管理面 HTTP API（spec/09 §4，任务卡 T12）。
package admin

import "encoding/json"

// CreateClientRequest 是 POST /admin/api/clients 的请求体。
type CreateClientRequest struct {
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	QuotaUnlimited bool     `json:"quota_unlimited"`
	QuotaNanoUSD   int64    `json:"quota_nano_usd"`
	RPMLimit       *int     `json:"rpm_limit"`
	AllowedModels  []string `json:"allowed_models"`
}

// UpdateClientRequest 是 PATCH /admin/api/clients/{id} 的请求体；nil 字段不修改。
type UpdateClientRequest struct {
	Name           *string   `json:"name"`
	Role           *string   `json:"role"`
	QuotaUnlimited *bool     `json:"quota_unlimited"`
	QuotaNanoUSD   *int64    `json:"quota_nano_usd"`
	RPMLimit       *int      `json:"rpm_limit"`
	AllowedModels  *[]string `json:"allowed_models"`
	Disabled       *bool     `json:"disabled"`
}

// ClientView 是客户端的对外表示。永远不包含 token_hash（ADM-16）。
type ClientView struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Role            string   `json:"role"`
	TokenDisplay    string   `json:"token_display"`
	QuotaUnlimited  bool     `json:"quota_unlimited"`
	QuotaNanoUSD    int64    `json:"quota_nano_usd"`
	SpentNanoUSD    int64    `json:"spent_nano_usd"`
	ReservedNanoUSD int64    `json:"reserved_nano_usd"`
	RPMLimit        int      `json:"rpm_limit"`
	AllowedModels   []string `json:"allowed_models"`
	Disabled        bool     `json:"disabled"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

// CreateKeyRequest 是 POST /admin/api/keys 的请求体。
type CreateKeyRequest struct {
	Pool        string `json:"pool"`
	Name        string `json:"name"`
	Secret      string `json:"secret"`
	Weight      *int   `json:"weight"`
	MaxInFlight *int   `json:"max_in_flight"`
	ExpiresAt   string `json:"expires_at"` // RFC 3339，可空（T62）
	Label       string `json:"label"`      // 来源标签，可空（T62）
}

// UpdateKeyRequest 是 PATCH /admin/api/keys/{id} 的请求体；State 只接受 "ready" | "disabled"。
type UpdateKeyRequest struct {
	Name        *string `json:"name"`
	Weight      *int    `json:"weight"`
	MaxInFlight *int    `json:"max_in_flight"`
	State       *string `json:"state"`
	// ExpiresAt 省略 = 不改；null = 清除；字符串 = RFC 3339（T62）
	ExpiresAt json.RawMessage `json:"expires_at,omitempty"`
	Label     *string         `json:"label"`
}

// KeyView 是上游 Key 的对外表示。永远不包含 secret 与密文（ADM-16）。
type KeyView struct {
	ID              int64  `json:"id"`
	Pool            string `json:"pool"`
	Name            string `json:"name"`
	SecretHint      string `json:"secret_hint"`
	State           string `json:"state"`
	StoredState     string `json:"stored_state,omitempty"` // state 为 orphaned 时，库中的状态（HR-19）
	StateReason     string `json:"state_reason"`
	Weight          int    `json:"weight"`
	MaxInFlight     int    `json:"max_in_flight"`
	InFlight        int    `json:"in_flight"`
	CooldownSeconds int    `json:"cooldown_seconds"`
	Breaker         string `json:"breaker"`
	ExpiresAt       string `json:"expires_at,omitempty"` // RFC 3339；不过期时省略（T62）
	Label           string `json:"label"`
	// 周期额度（T63，ADM-25）：池没有额度时三项都省略
	QuotaUsed    int64  `json:"quota_used,omitempty"`
	QuotaLimit   int64  `json:"quota_limit,omitempty"`
	QuotaResetAt string `json:"quota_reset_at,omitempty"` // RFC 3339；total 周期省略
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// UsageItem 是用量汇总的一行。
type UsageItem struct {
	ClientID    int64            `json:"client_id"`
	Model       string           `json:"model"`
	Requests    int64            `json:"requests"`
	Usage       map[string]int64 `json:"usage"` // 计量单位 → 数量，如 {"input_tokens":120,"pages":30}
	CostNanoUSD int64            `json:"cost_nano_usd"`
}

// ToolView 是 GET /admin/api/tools 中的一个工具（ADM-20）。
type ToolView struct {
	Name     string `json:"name"`
	Title    string `json:"title,omitempty"`
	Summary  string `json:"summary"`           // 描述首行
	Service  string `json:"service,omitempty"` // 工具固定的服务名；空 = 由参数选择
	Protocol string `json:"protocol"`
	ReadOnly bool   `json:"read_only"`
}

// ToolsView 是 GET /admin/api/tools 的响应（ADM-20）。
type ToolsView struct {
	ToolMode string     `json:"tool_mode"` // mcp.tool_mode；MCP 关闭时为 "disabled"
	Tools    []ToolView `json:"tools"`
}
