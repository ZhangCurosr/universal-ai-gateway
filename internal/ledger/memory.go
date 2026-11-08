package ledger

import (
	"sync"
	"sync/atomic"
	"time"

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
	fresh := make(map[int64]*account, len(accts))
	for _, a := range accts {
		fresh[a.ClientID] = newAccount(a)
	}
	m.mu.Lock()
	m.accounts = fresh
	m.mu.Unlock()
}

func newAccount(a Account) *account {
	acc := &account{}
	acc.unlimited.Store(a.Unlimited)
	acc.quota.Store(a.QuotaNanoUSD)
	acc.spent.Store(a.SpentNanoUSD)
	return acc
}

// UpsertAccount 实现 LED-10：已存在的账户只更新额度与 unlimited，spent 与在途预留保持不变；
// 不存在则新建（spent 取参数值）。
func (m *Memory) UpsertAccount(a Account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if acc, ok := m.accounts[a.ClientID]; ok {
		acc.quota.Store(a.QuotaNanoUSD)
		acc.unlimited.Store(a.Unlimited)
		return
	}
	m.accounts[a.ClientID] = newAccount(a)
}

func (m *Memory) account(clientID int64) (*account, bool) {
	m.mu.RLock()
	acc, ok := m.accounts[clientID]
	m.mu.RUnlock()
	return acc, ok
}

// Snapshot 返回账户当前状态；账户不存在返回 ok=false。
func (m *Memory) Snapshot(clientID int64) (AccountSnapshot, bool) {
	acc, ok := m.account(clientID)
	if !ok {
		return AccountSnapshot{}, false
	}
	return AccountSnapshot{
		ClientID:        clientID,
		Unlimited:       acc.unlimited.Load(),
		QuotaNanoUSD:    acc.quota.Load(),
		SpentNanoUSD:    acc.spent.Load(),
		ReservedNanoUSD: acc.reserved.Load(),
	}, true
}

// Reserve 实现 LED-02 ~ LED-05。
func (m *Memory) Reserve(clientID int64, requestID string, amount int64) (*Reservation, error) {
	acc, ok := m.account(clientID)
	if !ok {
		return nil, apperr.New(apperr.CodeUnauthenticated, "client account not found")
	}
	if float64(len(m.events)) >= float64(cap(m.events))*m.cfg.AdmissionHighWatermark {
		return nil, apperr.New(apperr.CodeOverloaded, "billing pipeline is saturated, retry later").WithRetryAfter(time.Second)
	}
	if amount <= 0 {
		amount = 1 // LED-01：每个请求都有预留记录
	}
	for {
		reserved := acc.reserved.Load()
		if !acc.unlimited.Load() {
			// 用减法比较，避免 spent + reserved + amount 溢出
			if acc.quota.Load()-acc.spent.Load()-reserved < amount {
				return nil, apperr.New(apperr.CodeInsufficientQuota, "insufficient quota")
			}
		}
		if acc.reserved.CompareAndSwap(reserved, reserved+amount) {
			return &Reservation{ClientID: clientID, RequestID: requestID, Amount: amount}, nil
		}
	}
}

// Settle 实现 LED-06 ~ LED-08、LED-11。事件队列满时阻塞（LED-07）。
func (m *Memory) Settle(r *Reservation, ev store.UsageEvent) error {
	if ev.CostNanoUSD < 0 {
		return apperr.New(apperr.CodeInternal, "negative cost in usage event")
	}
	if !r.tryFinish() {
		return ErrAlreadyFinished
	}
	if acc, ok := m.account(r.ClientID); ok {
		acc.reserved.Add(-r.Amount)
		acc.spent.Add(ev.CostNanoUSD)
	}
	// 账户在请求期间被删除时仍然发送事件：上游已经计费，事件不可丢（AGENTS §3 #4）。
	m.events <- ev
	return nil
}

// Release 实现 LED-08、LED-09。
func (m *Memory) Release(r *Reservation) {
	if !r.tryFinish() {
		return
	}
	if acc, ok := m.account(r.ClientID); ok {
		acc.reserved.Add(-r.Amount)
	}
}
