package scheduler

import (
	"container/list"
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

// Get 实现 AFF-03、AFF-04。
func (c *affinityCache) Get(key string, now time.Time) (int64, bool) {
	// TODO(T07)
	return 0, false
}

// Put 实现 AFF-02、AFF-05。
func (c *affinityCache) Put(key string, keyID int64, ttl time.Duration, now time.Time) {
	// TODO(T07)
}

// Delete 删除条目。
func (c *affinityCache) Delete(key string) {
	// TODO(T07)
}

// Len 返回条目总数（含尚未被清理的过期条目）。
func (c *affinityCache) Len() int {
	// TODO(T07)
	return 0
}
