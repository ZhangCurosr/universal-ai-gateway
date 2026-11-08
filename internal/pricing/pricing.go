// Package pricing 实现价格换算与成本计算（spec/06 §1–§2）。全部为纯函数，禁止使用浮点做成本运算。
package pricing

import "math"

// NanoPerUSD 是 1 USD 对应的 nano-USD。
const NanoPerUSD = 1_000_000_000

// Price 单位：nano-USD / 百万 token。
type Price struct {
	InputPerMTok      int64
	OutputPerMTok     int64
	CacheReadPerMTok  int64
	CacheWritePerMTok int64
}

// Usage 是一次请求的 token 用量。InputTokens 不含缓存读写（PRC-02）。
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// FromUSDPerMTok 把配置中的「USD / 百万 token」浮点价格转换为整数价格（只在加载配置时调用）。
func FromUSDPerMTok(input, output, cacheRead, cacheWrite float64) Price {
	conv := func(usd float64) int64 { return int64(math.Round(usd * NanoPerUSD)) }
	return Price{
		InputPerMTok:      conv(input),
		OutputPerMTok:     conv(output),
		CacheReadPerMTok:  conv(cacheRead),
		CacheWritePerMTok: conv(cacheWrite),
	}
}

// Cost 计算成本（PRC-01、PRC-04），结果向上取整。
func Cost(p Price, u Usage) int64 {
	total := nonNeg(u.InputTokens)*p.InputPerMTok +
		nonNeg(u.OutputTokens)*p.OutputPerMTok +
		nonNeg(u.CacheReadTokens)*p.CacheReadPerMTok +
		nonNeg(u.CacheWriteTokens)*p.CacheWritePerMTok
	return ceilDiv(total, 1_000_000)
}

// EstimateInputTokens 用请求体字节数保守估算输入 token（PRC-03）。
func EstimateInputTokens(bodyBytes int) int64 {
	return ceilDiv(int64(nonNegInt(bodyBytes)), 3)
}

// EstimateReserve 估算预留金额（PRC-03）：输入按估算 token × 输入单价，输出按 maxOutputTokens × 输出单价。
func EstimateReserve(p Price, bodyBytes int, maxOutputTokens int64) int64 {
	return Cost(p, Usage{InputTokens: EstimateInputTokens(bodyBytes), OutputTokens: maxOutputTokens})
}

// EstimateOutputTokens 用已转发给客户端的字节数估算输出 token（spec/06 §3.3）。
func EstimateOutputTokens(forwardedBytes int64) int64 {
	return ceilDiv(nonNeg(forwardedBytes), 4)
}

func ceilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func nonNegInt(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
