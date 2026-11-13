package scheduler

import (
	"bytes"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// Options 是调度器的可选依赖。
type Options struct {
	Now              func() time.Time // 为 nil 时用 time.Now
	Logger           *slog.Logger
	AffinityCapacity int // 0 = 默认 65536
	// OnQuarantine 在 Key 因连续鉴权失败被隔离时调用（ST-06）。
	// 调度器必须在不持有任何锁的情况下调用它；回调中可以做 I/O（例如写库）。
	OnQuarantine func(keyID int64, reason string)
}

// AcquireRequest 是一次选择请求。
type AcquireRequest struct {
	Pool        string
	AffinityKey string // 空 = 无亲和
	// Strict 为 true 时只能选 AffinityKey 记录的 Key：未命中返回 CodeNotFound，
	// 该 Key 不可用（禁用、冷却、熔断、满载或在 Exclude 中）返回 CodeUpstreamUnavailable（SCH-17）。
	Strict  bool
	Exclude []int64 // 本请求已尝试失败的 Key
}

// Lease 是对一个 Key 的占用。必须且只需调用一次 Finish。
type Lease struct {
	Key *Key

	s        *Scheduler
	pool     *pool
	probe    bool
	finished atomic.Bool
}

// KeyStatus 是管理 API 展示用的 Key 状态。
type KeyStatus struct {
	ID                int64
	Pool              string
	Name              string
	Persisted         PersistedState
	InFlight          int
	MaxInFlight       int
	CooldownRemaining time.Duration
	Breaker           BreakerState
	AuthFailures      int
	ExpiresAt         time.Time // 零值 = 不过期（T62）
	Expired           bool
}

// pool 是一个池的运行时状态。
type pool struct {
	cfg    PoolConfig
	mu     sync.Mutex // 保护 keys 切片替换与 SWRR 运算
	keys   []*Key     // guarded by mu
	cursor atomic.Uint64
}

// Scheduler 管理全部池。并发安全。
type Scheduler struct {
	opts     Options
	pools    atomic.Pointer[map[string]*pool] // 整体替换（SetPools，SCH-20）；读取不加锁
	affinity *affinityCache

	mu   sync.RWMutex
	byID map[int64]*Key // guarded by mu；SetKeys 整体替换
}

// New 创建调度器。pools 中的名称必须唯一（由 config.Validate 保证）。
func New(pools []PoolConfig, opts Options) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Scheduler{opts: opts, affinity: newAffinityCache(opts.AffinityCapacity), byID: map[int64]*Key{}}
	m := make(map[string]*pool, len(pools))
	for _, pc := range pools {
		m[pc.Name] = &pool{cfg: pc}
	}
	s.pools.Store(&m)
	return s
}

// SetPools 用 pools 替换全部池（SCH-20，配置热重载）：新池加入；已有池更新参数并保留其 Key 与运行时状态；
// 不在 pools 中的池停止调度，其 Key 从调度器移除。已发出的租约按发出时的池参数结束。
func (s *Scheduler) SetPools(pools []PoolConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := *s.pools.Load()
	next := make(map[string]*pool, len(pools))
	for _, pc := range pools {
		np := &pool{cfg: pc}
		if op, ok := old[pc.Name]; ok {
			op.mu.Lock()
			np.keys = op.keys
			op.mu.Unlock()
			np.cursor.Store(op.cursor.Load())
			for _, k := range np.keys {
				k.breaker.configure(pc.Breaker)
			}
		}
		next[pc.Name] = np
	}
	byID := make(map[int64]*Key, len(s.byID))
	for id, k := range s.byID {
		if _, ok := next[k.Pool]; ok {
			byID[id] = k
		}
	}
	s.byID = byID
	s.pools.Store(&next)
}

// SetKeys 用 keys 替换全部 Key，同 ID 的处理规则见 SCH-16。Pool 不存在的 Key 被忽略并记录 Warn。
func (s *Scheduler) SetKeys(keys []*Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pools := *s.pools.Load()
	byID := make(map[int64]*Key, len(keys))
	perPool := map[string][]*Key{}
	for _, k := range keys {
		p, ok := pools[k.Pool]
		if !ok {
			s.opts.Logger.Warn("scheduler: key references unknown pool; ignored", "key_id", k.ID, "pool", k.Pool)
			continue
		}
		if old, ok := s.byID[k.ID]; ok {
			if sameIdentity(old, k) {
				old.persisted.Store(k.persisted.Load())
				old.expiresAt.Store(k.expiresAt.Load()) // 过期时间是元数据，不影响身份（SCH-21）
				k = old
			} else {
				k.cooldownUntil.Store(old.cooldownUntil.Load())
				k.authFailures.Store(old.authFailures.Load())
			}
		}
		k.breaker.configure(p.cfg.Breaker)
		byID[k.ID] = k
		perPool[k.Pool] = append(perPool[k.Pool], k)
	}
	for name, p := range pools {
		ks := perPool[name]
		sort.Slice(ks, func(i, j int) bool { return ks[i].ID < ks[j].ID })
		p.mu.Lock()
		p.keys = ks
		p.mu.Unlock()
	}
	s.byID = byID
}

// sameIdentity 报告两个 Key 的导出字段是否完全相同（SCH-16）。
func sameIdentity(a, b *Key) bool {
	return a.Pool == b.Pool && a.Name == b.Name && bytes.Equal(a.Secret, b.Secret) &&
		a.Weight == b.Weight && a.MaxInFlight == b.MaxInFlight
}

// usable 实现 SCH-01 ~ SCH-05（SCH-01 的池判定由调用方保证）。
func usable(k *Key, now time.Time, exclude []int64) bool {
	if slices.Contains(exclude, k.ID) || k.Persisted() != PersistedReady || k.expired(now) {
		return false
	}
	if until := k.cooldownUntil.Load(); until > now.UnixNano() {
		return false
	}
	return k.inFlight.Load() < k.MaxInFlight && k.breaker.allow(now)
}

// Acquire 按 spec/07 §3 选择一个 Key。无可用 Key 返回 CodeUpstreamUnavailable（§3.1）。
func (s *Scheduler) Acquire(req AcquireRequest) (*Lease, error) {
	p, ok := (*s.pools.Load())[req.Pool]
	if !ok {
		return nil, apperr.New(apperr.CodeUpstreamUnavailable, "unknown pool "+req.Pool)
	}
	now := s.opts.Now()
	p.mu.Lock()
	all := p.keys
	p.mu.Unlock()

	candidates := make([]*Key, 0, len(all))
	for _, k := range all {
		if usable(k, now, req.Exclude) {
			candidates = append(candidates, k)
		}
	}

	if req.Strict { // SCH-17：不刷新 TTL，绝不回落到其他 Key
		id, ok := s.affinity.peek(req.AffinityKey, now)
		if req.AffinityKey == "" || !ok {
			return nil, apperr.New(apperr.CodeNotFound, "unknown or expired task; resubmit")
		}
		for _, k := range candidates {
			if k.ID == id {
				if l := s.claim(p, k, now); l != nil {
					return l, nil
				}
				break
			}
		}
		return nil, s.noKey(all, now)
	}

	if len(candidates) == 0 {
		return nil, s.noKey(all, now)
	}
	if req.AffinityKey != "" { // SCH-10
		if id, ok := s.affinity.Get(req.AffinityKey, now); ok {
			for _, k := range candidates {
				if k.ID == id {
					if l := s.claim(p, k, now); l != nil {
						return l, nil // Get 已刷新 TTL（SCH-13）
					}
					break
				}
			}
		}
	}
	var lease *Lease
	if p.cfg.Strategy == config.StrategyWeightedRoundRobin {
		lease = s.pickSWRR(p, candidates, now)
	} else {
		lease = s.pickLeastInflight(p, candidates, now)
	}
	if lease == nil {
		return nil, s.noKey(all, now)
	}
	if req.AffinityKey != "" {
		s.affinity.Put(req.AffinityKey, lease.Key.ID, p.cfg.AffinityTTL, now)
	}
	return lease, nil
}

// claim 占用 k 的并发槽位并经熔断器确认放行；失败时归还槽位并返回 nil。
func (s *Scheduler) claim(p *pool, k *Key, now time.Time) *Lease {
	if !k.tryAcquire() {
		return nil
	}
	probe, ok := k.breaker.onAcquire(now)
	if !ok {
		k.release()
		return nil
	}
	return &Lease{Key: k, s: s, pool: p, probe: probe}
}

// pickLeastInflight 实现 SCH-11：先按轮转游标旋转候选集（打破平局），再按负载升序、权重降序稳定排序。
func (s *Scheduler) pickLeastInflight(p *pool, c []*Key, now time.Time) *Lease {
	n := len(c)
	off := int(p.cursor.Add(1) % uint64(n))
	order := append(append(make([]*Key, 0, n), c[off:]...), c[:off]...)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		la := int64(a.inFlight.Load()) * int64(b.MaxInFlight)
		lb := int64(b.inFlight.Load()) * int64(a.MaxInFlight)
		if la != lb {
			return la < lb
		}
		return a.Weight > b.Weight
	})
	for _, k := range order {
		if l := s.claim(p, k, now); l != nil {
			return l
		}
	}
	return nil
}

// pickSWRR 实现 SCH-12（Nginx 平滑加权轮询），在池级 mutex 内只对候选集运算。
func (s *Scheduler) pickSWRR(p *pool, c []*Key, now time.Time) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := int64(0)
	for _, k := range c {
		k.swrrCurrent += int64(k.Weight)
		total += int64(k.Weight)
	}
	order := append([]*Key(nil), c...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].swrrCurrent > order[j].swrrCurrent })
	for _, k := range order {
		if l := s.claim(p, k, now); l != nil {
			k.swrrCurrent -= total
			return l
		}
	}
	return nil
}

// noKey 实现 §3.1。
func (s *Scheduler) noKey(all []*Key, now time.Time) error {
	var earliest int64
	expired := 0
	for _, k := range all {
		if k.Persisted() != PersistedReady {
			continue
		}
		if k.expired(now) {
			expired++
			continue
		}
		if until := k.cooldownUntil.Load(); until > now.UnixNano() && (earliest == 0 || until < earliest) {
			earliest = until
		}
	}
	msg := "no upstream key available"
	if expired > 0 {
		msg += fmt.Sprintf(" (%d expired)", expired)
	}
	e := apperr.New(apperr.CodeUpstreamUnavailable, msg)
	if earliest > 0 {
		wait := time.Duration(earliest - now.UnixNano())
		secs := (wait + time.Second - 1) / time.Second
		e = e.WithRetryAfter(max(secs, 1) * time.Second)
	}
	return e
}

// Finish 释放占用并按 Outcome 更新 Key 状态（spec/07 §5，ST-01 ~ ST-09）。重复调用无效。
func (l *Lease) Finish(o protocol.Outcome) {
	if !l.finished.CompareAndSwap(false, true) {
		return
	}
	k, cfg := l.Key, l.pool.cfg
	k.release()
	now := l.s.opts.Now()
	counted := false
	switch o.Kind {
	case protocol.OutcomeSuccess: // ST-01
		k.authFailures.Store(0)
		k.breaker.record(true, l.probe, now)
		counted = true
	case protocol.OutcomeRateLimited: // ST-05
		d := o.RetryAfter
		if d <= 0 {
			d = cfg.CooldownDefault
		}
		k.coolDown(now, min(max(d, time.Second), max(cfg.CooldownMax, time.Second)))
	case protocol.OutcomeAuthFailed: // ST-06
		if k.authFailures.Add(1) >= 2 {
			k.persisted.Store(int32(PersistedQuarantined))
			if cb := l.s.opts.OnQuarantine; cb != nil {
				go cb(k.ID, "upstream 401 twice") // 不持锁、异步：回调可能做 I/O
			}
		} else {
			k.coolDown(now, 60*time.Second)
		}
	case protocol.OutcomePermissionDenied: // ST-07
		k.coolDown(now, cfg.CooldownDefault)
	case protocol.OutcomeServerError, protocol.OutcomeTimeout, protocol.OutcomeNetworkError: // ST-09
		k.breaker.record(false, l.probe, now)
		counted = true
	}
	// ST-02 ~ ST-04、ST-08 不影响 Key；探测请求以不计入统计的结果结束时释放探测名额
	if l.probe && !counted {
		k.breaker.cancelProbe()
	}
}

// coolDown 把冷却截止时间延长到 now + d（只延长，不缩短）。
func (k *Key) coolDown(now time.Time, d time.Duration) {
	until := now.Add(d).UnixNano()
	for {
		cur := k.cooldownUntil.Load()
		if cur >= until || k.cooldownUntil.CompareAndSwap(cur, until) {
			return
		}
	}
}

// Pin 把 affinityKey 固定到本 Lease 的 Key，有效期 ttl（SCH-18，用于 Response.Pins）。
// 与普通亲和共用同一个缓存；可以在 Finish 之前或之后调用。
func (l *Lease) Pin(affinityKey string, ttl time.Duration) {
	if affinityKey == "" || ttl <= 0 {
		return
	}
	l.s.affinity.Put(affinityKey, l.Key.ID, ttl, l.s.opts.Now())
}

// PinnedPool 返回 affinityKey 当前固定到的 Key 所在的池（SCH-19）；未命中、已过期或该 Key 已不存在返回 ok=false。
// 不刷新 TTL。内核用它为不带服务名的严格亲和请求（如 GET /v1/responses/{id}）确定路由（CORE-11）。
func (s *Scheduler) PinnedPool(affinityKey string) (pool string, ok bool) {
	id, ok := s.affinity.peek(affinityKey, s.opts.Now())
	if !ok {
		return "", false
	}
	s.mu.RLock()
	k, ok := s.byID[id]
	s.mu.RUnlock()
	if !ok {
		return "", false
	}
	return k.Pool, true
}

// KeyStatus 返回全部 Key 的状态快照，按 ID 升序。
func (s *Scheduler) KeyStatus() []KeyStatus {
	now := s.opts.Now()
	s.mu.RLock()
	keys := make([]*Key, 0, len(s.byID))
	for _, k := range s.byID {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	out := make([]KeyStatus, 0, len(keys))
	for _, k := range keys {
		remaining := time.Duration(0)
		if until := k.cooldownUntil.Load(); until > now.UnixNano() {
			remaining = time.Duration(until - now.UnixNano())
		}
		out = append(out, KeyStatus{
			ID: k.ID, Pool: k.Pool, Name: k.Name, Persisted: k.Persisted(),
			InFlight: int(k.inFlight.Load()), MaxInFlight: int(k.MaxInFlight),
			CooldownRemaining: remaining, Breaker: k.breaker.current(now), AuthFailures: int(k.authFailures.Load()),
			ExpiresAt: k.ExpiresAt(), Expired: k.expired(now),
		})
	}
	return out
}
