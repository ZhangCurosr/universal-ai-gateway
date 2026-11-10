package ir

import "testing"

func TestTokenUsageZero(t *testing.T) {
	u := TokenUsage{}
	if u.Input != 0 || u.Output != 0 || u.CacheRead != 0 || u.CacheWrite != 0 {
		t.Fatalf("expected all zeros, got %+v", u)
	}
}

func TestStreamEventKind(t *testing.T) {
	kinds := []StreamEventKind{
		EventStart,
		EventText,
		EventThinking,
		EventToolStart,
		EventToolArgs,
		EventToolEnd,
		EventUsage,
		EventStop,
		EventEnd,
	}
	for i, k := range kinds {
		if int(k) != i {
			t.Errorf("expected kind %d, got %d", i, k)
		}
	}
}
