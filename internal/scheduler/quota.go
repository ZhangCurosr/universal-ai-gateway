package scheduler

import (
	"sync"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

// Quota 是池级周期额度（T63，spec/07 §7.3）。
type Quota struct {
	Unit     string
	Limit    int64
	Drain    bool // true = 剩余最少的 Key 优先；false = 剩余最多的优先（spread）
	Schedule config.QuotaSchedule
}

// quotaFromConfig 转换配置；配置已通过 CFG-21 校验，解析失败时返回 nil（不限额度）。
func quotaFromConfig(q *config.QuotaConfig) *Quota {
	if q == nil {
		return nil
	}
	s, err := q.Schedule()
	if err != nil {
		return nil
	}
	return &Quota{Unit: q.Unit, Limit: q.Limit, Drain: q.Strategy != config.QuotaSpread, Schedule: s}
}

// totalPeriodStart 是 total 周期（不重置）的固定开始时刻。
var totalPeriodStart = time.Unix(0, 0)

// PeriodStart 返回 now 所在周期的开始时刻（QT-02）。
func (q *Quota) PeriodStart(now time.Time) time.Time {
	s := q.Schedule
	if s.Period == config.QuotaPeriodTotal {
		return totalPeriodStart
	}
	l := now.In(s.Loc)
	y, m, d := l.Date()
	var c time.Time
	switch s.Period {
	case config.QuotaPeriodDay:
		c = time.Date(y, m, d, s.Hour, s.Minute, 0, 0, s.Loc)
		if c.After(now) {
			c = time.Date(y, m, d-1, s.Hour, s.Minute, 0, 0, s.Loc)
		}
	case config.QuotaPeriodWeek:
		back := (int(l.Weekday()) - int(s.Weekday) + 7) % 7
		c = time.Date(y, m, d-back, s.Hour, s.Minute, 0, 0, s.Loc)
		if c.After(now) {
			c = time.Date(y, m, d-back-7, s.Hour, s.Minute, 0, 0, s.Loc)
		}
	default: // month
		c = monthDay(y, m, s.Day, s.Hour, s.Minute, s.Loc)
		if c.After(now) {
			c = monthDay(y, m-1, s.Day, s.Hour, s.Minute, s.Loc)
		}
	}
	return c
}

// NextReset 返回 now 之后的下一个重置时刻；total 周期返回零值。
func (q *Quota) NextReset(now time.Time) time.Time {
	s := q.Schedule
	if s.Period == config.QuotaPeriodTotal { // 不重置；也不需要时区
		return time.Time{}
	}
	start := q.PeriodStart(now)
	l := start.In(s.Loc)
	y, m, d := l.Date()
	switch s.Period {
	case config.QuotaPeriodDay:
		return time.Date(y, m, d+1, s.Hour, s.Minute, 0, 0, s.Loc)
	case config.QuotaPeriodWeek:
		return time.Date(y, m, d+7, s.Hour, s.Minute, 0, 0, s.Loc)
	default:
		return monthDay(y, m+1, s.Day, s.Hour, s.Minute, s.Loc)
	}
}

// monthDay 返回 y 年 m 月的第 day 天（超过当月天数时取月末）的 hh:mm；m 可以越界（按 time.Date 规则进位）。
func monthDay(y int, m time.Month, day, hh, mm int, loc *time.Location) time.Time {
	first := time.Date(y, m, 1, 0, 0, 0, 0, loc)
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(day, last), hh, mm, 0, 0, loc)
}

// quotaState 是一把 Key 在当前周期的用量（QT-02），由 mu 保护。
type quotaState struct {
	mu    sync.Mutex
	start int64 // 周期开始时刻（UnixNano）；与当前周期不同时用量视为 0
	used  int64
}

// usedAt 返回周期 start 内的用量；记录的是更早的周期时先清零。
func (st *quotaState) usedAt(start int64) int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rollTo(start)
	return st.used
}

func (st *quotaState) add(start, n int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rollTo(start)
	st.used += n
}

// rollTo 进入新周期时清零；调用方持有 mu。时钟回拨（start 更早）时保留现有计数。
func (st *quotaState) rollTo(start int64) {
	if start > st.start {
		st.start, st.used = start, 0
	}
}

func (st *quotaState) set(start, used int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.start, st.used = start, used
}

func (st *quotaState) copyFrom(o *quotaState) {
	o.mu.Lock()
	start, used := o.start, o.used
	o.mu.Unlock()
	st.set(start, used)
}

// quotaOf 返回 key 所在池的额度；没有额度或池不存在时返回 nil。
func (s *Scheduler) quotaOf(k *Key) *Quota {
	if p, ok := (*s.pools.Load())[k.Pool]; ok {
		return p.cfg.Quota
	}
	return nil
}

// RecordUsage 把一次结算的用量计入 keyID 的本期额度（QT-03）：只取额度单位上的数量；
// Key 不存在、池没有额度或数量 <= 0 时忽略。不做 I/O，只锁这把 Key。
func (s *Scheduler) RecordUsage(keyID int64, usage map[string]int64) {
	s.mu.RLock()
	k, ok := s.byID[keyID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	q := s.quotaOf(k)
	if q == nil {
		return
	}
	if n := usage[q.Unit]; n > 0 {
		k.quota.add(q.PeriodStart(s.opts.Now()).UnixNano(), n)
	}
}

// SetQuotaUsage 用重算结果覆盖 pool 中各 Key 的本期用量（QT-06）；used 中没有的 Key 记为 0。
// periodStart 必须是重算时使用的周期开始时刻；与当前周期不同（重算期间跨过了重置时刻）时不做任何事并返回 false。
func (s *Scheduler) SetQuotaUsage(pool string, periodStart time.Time, used map[int64]int64) bool {
	p, ok := (*s.pools.Load())[pool]
	if !ok || p.cfg.Quota == nil || !p.cfg.Quota.PeriodStart(s.opts.Now()).Equal(periodStart) {
		return false
	}
	p.mu.Lock()
	keys := p.keys
	p.mu.Unlock()
	for _, k := range keys {
		k.quota.set(periodStart.UnixNano(), max(used[k.ID], 0))
	}
	return true
}

// QuotaPools 返回声明了额度的池及其额度，供装配代码在启动与重载时重算（QT-10、QT-11）。
func (s *Scheduler) QuotaPools() map[string]Quota {
	out := map[string]Quota{}
	for name, p := range *s.pools.Load() {
		if p.cfg.Quota != nil {
			out[name] = *p.cfg.Quota
		}
	}
	return out
}

// PoolKeyIDs 返回 pool 中全部 Key 的 ID（升序）。
func (s *Scheduler) PoolKeyIDs(pool string) []int64 {
	p, ok := (*s.pools.Load())[pool]
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]int64, 0, len(p.keys))
	for _, k := range p.keys {
		ids = append(ids, k.ID)
	}
	return ids
}

// quotaLeft 返回 k 在本期的剩余额度（可能为负：软上限，QT-04）。
func quotaLeft(q *Quota, k *Key, start int64) int64 {
	return q.Limit - k.quota.usedAt(start)
}
