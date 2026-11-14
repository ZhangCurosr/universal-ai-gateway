package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func ev(reqID string, client int64, model string, cost int64, at time.Time) store.UsageEvent {
	return store.UsageEvent{RequestID: reqID, ClientID: client, KeyID: 1, Model: model, Op: "call",
		Usage: map[string]int64{"input_tokens": 10, "output_tokens": 5}, CostNanoUSD: cost,
		UsageSource: store.UsageSourceUpstream, CreatedAt: at}
}

// STO-10：request_id 幂等
func TestUsageEventsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	batch := []store.UsageEvent{ev("r1", 1, "m", 100, fixedNow), ev("r2", 1, "m", 50, fixedNow)}
	if err := db.InsertUsageEvents(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertUsageEvents(ctx, batch); err != nil { // 重放同一批
		t.Fatalf("replay InsertUsageEvents() error = %v", err)
	}
	if err := db.InsertUsageEvents(ctx, nil); err != nil {
		t.Fatalf("InsertUsageEvents(nil) error = %v", err)
	}
	sums, err := db.SumCostByClient(ctx)
	if err != nil || sums[1] != 150 {
		t.Fatalf("SumCostByClient() = %v, %v; want client 1 = 150", sums, err)
	}
}

// STO-10：只有重复 request_id 可以被跳过，其他约束违例必须报错，计费事件不能被静默丢弃。
func TestUsageEventsConstraintViolationIsError(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	bad := ev("r-bad", 1, "m", 10, fixedNow)
	bad.UsageSource = "bogus" // 违反 CHECK 约束
	if err := db.InsertUsageEvents(ctx, []store.UsageEvent{bad}); err == nil {
		t.Fatal("InsertUsageEvents() with invalid usage_source error = nil, want error")
	}
}

// STO-11
func TestSumCostByClient(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_ = db.InsertUsageEvents(ctx, []store.UsageEvent{
		ev("a", 1, "m", 10, fixedNow), ev("b", 2, "m", 20, fixedNow), ev("c", 2, "m", 5, fixedNow),
	})
	sums, err := db.SumCostByClient(ctx)
	if err != nil || sums[1] != 10 || sums[2] != 25 || len(sums) != 2 {
		t.Fatalf("SumCostByClient() = %v, %v", sums, err)
	}
}

// STO-12：[From, To) 区间与分组
func TestQueryUsage(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t0 := fixedNow
	_ = db.InsertUsageEvents(ctx, []store.UsageEvent{
		ev("a", 1, "m1", 10, t0),
		ev("b", 1, "m1", 10, t0.Add(time.Minute)),
		{RequestID: "c", ClientID: 1, KeyID: 1, Model: "m2", Op: "search", Usage: map[string]int64{"credits": 2},
			CostNanoUSD: 7, UsageSource: store.UsageSourceUpstream, CreatedAt: t0.Add(time.Minute)},
		ev("d", 2, "m1", 99, t0.Add(time.Hour)), // 恰好等于 To，不计入
	})
	got, err := db.QueryUsage(ctx, store.UsageQuery{From: t0, To: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("QueryUsage() = %+v, want 2 groups", got)
	}
	if got[0].Model != "m1" || got[0].Requests != 2 || got[0].CostNanoUSD != 20 ||
		got[0].Usage["input_tokens"] != 20 || got[0].Usage["output_tokens"] != 10 || len(got[0].Usage) != 2 {
		t.Errorf("group 0 = %+v", got[0])
	}
	if got[1].Model != "m2" || got[1].Requests != 1 || got[1].Usage["credits"] != 2 || len(got[1].Usage) != 1 {
		t.Errorf("group 1 = %+v", got[1])
	}
}

func TestQueryUsageFilteredByClient(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t0 := fixedNow
	_ = db.InsertUsageEvents(ctx, []store.UsageEvent{
		ev("a", 1, "m1", 10, t0),
		ev("b", 2, "m1", 20, t0),
	})
	got, err := db.QueryUsage(ctx, store.UsageQuery{ClientID: 2, From: t0, To: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ClientID != 2 || got[0].CostNanoUSD != 20 {
		t.Fatalf("QueryUsage() with ClientID=2 got %+v", got)
	}
}

// STO-13 / STO-14
func TestAuditInsertAndDelete(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	rec := func(id string, at time.Time) store.AuditRecord {
		return store.AuditRecord{RequestID: id, ClientID: 1, Model: "m", Op: "messages", IngressProtocol: "anthropic",
			UpstreamProtocol: "anthropic", Status: 200, Outcome: "success", Attempts: 1, TTFBMillis: -1, CreatedAt: at}
	}
	if err := db.InsertAuditRecords(ctx, []store.AuditRecord{rec("old", fixedNow.Add(-48*time.Hour)), rec("new", fixedNow)}); err != nil {
		t.Fatal(err)
	}
	n, err := db.DeleteAuditBefore(ctx, fixedNow.Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("DeleteAuditBefore() = %d, %v; want 1", n, err)
	}
}

func TestAuditRecordsEmptyAndNoneDeleted(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := db.InsertAuditRecords(ctx, nil); err != nil {
		t.Fatalf("InsertAuditRecords(nil) error = %v", err)
	}
	n, err := db.DeleteAuditBefore(ctx, fixedNow)
	if err != nil || n != 0 {
		t.Fatalf("DeleteAuditBefore(empty) = %d, %v; want 0", n, err)
	}
}

// STO-16：usage 列按键排序序列化；Usage 为 nil 时写 {}。
func TestUsageColumnSerialization(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	withUsage := ev("r-sorted", 1, "m", 1, fixedNow)
	withUsage.Usage = map[string]int64{"output_tokens": 5, "cache_read_tokens": 2, "input_tokens": 10}
	noUsage := ev("r-nil", 1, "m", 0, fixedNow)
	noUsage.Usage = nil
	if err := db.InsertUsageEvents(ctx, []store.UsageEvent{withUsage, noUsage}); err != nil {
		t.Fatal(err)
	}
	for reqID, want := range map[string]string{
		"r-sorted": `{"cache_read_tokens":2,"input_tokens":10,"output_tokens":5}`,
		"r-nil":    `{}`,
	} {
		var got string
		if err := db.db.QueryRowContext(ctx, `SELECT usage FROM usage_events WHERE request_id = ?`, reqID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("usage for %s = %s, want %s", reqID, got, want)
		}
	}
}

// STO-21：按 Key 与时间求某个单位的和；没有用量的 Key 不出现；非法单位名报错；超过一批的 Key 分批查询
func TestSumKeyUsage(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mk := func(req string, key int64, at time.Time, usage map[string]int64) store.UsageEvent {
		return store.UsageEvent{RequestID: req, ClientID: 1, KeyID: key, Model: "m", Op: "call", Usage: usage,
			UsageSource: store.UsageSourceUpstream, CreatedAt: at}
	}
	since := fixedNow.Add(-time.Hour)
	events := []store.UsageEvent{
		mk("a", 1, fixedNow, map[string]int64{"credits": 2, "requests": 1}),
		mk("b", 1, fixedNow.Add(-30*time.Minute), map[string]int64{"credits": 3}),
		mk("c", 1, fixedNow.Add(-2*time.Hour), map[string]int64{"credits": 100}), // 周期之前
		mk("d", 2, fixedNow, map[string]int64{"requests": 1}),                    // 没有 credits
		mk("e", 3, since, map[string]int64{"credits": 7}),                        // 恰好在周期开始
		mk("f", 600, fixedNow, map[string]int64{"credits": 1}),
	}
	if err := db.InsertUsageEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	ids := []int64{1, 2, 3, 4}
	for i := int64(5); i <= 600; i++ { // 超过一批（500）
		ids = append(ids, i)
	}
	got, err := db.SumKeyUsage(ctx, "credits", since, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1] != 5 || got[3] != 7 || got[600] != 1 {
		t.Fatalf("SumKeyUsage = %v", got)
	}
	if got, err := db.SumKeyUsage(ctx, "requests", since, []int64{1, 2}); err != nil || got[1] != 1 || got[2] != 1 {
		t.Fatalf("requests = %v %v", got, err)
	}
	if got, err := db.SumKeyUsage(ctx, "credits", since, nil); err != nil || len(got) != 0 {
		t.Fatalf("no keys = %v %v", got, err)
	}
	if _, err := db.SumKeyUsage(ctx, `credits") OR 1=1 --`, since, ids); err == nil {
		t.Fatal("invalid unit name must be rejected")
	}
}

// STO-22：按 Key 汇总请求数、各单位用量与费用；时间范围左闭右开；跨度上限
func TestSumUsageByKey(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mk := func(req string, key int64, at time.Time, cost int64, usage map[string]int64) store.UsageEvent {
		return store.UsageEvent{RequestID: req, ClientID: 1, KeyID: key, Model: "m", Op: "call", Usage: usage, CostNanoUSD: cost,
			UsageSource: store.UsageSourceUpstream, CreatedAt: at}
	}
	from, to := fixedNow.Add(-24*time.Hour), fixedNow
	if err := db.InsertUsageEvents(ctx, []store.UsageEvent{
		mk("a", 1, fixedNow.Add(-time.Hour), 10, map[string]int64{"credits": 2}),
		mk("b", 1, fixedNow.Add(-2*time.Hour), 5, map[string]int64{"credits": 1, "requests": 1}),
		mk("c", 1, to, 99, map[string]int64{"credits": 50}), // 右端不含
		mk("d", 2, from, 7, nil), // 左端含；没有用量
		mk("e", 3, fixedNow.Add(-time.Hour), 1, map[string]int64{"pages": 3}), // 不在请求的 Key 中
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.SumUsageByKey(ctx, from, to, []int64{1, 2, 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].KeyID != 1 || got[0].Requests != 2 || got[0].CostNanoUSD != 15 || got[0].Usage["credits"] != 3 || got[0].Usage["requests"] != 1 {
		t.Fatalf("key 1 = %+v", got)
	}
	if got[1].KeyID != 2 || got[1].Requests != 1 || got[1].CostNanoUSD != 7 || len(got[1].Usage) != 0 {
		t.Fatalf("key 2 = %+v", got[1])
	}
	if _, err := db.SumUsageByKey(ctx, to, from, []int64{1}); err == nil {
		t.Fatal("reversed range must be rejected")
	}
	if _, err := db.SumUsageByKey(ctx, to.Add(-32*24*time.Hour), to, []int64{1}); err == nil {
		t.Fatal("range over 31 days must be rejected")
	}
}
