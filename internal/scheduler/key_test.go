package scheduler

import (
	"sync"
	"sync/atomic"
	"testing"
)

// SCH-14：并发 tryAcquire 永不超过 MaxInFlight
func TestTryAcquireNeverExceedsMax(t *testing.T) {
	k := NewKey(1, "p", "k", nil, 1, 3, PersistedReady)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if k.tryAcquire() {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 3 || k.InFlight() != 3 {
		t.Fatalf("acquired = %d, inFlight = %d; want 3, 3", ok.Load(), k.InFlight())
	}
	k.release()
	if !k.tryAcquire() {
		t.Fatal("tryAcquire after release = false")
	}
}

func TestParsePersistedState(t *testing.T) {
	tests := map[string]PersistedState{
		"ready": PersistedReady, "quarantined": PersistedQuarantined, "disabled": PersistedDisabled, "weird": PersistedDisabled,
	}
	for in, want := range tests {
		if got := ParsePersistedState(in); got != want {
			t.Errorf("ParsePersistedState(%q) = %v, want %v", in, got, want)
		}
	}
}
