package scheduler

import (
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// RT-01、RT-02：快照只含冷却中、有失败计数的 Key 与任务固定（不含普通亲和、过期条目）
func TestRuntimeSnapshot(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10), key(2, 1, 10), key(3, 1, 10))
	l := mustAcquire(t, h.s, AcquireRequest{Pool: "p", Exclude: []int64{2, 3}})
	l.Pin("c1:j:task", time.Hour)
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeRateLimited, RetryAfter: 5 * time.Minute})
	l = mustAcquire(t, h.s, AcquireRequest{Pool: "p", Exclude: []int64{1, 3}})
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeAuthFailed}) // 第一次 401：冷却 60 秒、计数 1
	l = mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "c1:u:session", Exclude: []int64{1, 2}})
	l.Pin("c1:j:short", time.Second)
	l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	h.c.Advance(2 * time.Second)

	st := h.s.RuntimeSnapshot()
	if len(st.Keys) != 2 || st.Keys[0].KeyID != 1 || !st.Keys[0].CooldownUntil.Equal(t0.Add(5*time.Minute)) ||
		st.Keys[1].KeyID != 2 || st.Keys[1].AuthFailures != 1 {
		t.Fatalf("keys = %+v", st.Keys)
	}
	if len(st.Pins) != 1 || st.Pins[0].AffinityKey != "c1:j:task" || st.Pins[0].KeyID != 1 || !st.Pins[0].ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("pins = %+v (session affinity and expired pins must not appear)", st.Pins)
	}
	h.c.Advance(10 * time.Minute) // 冷却结束：Key 1 不再出现；Key 2 仍有失败计数
	if st := h.s.RuntimeSnapshot(); len(st.Keys) != 1 || st.Keys[0].KeyID != 2 || !st.Keys[0].CooldownUntil.IsZero() {
		t.Fatalf("after cooldown = %+v", st.Keys)
	}
}

// RT-03：恢复后继续冷却、严格亲和命中同一 Key；不存在的 Key 忽略；冷却按当前上限截断；只延长不缩短
func TestRestoreRuntime(t *testing.T) {
	h := newHarness(t, config.StrategyLeastInflight, key(1, 1, 10), key(2, 1, 10))
	cooling, pins := h.s.RestoreRuntime(RuntimeState{
		Keys: []KeyRuntime{
			{KeyID: 1, CooldownUntil: t0.Add(24 * time.Hour), AuthFailures: 1}, // 超过 cooldown.max（10 分钟）
			{KeyID: 2, CooldownUntil: t0.Add(-time.Minute)},                    // 已结束
			{KeyID: 99, CooldownUntil: t0.Add(time.Hour)},                      // 不存在
		},
		Pins: []PinRecord{
			{AffinityKey: "c1:j:task", KeyID: 2, ExpiresAt: t0.Add(time.Hour)},
			{AffinityKey: "c1:j:gone", KeyID: 99, ExpiresAt: t0.Add(time.Hour)},
			{AffinityKey: "c1:j:old", KeyID: 2, ExpiresAt: t0.Add(-time.Second)},
		},
	})
	if cooling != 1 || pins != 1 {
		t.Fatalf("restored %d cooling, %d pins", cooling, pins)
	}
	st := h.s.KeyStatus()
	if st[0].CooldownRemaining != 10*time.Minute || st[0].AuthFailures != 1 || st[1].CooldownRemaining != 0 {
		t.Fatalf("status = %+v", st)
	}
	for i := 0; i < 3; i++ {
		l := mustAcquire(t, h.s, AcquireRequest{Pool: "p", AffinityKey: "c1:j:task", Strict: true})
		if l.Key.ID != 2 {
			t.Fatalf("strict pin after restore got key %d", l.Key.ID)
		}
		l.Finish(protocol.Outcome{Kind: protocol.OutcomeSuccess})
	}
	// 恢复的固定记录会再次出现在快照中（重启多次也不丢）
	if snap := h.s.RuntimeSnapshot(); len(snap.Pins) != 1 || snap.Pins[0].AffinityKey != "c1:j:task" {
		t.Fatalf("restored pin not in snapshot: %+v", snap.Pins)
	}
	// 只延长不缩短：已有更长的冷却时不被缩短
	h.s.RestoreRuntime(RuntimeState{Keys: []KeyRuntime{{KeyID: 1, CooldownUntil: t0.Add(time.Minute)}}})
	if st := h.s.KeyStatus(); st[0].CooldownRemaining != 10*time.Minute {
		t.Fatalf("cooldown shortened: %v", st[0].CooldownRemaining)
	}
}

// RT-01：普通亲和覆盖固定条目时不取消固定标记
func TestPinFlagSurvivesPut(t *testing.T) {
	c := newAffinityCache(16)
	c.pin("k", 1, time.Hour, t0)
	c.Put("k", 1, time.Minute, t0)
	if got := c.pinnedEntries(t0); len(got) != 1 {
		t.Fatalf("pinned entries = %+v", got)
	}
	c.Put("s", 2, time.Hour, t0)
	if got := c.pinnedEntries(t0); len(got) != 1 {
		t.Fatalf("session affinity counted as pinned: %+v", got)
	}
}
