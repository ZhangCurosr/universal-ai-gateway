// Package pricing 实现计量与成本计算（spec/06 §1–§2）。全部为纯函数，禁止使用浮点做成本运算。
//
// 计费以「计量单位」为基础：单位是适配器定义的可计数量，例如大模型的 input_tokens、
// 搜索 API 的 requests、文档解析 API 的 pages。内核与账本不关心单位的含义，只按价目表相乘。
package pricing

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NanoPerUSD 是 1 USD 对应的 nano-USD。
const NanoPerUSD = 1_000_000_000

// 标准计量单位。适配器可以使用其他单位，名称必须满足 ValidUnit。
const (
	UnitInputTokens      = "input_tokens"       // 不含缓存读写的普通输入 token（PRC-02）
	UnitOutputTokens     = "output_tokens"      // 输出 token
	UnitCacheReadTokens  = "cache_read_tokens"  // 缓存命中的输入 token
	UnitCacheWriteTokens = "cache_write_tokens" // 写入缓存的输入 token
	UnitRequests         = "requests"           // 调用次数（按次计费的 API）
)

var unitPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidUnit 报告 u 是否是合法的单位名：小写字母开头，只含小写字母、数字、下划线，最长 64。
func ValidUnit(u string) bool { return unitPattern.MatchString(u) }

// Usage 是一次请求在各单位上的用量。nil 与空 map 等价；值 <= 0 的单位视为不存在（PRC-04）。
type Usage map[string]int64

// Get 返回单位 unit 的用量；不存在或为负返回 0。
func (u Usage) Get(unit string) int64 { return nonNeg(u[unit]) }

// Equal 报告两份用量是否相同（忽略值 <= 0 的单位）。
func (u Usage) Equal(v Usage) bool {
	for k := range u {
		if u.Get(k) != v.Get(k) {
			return false
		}
	}
	for k := range v {
		if u.Get(k) != v.Get(k) {
			return false
		}
	}
	return true
}

// Clean 返回去掉值 <= 0 的单位后的副本；结果为空时返回 nil。
func (u Usage) Clean() Usage {
	var out Usage
	for k, v := range u {
		if v > 0 {
			if out == nil {
				out = Usage{}
			}
			out[k] = v
		}
	}
	return out
}

// Rate 是一个单位的价格：每 Per 个单位 NanoUSD 纳美元。Per >= 1。
type Rate struct {
	NanoUSD int64
	Per     int64
}

// Price 是一个模型（服务）的价目表：单位 → 单价。
type Price map[string]Rate

// ParseRate 解析配置中的价格字符串 "USD[/PER]"（PRC-05）：
//
//	"3/1M"   每 1,000,000 个单位 3 美元
//	"0.008"  每 1 个单位 0.008 美元
//	"1.5/1K" 每 1,000 个单位 1.5 美元
//
// USD 是非负十进制数，最多 9 位小数（精确换算为 nano-USD，不经过浮点）；PER 是正整数，可带 K 或 M 后缀。
func ParseRate(s string) (Rate, error) {
	usd, per, hasPer := strings.Cut(strings.TrimSpace(s), "/")
	nano, err := parseNanoUSD(strings.TrimSpace(usd))
	if err != nil {
		return Rate{}, fmt.Errorf("price %q: %w", s, err)
	}
	r := Rate{NanoUSD: nano, Per: 1}
	if hasPer {
		if r.Per, err = parsePer(strings.TrimSpace(per)); err != nil {
			return Rate{}, fmt.Errorf("price %q: %w", s, err)
		}
	}
	return r, nil
}

func parseNanoUSD(s string) (int64, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || strings.ContainsAny(whole, "+-") || len(frac) > 9 {
		return 0, errors.New("USD must be a non-negative decimal with at most 9 fraction digits")
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > math.MaxInt64/NanoPerUSD-1 {
		return 0, errors.New("invalid USD amount")
	}
	var f int64
	if frac != "" {
		if strings.ContainsAny(frac, "+-") {
			return 0, errors.New("invalid USD amount")
		}
		if f, err = strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64); err != nil {
			return 0, errors.New("invalid USD amount")
		}
	}
	return w*NanoPerUSD + f, nil
}

func parsePer(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "M"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		mult, s = 1_000, strings.TrimSuffix(s, "K")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || n > math.MaxInt64/mult {
		return 0, errors.New("PER must be a positive integer, optionally suffixed with K or M")
	}
	return n * mult, nil
}

// Cost 计算成本（PRC-01）：Σ 用量 × NanoUSD / Per，各项精确累加后整体向上取整；超过 int64 时饱和为 MaxInt64。
// unpriced 返回 u 中有用量但 p 中没有定价的单位（按 0 计价），按名称升序；正常情况下为空，
// 因为 core.New 会校验价目表覆盖了出站适配器声明的全部单位（CORE-05）。
func Cost(p Price, u Usage) (nano int64, unpriced []string) {
	total := new(big.Rat)
	for unit, qty := range u {
		if qty <= 0 {
			continue
		}
		r, ok := p[unit]
		if !ok {
			unpriced = append(unpriced, unit)
			continue
		}
		if r.Per < 1 || r.NanoUSD <= 0 {
			continue
		}
		term := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(qty), big.NewInt(r.NanoUSD)), big.NewInt(r.Per))
		total.Add(total, term)
	}
	sort.Strings(unpriced)
	// ceil(num / den)
	num, den := total.Num(), total.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return math.MaxInt64, unpriced
	}
	return q.Int64(), unpriced
}

// ReserveUsage 合成用于预留的用量（PRC-03）。caps 是模型配置的 reserve：
// 对 estimate 中有的单位取 min(estimate, caps[单位])（caps 中没有该单位则取 estimate）；
// 对 estimate 中没有（键不存在）、caps 中有的单位取 caps[单位]。即 caps 既是上限也是「请求未声明时」的默认值；
// estimate 中显式为 0 的单位表示「确定不会产生」，不会被 caps 补上。
func ReserveUsage(estimate, caps Usage) Usage {
	out := Usage{}
	for unit := range estimate {
		v := estimate.Get(unit)
		if c, ok := caps[unit]; ok && c >= 0 && c < v {
			v = c
		}
		if v > 0 {
			out[unit] = v
		}
	}
	for unit := range caps {
		if _, ok := estimate[unit]; !ok && caps.Get(unit) > 0 {
			out[unit] = caps.Get(unit)
		}
	}
	return out
}

// EstimateInputTokens 用请求体字节数保守估算输入 token：ceil(bytes / 3)。供大模型类入站适配器填写 Envelope.Estimate。
func EstimateInputTokens(bodyBytes int) int64 {
	return ceilDiv(int64(max(bodyBytes, 0)), 3)
}

// EstimateOutputTokens 用已转发给客户端的文本字节数估算输出 token：ceil(bytes / 4)。
// 供大模型类出站适配器在上游未给出输出用量时使用（spec/06 §3.3）。
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
