package scheduler

import (
	"fmt"
	"hash/fnv"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

func TestAffinityGetPut(t *testing.T) {
	t.Skip("TODO(T07)")
	c := newAffinityCache(0)
	if _, ok := c.Get("s1", t0); ok {
		t.Fatal("Get on empty cache ok = true")
	}
	c.Put("s1", 42, time.Minute, t0)
	if id, ok := c.Get("s1", t0.Add(30*time.Second)); !ok || id != 42 {
		t.Fatalf("Get() = %d, %v; want 42, true", id, ok)
	}
	c.Put("s1", 7, time.Minute, t0) // AFF-05：覆盖
	if id, _ := c.Get("s1", t0); id != 7 {
		t.Fatalf("Get() after overwrite = %d, want 7", id)
	}
	c.Delete("s1")
	if _, ok := c.Get("s1", t0); ok || c.Len() != 0 {
		t.Fatal("entry still present after Delete")
	}
}

func TestAffinityExpire(t *testing.T) {
	t.Skip("TODO(T07)")
	c := newAffinityCache(0)
	c.Put("s1", 1, time.Minute, t0)
	if _, ok := c.Get("s1", t0.Add(61*time.Second)); ok { // AFF-03
		t.Fatal("expired entry returned")
	}
	if c.Len() != 0 {
		t.Fatalf("Len() = %d after expired Get, want 0", c.Len())
	}
}

func TestAffinityTouchExtends(t *testing.T) {
	t.Skip("TODO(T07)")
	c := newAffinityCache(0)
	c.Put("s1", 1, time.Minute, t0)
	c.Get("s1", t0.Add(50*time.Second)) // AFF-04：续期到 t0+110s
	if _, ok := c.Get("s1", t0.Add(100*time.Second)); !ok {
		t.Fatal("entry expired although it was touched")
	}
}

// 找到落在同一分片的两个 key
func sameShardKeys() (string, string) {
	shard := func(s string) uint32 { h := fnv.New32a(); h.Write([]byte(s)); return h.Sum32() % affinityShards }
	first := "k0"
	for i := 1; ; i++ {
		k := fmt.Sprintf("k%d", i)
		if shard(k) == shard(first) {
			return first, k
		}
	}
}

func TestAffinityLRUEviction(t *testing.T) {
	t.Skip("TODO(T07)")
	c := newAffinityCache(16) // 每分片容量 1（AFF-02）
	a, b := sameShardKeys()
	c.Put(a, 1, time.Hour, t0)
	c.Put(b, 2, time.Hour, t0)
	if _, ok := c.Get(a, t0); ok {
		t.Fatal("least recently used entry not evicted")
	}
	if id, ok := c.Get(b, t0); !ok || id != 2 {
		t.Fatalf("Get(b) = %d, %v", id, ok)
	}
}

func TestAffinityConcurrent(t *testing.T) {
	t.Skip("TODO(T07)")
	c := newAffinityCache(1024)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				k := fmt.Sprintf("g%d-%d", g, i%50)
				c.Put(k, int64(i), time.Minute, t0)
				c.Get(k, t0)
			}
		}(g)
	}
	wg.Wait()
	if c.Len() > 1024 {
		t.Fatalf("Len() = %d exceeds capacity", c.Len())
	}
}
