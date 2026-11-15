package admin

import (
	"encoding/csv"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// UnitSource 是装配代码给出的「单位 → 报告它的协议」（ADM-32，T79）：当前配置中各池协议的 Egress.Units()。
type UnitSource struct {
	Name      string
	Protocols []string
}

// UnitInfo 是统计响应中的一个单位。
type UnitInfo struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`      // pricing.UnitKind：tokens | count | other
	Protocols []string `json:"protocols"` // 报告它的协议；只在历史数据中出现的单位为空
}

// UsageStatsItem 是一组统计；没有参与分组的维度不输出。
type UsageStatsItem struct {
	Time              string           `json:"time,omitempty"` // 桶的起点，按请求的时区（RFC 3339）
	ClientID          int64            `json:"client_id,omitempty"`
	ClientName        string           `json:"client_name,omitempty"`
	Model             string           `json:"model,omitempty"`
	Pool              *string          `json:"pool,omitempty"` // 按池分组时总是输出；Key 已删除时为 ""
	KeyID             int64            `json:"key_id,omitempty"`
	KeyName           string           `json:"key_name,omitempty"`
	Requests          int64            `json:"requests"`
	EstimatedRequests int64            `json:"estimated_requests"`
	CostNanoUSD       int64            `json:"cost_nano_usd"`
	Tokens            int64            `json:"tokens"` // kind = tokens 的单位之和（PRC-07）
	Usage             map[string]int64 `json:"usage"`
}

// UsageStatsTotals 是全部分组的合计。
type UsageStatsTotals struct {
	Requests          int64            `json:"requests"`
	EstimatedRequests int64            `json:"estimated_requests"`
	CostNanoUSD       int64            `json:"cost_nano_usd"`
	Tokens            int64            `json:"tokens"`
	Usage             map[string]int64 `json:"usage"`
}

func tokenSum(u map[string]int64) int64 {
	var n int64
	for k, v := range u {
		if pricing.UnitKind(k) == pricing.KindTokens {
			n += v
		}
	}
	return n
}

// handleUsageStats 实现 ADM-32、ADM-33。
func (s *server) handleUsageStats(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	now := s.d.Now().UTC()
	q := store.UsageAggQuery{To: now}
	var err error
	if v := qv.Get("to"); v != "" {
		if q.To, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "invalid to parameter: RFC 3339 required"))
			return
		}
	}
	q.From = q.To.Add(-24 * time.Hour)
	if v := qv.Get("from"); v != "" {
		if q.From, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "invalid from parameter: RFC 3339 required"))
			return
		}
	}
	if v := qv.Get("group"); v != "" {
		for _, g := range strings.Split(v, ",") {
			if g = strings.TrimSpace(g); g != "" {
				q.GroupBy = append(q.GroupBy, g)
			}
		}
	}
	q.Interval = qv.Get("interval")
	if q.Interval == "" {
		q.Interval = store.IntervalDay
	}
	if q.TZ, err = config.ParseTZ(qv.Get("tz")); err != nil {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, err.Error()))
		return
	}
	if v := qv.Get("client_id"); v != "" {
		if q.ClientID, err = strconv.ParseInt(v, 10, 64); err != nil || q.ClientID < 0 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "invalid client_id"))
			return
		}
	}
	q.Model, q.Pool = qv.Get("model"), qv.Get("pool")
	format := qv.Get("format")
	if format != "" && format != "json" && format != "csv" {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "format must be json or csv"))
		return
	}

	buckets, err := s.d.Store.AggregateUsage(r.Context(), q)
	switch {
	case errors.Is(err, store.ErrInvalidArgument):
		writeError(w, apperr.New(apperr.CodeInvalidRequest, strings.TrimPrefix(err.Error(), store.ErrInvalidArgument.Error()+": ")))
		return
	case errors.Is(err, store.ErrTooManyBuckets):
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "too many rows: use a shorter range or fewer group dimensions"))
		return
	case err != nil:
		writeError(w, err)
		return
	}

	names, err := s.statsNames(r, q.GroupBy)
	if err != nil {
		writeError(w, err)
		return
	}
	grouped := map[string]bool{}
	for _, g := range q.GroupBy {
		grouped[g] = true
	}
	items := make([]UsageStatsItem, 0, len(buckets))
	totals := UsageStatsTotals{Usage: map[string]int64{}}
	seen := map[string]bool{}
	for _, b := range buckets {
		it := UsageStatsItem{Requests: b.Requests, EstimatedRequests: b.EstimatedRequests, CostNanoUSD: b.CostNanoUSD, Usage: b.Usage, Tokens: tokenSum(b.Usage)}
		if grouped[store.GroupTime] {
			it.Time = b.Time.In(q.TZ).Format(time.RFC3339)
		}
		if grouped[store.GroupClient] {
			it.ClientID, it.ClientName = b.ClientID, names.clients[b.ClientID]
		}
		if grouped[store.GroupModel] {
			it.Model = b.Model
		}
		if grouped[store.GroupPool] {
			p := b.Pool
			it.Pool = &p
		}
		if grouped[store.GroupKey] {
			it.KeyID, it.KeyName = b.KeyID, names.keys[b.KeyID]
		}
		items = append(items, it)
		totals.Requests += b.Requests
		totals.EstimatedRequests += b.EstimatedRequests
		totals.CostNanoUSD += b.CostNanoUSD
		for u, n := range b.Usage {
			totals.Usage[u] += n
			seen[u] = true
		}
	}
	totals.Tokens = tokenSum(totals.Usage)
	units := s.statsUnits(seen)

	if format == "csv" {
		writeUsageCSV(w, q.GroupBy, items, units)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "units": units, "totals": totals})
}

type statsNames struct {
	clients map[int64]string
	keys    map[int64]string
}

// statsNames 只在按客户端 / Key 分组时读取名称（不含 Token 与密钥）。
func (s *server) statsNames(r *http.Request, groups []string) (statsNames, error) {
	n := statsNames{clients: map[int64]string{}, keys: map[int64]string{}}
	for _, g := range groups {
		switch g {
		case store.GroupClient:
			cs, err := s.d.Store.ListClients(r.Context())
			if err != nil {
				return n, err
			}
			for _, c := range cs {
				n.clients[c.ID] = c.Name
			}
		case store.GroupKey:
			ks, err := s.d.Store.ListUpstreamKeys(r.Context())
			if err != nil {
				return n, err
			}
			for _, k := range ks {
				n.keys[k.ID] = k.Name
			}
		}
	}
	return n, nil
}

// statsUnits 合并配置中协议报告的单位与结果中出现的单位，按 pricing.SortUnits 排序。
func (s *server) statsUnits(seen map[string]bool) []UnitInfo {
	protocols := map[string][]string{}
	if s.d.Units != nil {
		for _, u := range s.d.Units() {
			protocols[u.Name] = append(protocols[u.Name], u.Protocols...)
		}
	}
	for u := range seen {
		if _, ok := protocols[u]; !ok {
			protocols[u] = nil
		}
	}
	names := make([]string, 0, len(protocols))
	for u := range protocols {
		names = append(names, u)
	}
	pricing.SortUnits(names)
	out := make([]UnitInfo, 0, len(names))
	for _, u := range names {
		ps := append([]string{}, protocols[u]...)
		sort.Strings(ps)
		out = append(out, UnitInfo{Name: u, Kind: pricing.UnitKind(u), Protocols: ps})
	}
	return out
}

// formatUSD 把 nano-USD 写成十进制美元字符串（不用浮点）。
func formatUSD(nano int64) string {
	sign := ""
	if nano < 0 {
		sign, nano = "-", -nano
	}
	frac := strings.TrimRight(strconv.FormatInt(1_000_000_000+nano%1_000_000_000, 10)[1:], "0")
	if frac == "" {
		return sign + strconv.FormatInt(nano/1_000_000_000, 10)
	}
	return sign + strconv.FormatInt(nano/1_000_000_000, 10) + "." + frac
}

// writeUsageCSV 实现 ADM-33：分组列 + 请求数 + 估算请求数 + 成本（USD）+ token 合计 + 每个单位一列。
// 开头写 UTF-8 BOM，让表格软件按 UTF-8 打开（客户端名可能含中文）。
func writeUsageCSV(w http.ResponseWriter, groups []string, items []UsageStatsItem, units []UnitInfo) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage.csv"`)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return
	}
	cw := csv.NewWriter(w)
	var header []string
	for _, g := range groups {
		switch g {
		case store.GroupClient:
			header = append(header, "client_id", "client")
		case store.GroupKey:
			header = append(header, "key_id", "key")
		default:
			header = append(header, g)
		}
	}
	header = append(header, "requests", "estimated_requests", "cost_usd", "tokens")
	for _, u := range units {
		header = append(header, u.Name)
	}
	if cw.Write(header) != nil {
		return
	}
	for _, it := range items {
		var row []string
		for _, g := range groups {
			switch g {
			case store.GroupTime:
				row = append(row, it.Time)
			case store.GroupClient:
				row = append(row, strconv.FormatInt(it.ClientID, 10), it.ClientName)
			case store.GroupModel:
				row = append(row, it.Model)
			case store.GroupPool:
				p := ""
				if it.Pool != nil {
					p = *it.Pool
				}
				row = append(row, p)
			case store.GroupKey:
				row = append(row, strconv.FormatInt(it.KeyID, 10), it.KeyName)
			}
		}
		row = append(row, strconv.FormatInt(it.Requests, 10), strconv.FormatInt(it.EstimatedRequests, 10), formatUSD(it.CostNanoUSD), strconv.FormatInt(it.Tokens, 10))
		for _, u := range units {
			row = append(row, strconv.FormatInt(it.Usage[u.Name], 10))
		}
		if cw.Write(row) != nil {
			return
		}
	}
	cw.Flush()
}
