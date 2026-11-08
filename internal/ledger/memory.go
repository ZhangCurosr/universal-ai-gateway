package ledger

import (
	"sync"
	"sync/atomic"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// MemoryConfig 配置内存账本。
type MemoryConfig struct {
	EventQueueCapacity     int     // 计费事件 channel 容量
	AdmissionHighWatermark float64 // (0,1]，队列占用达到该比例时 Reserve 返回 CodeOverloaded（LED-04）
}

// Memory 是 Standalone 部署的内存账本。
type Memory struct {
	cfg    MemoryConfig
	events chan store.UsageEvent

	mu       sync.RWMutex
	accounts map[int64]*account // guarded by mu（map 本身；account 内部字段为原子）
}

type account struct {
	unlimited atomic.Bool
	quota     atomic.Int64
	spent     atomic.Int64
	reserved  atomic.Int64
}

var _ Ledger = (*Memory)(nil)

// NewMemory 创建内存账本。
func NewMemory(cfg MemoryConfig) *Memory {
	if cfg.EventQueueCapacity <= 0 {
		cfg.EventQueueCapacity = 4096
	}
	if cfg.AdmissionHighWatermark <= 0 || cfg.AdmissionHighWatermark > 1 {
		cfg.AdmissionHighWatermark = 0.9
	}
	return &Memory{
		cfg:      cfg,
		events:   make(chan store.UsageEvent, cfg.EventQueueCapacity),
		accounts: map[int64]*account{},
	}
}

// Events 返回计费事件 channel，供 Flusher 消费。
func (m *Memory) Events() <-chan store.UsageEvent { return m.events }

// LoadAccounts 在启动时加载全部账户（覆盖已有账户）。
func (m *Memory) LoadAccounts(accts []Account) {
	// TODO(T05)
}

// UpsertAccount 实现 LED-10。
func (m *Memory) UpsertAccount(a Account) {
	// TODO(T05)
}

// Snapshot 返回账户当前状态；账户不存在返回 ok=false。
func (m *Memory) Snapshot(clientID int64) (AccountSnapshot, bool) {
	// TODO(T05)
	return AccountSnapshot{}, false
}

// Reserve 实现 LED-02 ~ LED-05。
func (m *Memory) Reserve(clientID int64, requestID string, amount int64) (*Reservation, error) {
	// TODO(T05)
	return nil, apperr.NotImplemented("T05")
}

// Settle 实现 LED-06 ~ LED-08、LED-11。
func (m *Memory) Settle(r *Reservation, ev store.UsageEvent) error {
	// TODO(T05)
	return apperr.NotImplemented("T05")
}

// Release 实现 LED-08、LED-09。
func (m *Memory) Release(r *Reservation) {
	// TODO(T05)
}
