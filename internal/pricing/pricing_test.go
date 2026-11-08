package pricing

import (
	"math"
	"reflect"
	"testing"
)

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		want    Rate
		wantErr bool
	}{
		{"3/1M", Rate{3 * NanoPerUSD, 1_000_000}, false},
		{"0.008", Rate{8_000_000, 1}, false},
		{"1.5/1K", Rate{1_500_000_000, 1_000}, false},
		{" 0.000000001 / 7 ", Rate{1, 7}, false},
		{"0", Rate{0, 1}, false},
		{"-1", Rate{}, true},
		{"1.0000000001", Rate{}, true}, // 超过 9 位小数
		{"abc", Rate{}, true},
		{"1/0", Rate{}, true},
		{"1/2G", Rate{}, true},
		{".5", Rate{}, true},
	}
	for _, tt := range tests {
		got, err := ParseRate(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseRate(%q) = %+v, %v; want %+v, wantErr=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func mustRate(t *testing.T, s string) Rate {
	t.Helper()
	r, err := ParseRate(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCost(t *testing.T) {
	p := Price{
		UnitInputTokens:      mustRate(t, "3/1M"),
		UnitOutputTokens:     mustRate(t, "15/1M"),
		UnitCacheReadTokens:  mustRate(t, "0.3/1M"),
		UnitCacheWriteTokens: mustRate(t, "3.75/1M"),
		UnitRequests:         mustRate(t, "0.008"),
		"pages":              mustRate(t, "0.01"),
	}
	tests := []struct {
		name string
		u    Usage
		want int64
	}{
		{"zero", nil, 0},
		{"input only", Usage{UnitInputTokens: 1000}, 3_000_000},
		{"one output token", Usage{UnitOutputTokens: 1}, 15_000},
		{"PRC-04 negative treated as zero", Usage{UnitInputTokens: -5, UnitOutputTokens: 1}, 15_000},
		{"all token units", Usage{UnitInputTokens: 100, UnitOutputTokens: 10, UnitCacheReadTokens: 1000, UnitCacheWriteTokens: 100}, 300_000 + 150_000 + 300_000 + 375_000},
		{"per request", Usage{UnitRequests: 2}, 16_000_000},
		{"per page plus request", Usage{UnitRequests: 1, "pages": 30}, 8_000_000 + 300_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, unpriced := Cost(p, tt.u)
			if got != tt.want || len(unpriced) != 0 {
				t.Fatalf("Cost(%v) = %d, %v; want %d", tt.u, got, unpriced, tt.want)
			}
		})
	}
}

func TestCostRoundsUpOnceAfterSumming(t *testing.T) {
	p := Price{"a": {NanoUSD: 1, Per: 3}, "b": {NanoUSD: 1, Per: 3}}
	// 1/3 + 1/3 = 2/3 → 1（先累加再取整，而不是每项取整得 2）
	if got, _ := Cost(p, Usage{"a": 1, "b": 1}); got != 1 {
		t.Fatalf("Cost() = %d, want 1", got)
	}
}

func TestCostUnpricedAndSaturation(t *testing.T) {
	got, unpriced := Cost(Price{"a": {NanoUSD: 5, Per: 1}}, Usage{"a": 1, "zeta": 3, "beta": 1, "gone": 0})
	if got != 5 || !reflect.DeepEqual(unpriced, []string{"beta", "zeta"}) {
		t.Fatalf("Cost() = %d, %v", got, unpriced)
	}
	if got, _ := Cost(Price{"a": {NanoUSD: math.MaxInt64, Per: 1}}, Usage{"a": math.MaxInt64}); got != math.MaxInt64 {
		t.Fatalf("Cost() overflow = %d, want MaxInt64", got)
	}
}

func TestReserveUsage(t *testing.T) {
	got := ReserveUsage(
		Usage{UnitInputTokens: 1000, UnitOutputTokens: 100000},
		Usage{UnitOutputTokens: 8192, UnitRequests: 1},
	)
	want := Usage{UnitInputTokens: 1000, UnitOutputTokens: 8192, UnitRequests: 1}
	if !got.Equal(want) {
		t.Fatalf("ReserveUsage() = %v, want %v", got, want)
	}
	// 估计中显式为 0 的单位不会被 caps 的默认值补上
	if got := ReserveUsage(Usage{"pages": 0}, Usage{"pages": 600}); len(got) != 0 {
		t.Fatalf("ReserveUsage(explicit zero) = %v, want empty", got)
	}
	if got := ReserveUsage(nil, nil); len(got) != 0 {
		t.Fatalf("ReserveUsage(nil, nil) = %v", got)
	}
}

func TestUsageHelpers(t *testing.T) {
	if !(Usage{"a": 1, "b": 0}).Equal(Usage{"a": 1}) || (Usage{"a": 1}).Equal(Usage{"a": 2}) {
		t.Fatal("Equal() wrong")
	}
	if c := (Usage{"a": 0, "b": -1}).Clean(); c != nil {
		t.Fatalf("Clean() = %v, want nil", c)
	}
	if !ValidUnit("pages") || ValidUnit("Pages") || ValidUnit("1x") || ValidUnit("") {
		t.Fatal("ValidUnit() wrong")
	}
	if EstimateInputTokens(1) != 1 || EstimateOutputTokens(9) != 3 {
		t.Fatal("estimates wrong")
	}
}
