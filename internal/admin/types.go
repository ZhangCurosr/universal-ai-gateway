// Package admin 实现管理面 HTTP API（spec/09 §4，任务卡 T12）。
package admin

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
}

// UpdateKeyRequest 是 PATCH /admin/api/keys/{id} 的请求体；State 只接受 "ready" | "disabled"。
type UpdateKeyRequest struct {
	Name        *string `json:"name"`
	Weight      *int    `json:"weight"`
	MaxInFlight *int    `json:"max_in_flight"`
	State       *string `json:"state"`
}

// KeyView 是上游 Key 的对外表示。永远不包含 secret 与密文（ADM-16）。
type KeyView struct {
	ID              int64  `json:"id"`
	Pool            string `json:"pool"`
	Name            string `json:"name"`
	SecretHint      string `json:"secret_hint"`
	State           string `json:"state"`
	StateReason     string `json:"state_reason"`
	Weight          int    `json:"weight"`
	MaxInFlight     int    `json:"max_in_flight"`
	InFlight        int    `json:"in_flight"`
	CooldownSeconds int    `json:"cooldown_seconds"`
	Breaker         string `json:"breaker"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// UsageItem 是用量汇总的一行。
type UsageItem struct {
	ClientID         int64  `json:"client_id"`
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	CostNanoUSD      int64  `json:"cost_nano_usd"`
}
