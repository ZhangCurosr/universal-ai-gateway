package main

// 调度器运行时状态的持久化（spec/07 RT-10 ~ RT-14，任务卡 T68）：启动时恢复，后台每 5 秒有变化才写库，
// 关闭时（数据面排空之后）写最后一次。只在控制面执行，数据面热路径不访问数据库。

import (
	"context"
	"log/slog"
	"reflect"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// runtimeSnapshotInterval 是快照间隔（RT-11；决策 3：固定，不开放配置）。
const runtimeSnapshotInterval = 5 * time.Second

// runtimeStateStore 是本文件用到的存储方法。
type runtimeStateStore interface {
	SaveRuntimeState(ctx context.Context, keys []store.KeyRuntime, pins []store.TaskPin) error
	LoadRuntimeState(ctx context.Context, now time.Time) ([]store.KeyRuntime, []store.TaskPin, error)
}

// restoreRuntimeState 实现 RT-10：读取失败只记 Warn（状态丢失只是回到没有持久化时的行为）。
func restoreRuntimeState(ctx context.Context, st runtimeStateStore, sched *scheduler.Scheduler, log *slog.Logger) {
	keys, pins, err := st.LoadRuntimeState(ctx, time.Now())
	if err != nil {
		log.Warn("runtime state: cannot load; starting without it", "err", err)
		return
	}
	rs := scheduler.RuntimeState{}
	for _, k := range keys {
		rs.Keys = append(rs.Keys, scheduler.KeyRuntime{KeyID: k.KeyID, CooldownUntil: k.CooldownUntil, AuthFailures: k.AuthFailures})
	}
	for _, p := range pins {
		rs.Pins = append(rs.Pins, scheduler.PinRecord{AffinityKey: p.AffinityKey, KeyID: p.KeyID, ExpiresAt: p.ExpiresAt})
	}
	cooling, pinned := sched.RestoreRuntime(rs)
	if cooling > 0 || pinned > 0 || len(keys) > 0 {
		log.Info("runtime state restored", "cooling_keys", cooling, "task_pins", pinned, "keys_with_auth_failures", countFailures(keys))
	}
}

func countFailures(keys []store.KeyRuntime) int {
	n := 0
	for _, k := range keys {
		if k.AuthFailures > 0 {
			n++
		}
	}
	return n
}

// runtimeSnapshotter 返回后台 worker（RT-11、RT-12）。ctx 取消时用独立的短超时写最后一次。
func runtimeSnapshotter(st runtimeStateStore, sched *scheduler.Scheduler, log *slog.Logger, interval time.Duration) func(context.Context) error {
	return func(ctx context.Context) error {
		var last *scheduler.RuntimeState
		save := func(ctx context.Context, force bool) {
			snap := sched.RuntimeSnapshot()
			if !force && last != nil && reflect.DeepEqual(*last, snap) {
				return
			}
			keys := make([]store.KeyRuntime, 0, len(snap.Keys))
			for _, k := range snap.Keys {
				keys = append(keys, store.KeyRuntime{KeyID: k.KeyID, CooldownUntil: k.CooldownUntil, AuthFailures: k.AuthFailures})
			}
			pins := make([]store.TaskPin, 0, len(snap.Pins))
			for _, p := range snap.Pins {
				pins = append(pins, store.TaskPin{AffinityKey: p.AffinityKey, KeyID: p.KeyID, ExpiresAt: p.ExpiresAt})
			}
			if err := st.SaveRuntimeState(ctx, keys, pins); err != nil {
				log.Warn("runtime state: snapshot not saved; will retry", "err", err)
				return
			}
			last = &snap
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				save(final, true) // RT-12：关闭时总是写最后一次
				cancel()
				return nil
			case <-t.C:
				save(ctx, false)
			}
		}
	}
}
