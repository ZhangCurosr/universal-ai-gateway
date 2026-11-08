// Package ledger 实现预留-结算账本与计费事件刷盘（spec/06）。
package ledger

import (
	"errors"
	"sync/atomic"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// ErrAlreadyFinished 表示对同一个 Reservation 第二次调用 Settle（LED-08）。
var ErrAlreadyFinished = errors.New("ledger: reservation already finished")

// Ledger 是数据面使用的账本接口。实现必须并发安全且不做 I/O。
type Ledger interface {
	// Reserve 冻结 amount（nano-USD）。失败返回 *apperr.Error：
	// CodeUnauthenticated（账户不存在）、CodeInsufficientQuota、CodeOverloaded。
	Reserve(clientID int64, requestID string, amount int64) (*Reservation, error)
	// Settle 释放预留并按 ev.CostNanoUSD 扣费，产生一条计费事件（LED-06 ~ LED-08、LED-11）。
	Settle(r *Reservation, ev store.UsageEvent) error
	// Release 释放预留，不扣费、不产生事件（LED-09）。
	Release(r *Reservation)
}

// Reservation 是一次预留。只能结束一次（Settle 或 Release）。
type Reservation struct {
	ClientID  int64
	RequestID string
	Amount    int64

	finished atomic.Bool
}

// tryFinish 把 Reservation 标记为已结束；已结束过返回 false。实现 LED-08 时使用。
func (r *Reservation) tryFinish() bool {
	return r.finished.CompareAndSwap(false, true)
}

// Account 是加载到账本中的账户初始状态。
type Account struct {
	ClientID     int64
	Unlimited    bool
	QuotaNanoUSD int64
	SpentNanoUSD int64
}

// AccountSnapshot 是账户当前状态（管理 API 展示用）。
type AccountSnapshot struct {
	ClientID        int64
	Unlimited       bool
	QuotaNanoUSD    int64
	SpentNanoUSD    int64
	ReservedNanoUSD int64
}
