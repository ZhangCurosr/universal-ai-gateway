package pricing

import "testing"

func TestFromUSDPerMTok(t *testing.T) {
	p := FromUSDPerMTok(3.0, 15.0, 0.30, 3.75)
	want := Price{3_000_000_000, 15_000_000_000, 300_000_000, 3_750_000_000}
	if p != want {
		t.Fatalf("FromUSDPerMTok() = %+v, want %+v", p, want)
	}
}

func TestCost(t *testing.T) {
	p := FromUSDPerMTok(3.0, 15.0, 0.30, 3.75)
	tests := []struct {
		name string
		u    Usage
		want int64
	}{
		{"zero", Usage{}, 0},
		// 1000 输入 × $3/M = $0.003 = 3,000,000 nano
		{"input only", Usage{InputTokens: 1000}, 3_000_000},
		// 1 个输出 token × 15000 nano/token = 15000
		{"one output token", Usage{OutputTokens: 1}, 15_000},
		{"PRC-04 negative treated as zero", Usage{InputTokens: -5, OutputTokens: 1}, 15_000},
		{"all fields", Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 1000, CacheWriteTokens: 100}, 300_000 + 150_000 + 300_000 + 375_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cost(p, tt.u); got != tt.want {
				t.Fatalf("Cost(%+v) = %d, want %d", tt.u, got, tt.want)
			}
		})
	}
}

func TestCostRoundsUp(t *testing.T) {
	p := Price{InputPerMTok: 1} // 1 nano / 百万 token
	if got := Cost(p, Usage{InputTokens: 1}); got != 1 {
		t.Fatalf("Cost() = %d, want 1 (ceil)", got)
	}
}

func TestEstimateReserve(t *testing.T) {
	p := FromUSDPerMTok(3.0, 15.0, 0, 0)
	// 3000 字节 → 1000 输入 token → 3,000,000；100 输出 → 1,500,000
	if got := EstimateReserve(p, 3000, 100); got != 4_500_000 {
		t.Fatalf("EstimateReserve() = %d, want 4500000", got)
	}
	if got := EstimateInputTokens(1); got != 1 {
		t.Fatalf("EstimateInputTokens(1) = %d, want 1", got)
	}
}
