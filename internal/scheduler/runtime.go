package scheduler

// 运行时状态的快照与恢复（spec/07 RT-01 ~ RT-04，任务卡 T68）。只读写内存；数据库读写由装配代码负责。

import (
	"sort"
	"time"
)

// KeyRuntime 是一把 Key 需要跨重启保留的状态：冷却截止时间（零值 = 没有冷却）与连续鉴权失败次数。
type KeyRuntime struct {
	KeyID         int64
	CooldownUntil time.Time
	AuthFailures  int
}

// PinRecord 是一条任务固定记录。AffinityKey 是内核算出的亲和键（哈希）。
type PinRecord struct {
	AffinityKey string
	KeyID       int64
	ExpiresAt   time.Time
}

// RuntimeState 是调度器运行时状态的快照。Keys 按 KeyID、Pins 按 AffinityKey 升序，便于比较是否变化。
type RuntimeState struct {
	Keys []KeyRuntime
	Pins []PinRecord
}

// RuntimeSnapshot 实现 RT-02：冷却未结束或有失败计数的 Key，以及未过期的任务固定。不阻塞 Acquire。
func (s *Scheduler) RuntimeSnapshot() RuntimeState {
	now := s.opts.Now()
	s.mu.RLock()
	keys := make([]*Key, 0, len(s.byID))
	for _, k := range s.byID {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	var st RuntimeState
	for _, k := range keys {
		until := k.cooldownUntil.Load()
		fails := int(k.authFailures.Load())
		if until <= now.UnixNano() {
			until = 0
		}
		if until == 0 && fails == 0 {
			continue
		}
		kr := KeyRuntime{KeyID: k.ID, AuthFailures: fails}
		if until != 0 {
			kr.CooldownUntil = time.Unix(0, until)
		}
		st.Keys = append(st.Keys, kr)
	}
	sort.Slice(st.Keys, func(i, j int) bool { return st.Keys[i].KeyID < st.Keys[j].KeyID })
	st.Pins = s.affinity.pinnedEntries(now)
	sort.Slice(st.Pins, func(i, j int) bool { return st.Pins[i].AffinityKey < st.Pins[j].AffinityKey })
	return st
}

// RestoreRuntime 实现 RT-03：只恢复当前存在的 Key；冷却截止时间不晚于「现在 + 池当前的 cooldown.max」，
// 只延长不缩短；固定记录按剩余有效期写回并标记为固定。返回恢复的冷却 Key 数与固定记录数。
func (s *Scheduler) RestoreRuntime(st RuntimeState) (cooling, pins int) {
	now := s.opts.Now()
	pools := *s.pools.Load()
	s.mu.RLock()
	byID := s.byID
	s.mu.RUnlock()
	for _, kr := range st.Keys {
		k, ok := byID[kr.KeyID]
		if !ok {
			continue
		}
		if !kr.CooldownUntil.IsZero() && kr.CooldownUntil.After(now) {
			until := kr.CooldownUntil
			if p, ok := pools[k.Pool]; ok && p.cfg.CooldownMax > 0 {
				if limit := now.Add(p.cfg.CooldownMax); until.After(limit) {
					until = limit
				}
			}
			k.coolDown(now, until.Sub(now))
			cooling++
		}
		if f := int32(kr.AuthFailures); f > k.authFailures.Load() {
			k.authFailures.Store(f)
		}
	}
	for _, p := range st.Pins {
		if _, ok := byID[p.KeyID]; !ok || !p.ExpiresAt.After(now) {
			continue
		}
		s.affinity.pin(p.AffinityKey, p.KeyID, p.ExpiresAt.Sub(now), now)
		pins++
	}
	return cooling, pins
}
