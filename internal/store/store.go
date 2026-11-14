// Package store 定义持久化接口与记录类型（spec/04 §4）。实现在 internal/store/sqlite。
//
// 只有控制面（cmd/gateway、admin、ledger.Flusher、audit.Writer、auth.Reloader）可以调用本接口；
// 数据面请求路径禁止访问存储（ARCH-01）。
package store

import (
	"context"
	"errors"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
)

// 哨兵错误。实现必须返回（或包装）这些值，调用方用 errors.Is 判断。
var (
	ErrNotFound        = errors.New("store: not found")
	ErrConflict        = errors.New("store: conflict")
	ErrInvalidArgument = errors.New("store: invalid argument")
)

// 客户端角色。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// 上游 Key 的持久化状态（瞬时状态如冷却、熔断只在内存）。
const (
	KeyStateReady       = "ready"
	KeyStateQuarantined = "quarantined"
	KeyStateDisabled    = "disabled"
)

// 用量来源。
const (
	UsageSourceUpstream  = "upstream"
	UsageSourceEstimated = "estimated"
)

// Client 是 clients 表的一行。
type Client struct {
	ID             int64
	Name           string
	Role           string
	TokenHash      string
	TokenDisplay   string
	QuotaUnlimited bool
	QuotaNanoUSD   int64
	RPMLimit       int
	AllowedModels  []string
	Disabled       bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UpstreamKey 是 upstream_keys 表的一行。
type UpstreamKey struct {
	ID          int64
	Pool        string
	Name        string
	Fingerprint string
	SecretHint  string
	Sealed      crypto.Sealed
	Weight      int
	MaxInFlight int
	State       string
	StateReason string
	ExpiresAt   time.Time // 零值 = 不过期（T62）
	Label       string    // 来源标签，可空（T62）
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// UpstreamKeyUpdate 中为 nil 的字段不修改（STO-08）。
type UpstreamKeyUpdate struct {
	Name        *string
	Weight      *int
	MaxInFlight *int
	ExpiresAt   *time.Time // 指向零值表示清除过期时间（T62）
	Label       *string
}

// KeyRuntime 是一把 Key 的运行时状态（T68）：冷却截止时间（零值 = 没有冷却）与连续鉴权失败次数。
type KeyRuntime struct {
	KeyID         int64
	CooldownUntil time.Time
	AuthFailures  int
}

// TaskPin 是一条任务固定记录（T68）。AffinityKey 是内核算出的亲和键（哈希），不含任务 ID 明文。
type TaskPin struct {
	AffinityKey string
	KeyID       int64
	ExpiresAt   time.Time
}

// SealFunc 在插入事务中、拿到自增 id 之后被调用，返回该行的密文（STO-06）。
type SealFunc func(id int64) (crypto.Sealed, error)

// UsageEvent 是一条计费事件（usage_events 表）。
type UsageEvent struct {
	RequestID   string
	ClientID    int64
	KeyID       int64
	Model       string
	Op          string           // 操作名（protocol.Envelope.Op）
	Usage       map[string]int64 // 计量单位 → 数量（只含 > 0 的单位）
	CostNanoUSD int64
	UsageSource string
	CreatedAt   time.Time
}

// UsageQuery 是用量汇总查询条件，时间区间为 [From, To)。ClientID 为 0 表示全部客户端。
type UsageQuery struct {
	ClientID int64
	From     time.Time
	To       time.Time
}

// UsageSummary 是按 (client_id, model) 分组的汇总。
type UsageSummary struct {
	ClientID    int64
	Model       string
	Requests    int64
	Usage       map[string]int64 // 各计量单位之和；没有用量时为空 map（非 nil）
	CostNanoUSD int64
}

// AuditRecord 是一条审计记录（audit_records 表）。不包含任何请求/响应正文。
type AuditRecord struct {
	RequestID        string
	ClientID         int64
	KeyID            int64
	Model            string
	Op               string
	IngressProtocol  string
	UpstreamProtocol string
	Status           int
	ErrorCode        string
	Outcome          string // 最后一次尝试的 protocol.OutcomeKind.String()；未尝试为 ""
	Attempts         int
	Stream           bool
	TTFBMillis       int64 // 无则 -1
	LatencyMillis    int64
	CreatedAt        time.Time
}

// Store 是全部持久化操作。方法语义见 spec/04 §4.1（STO-01 ~ STO-16）。
type Store interface {
	CreateClient(ctx context.Context, c *Client) (int64, error)
	GetClient(ctx context.Context, id int64) (*Client, error)
	ListClients(ctx context.Context) ([]Client, error)
	UpdateClient(ctx context.Context, c *Client) error
	CountAdmins(ctx context.Context) (int, error)

	InsertUpstreamKey(ctx context.Context, k *UpstreamKey, seal SealFunc) (int64, error)
	ListUpstreamKeys(ctx context.Context) ([]UpstreamKey, error)
	GetUpstreamKey(ctx context.Context, id int64) (*UpstreamKey, error)
	UpdateUpstreamKeyMeta(ctx context.Context, id int64, upd UpstreamKeyUpdate) error
	SetUpstreamKeyState(ctx context.Context, id int64, state, reason string) error

	InsertUsageEvents(ctx context.Context, events []UsageEvent) error
	SumCostByClient(ctx context.Context) (map[int64]int64, error)
	QueryUsage(ctx context.Context, q UsageQuery) ([]UsageSummary, error)

	InsertAuditRecords(ctx context.Context, recs []AuditRecord) error
	DeleteAuditBefore(ctx context.Context, before time.Time) (int64, error)

	// SaveRuntimeState 用一个事务整体替换调度器运行时状态的快照（STO-18，T68）。
	SaveRuntimeState(ctx context.Context, keys []KeyRuntime, pins []TaskPin) error
	// LoadRuntimeState 返回快照中在 now 时仍有效的部分（STO-19，T68）。
	LoadRuntimeState(ctx context.Context, now time.Time) ([]KeyRuntime, []TaskPin, error)
	// SumKeyUsage 返回 keyIDs 中各 Key 自 since 起在计量单位 unit 上的用量之和（STO-21，T63）；没有用量的 Key 不出现。
	SumKeyUsage(ctx context.Context, unit string, since time.Time, keyIDs []int64) (map[int64]int64, error)
	// SumUsageByKey 按 Key 汇总 [from, to) 内的计费事件（STO-22，T65）；没有事件的 Key 不出现。to - from 不得超过 31 天。
	SumUsageByKey(ctx context.Context, from, to time.Time, keyIDs []int64) ([]KeyUsage, error)

	GetSetting(ctx context.Context, key string) (string, bool, error)
	PutSetting(ctx context.Context, key, value string) error

	Close() error
}

// KeyUsage 是一把 Key 在一段时间内的用量汇总（STO-22，T65）。
type KeyUsage struct {
	KeyID       int64
	Requests    int64            // 计费事件数
	Usage       map[string]int64 // 各计量单位之和
	CostNanoUSD int64
}

// MaxKeyUsageRange 是 SumUsageByKey 允许的最长时间跨度。
const MaxKeyUsageRange = 31 * 24 * time.Hour
