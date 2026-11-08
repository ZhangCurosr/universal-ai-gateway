package scheduler

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"
)

const (
	affinityShards          = 16
	defaultAffinityCapacity = 65536
)

// affinityCache 是分片 LRU + TTL 的亲和缓存（spec/07 §4，AFF-01 ~ AFF-06）。
type affinityCache struct {
	shards [affinityShards]affinityShard
}

type affinityShard struct {
	mu    sync.Mutex
	cap   int                      // guarded by mu
	items map[string]*list.Element // guarded by mu
	lru   *list.List               // guarded by mu；Front = 最近使用
}

type affinityEntry struct {
	key      string
	keyID    int64
	ttl      time.Duration
	expireAt time.Time
}

// newAffinityCache 创建缓存；capacity <= 0 时使用默认容量。
func newAffinityCache(capacity int) *affinityCache {
	if capacity <= 0 {
		capacity = defaultAffinityCapacity
	}
	c := &affinityCache{}
	per := (capacity + affinityShards - 1) / affinityShards
	for i := range c.shards {
		c.shards[i] = affinityShard{cap: per, items: map[string]*list.Element{}, lru: list.New()}
	}
	return c
}

func (c *affinityCache) shard(key string) *affinityShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key)) // hash.Hash 的 Write 永不返回错误
	return &c.shards[h.Sum32()%affinityShards]
}

// Get 实现 AFF-03、AFF-04：命中且未过期时刷新到期时间与 LRU 位置；过期条目在此删除。
func (c *affinityCache) Get(key string, now time.Time) (int64, bool) {
	return c.lookup(key, now, true)
}

// peek 与 Get 相同但不刷新 TTL 与 LRU 位置（严格亲和、PinnedPool 使用，SCH-17、SCH-19）。
func (c *affinityCache) peek(key string, now time.Time) (int64, bool) {
	return c.lookup(key, now, false)
}

func (c *affinityCache) lookup(key string, now time.Time, touch bool) (int64, bool) {
	sh := c.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	el, ok := sh.items[key]
	if !ok {
		return 0, false
	}
	e := el.Value.(*affinityEntry)
	if !now.Before(e.expireAt) {
		sh.lru.Remove(el)
		delete(sh.items, key)
		return 0, false
	}
	if touch {
		e.expireAt = now.Add(e.ttl)
		sh.lru.MoveToFront(el)
	}
	return e.keyID, true
}

// Put 实现 AFF-02、AFF-05：写入或覆盖；分片满时淘汰最久未使用的条目。
func (c *affinityCache) Put(key string, keyID int64, ttl time.Duration, now time.Time) {
	sh := c.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if el, ok := sh.items[key]; ok {
		e := el.Value.(*affinityEntry)
		e.keyID, e.ttl, e.expireAt = keyID, ttl, now.Add(ttl)
		sh.lru.MoveToFront(el)
		return
	}
	for sh.lru.Len() >= sh.cap {
		oldest := sh.lru.Back()
		sh.lru.Remove(oldest)
		delete(sh.items, oldest.Value.(*affinityEntry).key)
	}
	sh.items[key] = sh.lru.PushFront(&affinityEntry{key: key, keyID: keyID, ttl: ttl, expireAt: now.Add(ttl)})
}

// Delete 删除条目。
func (c *affinityCache) Delete(key string) {
	sh := c.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if el, ok := sh.items[key]; ok {
		sh.lru.Remove(el)
		delete(sh.items, key)
	}
}

// Len 返回条目总数（含尚未被清理的过期条目）。
func (c *affinityCache) Len() int {
	n := 0
	for i := range c.shards {
		sh := &c.shards[i]
		sh.mu.Lock()
		n += sh.lru.Len()
		sh.mu.Unlock()
	}
	return n
}
