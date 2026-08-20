package mlops

import (
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// waitFor 轮询断言（异步 collector 落库有延迟）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestUsageCollectorPersistsAndPrices(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)
	svc.StartUsage(UsageSettings{QueueSize: 64})
	defer svc.StopUsage()

	// 配价：输入 2.5 元/M、输出 10 元/M
	if _, err := svc.SavePricing(PricingInput{
		Provider: "openai", Model: "gpt-4o", PriceInPerM: "2.5", PriceOutPerM: "10",
	}, "admin"); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	svc.Record(usage.Record{
		OperationID: "op-1", CallID: "call-1",
		Provider: "openai", Model: "gpt-4o", Scenario: "chat", EntryPoint: "/api/v1/ainexus/chat",
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, UsagePresent: true,
		OK: true, Status: "success", StartedAt: now, FinishedAt: now.Add(2 * time.Second),
	})
	// 未配价模型：priced=false 只计 token
	svc.Record(usage.Record{
		OperationID: "op-2", CallID: "call-2",
		Provider: "x", Model: "free-model", Scenario: "compress",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, UsagePresent: true,
		OK: true, Status: "success", StartedAt: now, FinishedAt: now,
	})
	// usage 缺失
	svc.Record(usage.Record{
		OperationID: "op-2", CallID: "call-3",
		Provider: "openai", Model: "gpt-4o", Scenario: "investigate",
		OK: false, Status: "provider_error", Error: "boom",
		StartedAt: now, FinishedAt: now,
	})

	if !waitFor(t, 3*time.Second, func() bool {
		_, total, _, _ := st.ListMLUsage(store.MLUsageFilter{})
		return total == 3
	}) {
		t.Fatal("3 records should be persisted")
	}

	items, _, _, err := st.ListMLUsage(store.MLUsageFilter{OperationID: "op-1"})
	if err != nil || len(items) != 1 {
		t.Fatalf("op-1 items = %v err = %v", items, err)
	}
	r := items[0]
	// 1000×2.5 + 500×10 = 7500 元/M → 微元 = 7500×1e6？不：
	// cost = tokens×price_micro/1e6 = 1000×2_500_000/1e6 + 500×10_000_000/1e6 = 2500+5000 = 7500 微元
	if !r.Priced || r.CostMinor != 7500 {
		t.Fatalf("priced=%v cost=%d, want true/7500", r.Priced, r.CostMinor)
	}
	if r.PricingVersion == "" || r.Currency != "CNY" {
		t.Fatalf("pricing snapshot missing: %+v", r)
	}

	// 免费价格 ≠ 无价格：0 元配价 → priced=true、金额 0
	if _, err := svc.SavePricing(PricingInput{
		Provider: "x", Model: "free-model", PriceInPerM: "0", PriceOutPerM: "0",
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	svc.Record(usage.Record{
		OperationID: "op-3", CallID: "call-4",
		Provider: "x", Model: "free-model", Scenario: "chat",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, UsagePresent: true,
		OK: true, Status: "success", StartedAt: now, FinishedAt: now,
	})
	if !waitFor(t, 3*time.Second, func() bool {
		items, _, _, _ := st.ListMLUsage(store.MLUsageFilter{OperationID: "op-3"})
		return len(items) == 1 && items[0].Priced && items[0].CostMinor == 0
	}) {
		items, _, _, _ := st.ListMLUsage(store.MLUsageFilter{OperationID: "op-3"})
		t.Fatalf("free pricing should be priced=true cost=0: %+v", items)
	}

	// call_id 幂等：重复 Record 同 call_id 不重复入账
	before, _, _, _ := st.ListMLUsage(store.MLUsageFilter{})
	svc.Record(usage.Record{
		OperationID: "op-1", CallID: "call-1",
		Provider: "openai", Model: "gpt-4o", Scenario: "chat",
		PromptTokens: 1000, CompletionTokens: 500, UsagePresent: true,
		OK: true, Status: "success", StartedAt: now, FinishedAt: now,
	})
	time.Sleep(100 * time.Millisecond)
	after, _, _, _ := st.ListMLUsage(store.MLUsageFilter{})
	if len(after) != len(before) {
		t.Fatalf("duplicate call_id added records: %d -> %d", len(before), len(after))
	}
	if dedup := svc.collectorStats().Deduped; dedup != 1 {
		t.Fatalf("deduped counter = %d, want 1", dedup)
	}

	// 总览：今日行存在且金额一致
	ov, err := svc.CostsOverview()
	if err != nil {
		t.Fatal(err)
	}
	if ov.TodayTotals.Calls != 4 {
		t.Fatalf("today calls = %d, want 4", ov.TodayTotals.Calls)
	}
	if ov.TodayTotals.UnmeteredCalls != 1 || ov.TodayTotals.PricedCalls != 2 {
		t.Fatalf("today unmetered=%d priced=%d", ov.TodayTotals.UnmeteredCalls, ov.TodayTotals.PricedCalls)
	}
	if ov.TodayTotals.CostMinor != 7500 {
		t.Fatalf("today cost = %d, want 7500", ov.TodayTotals.CostMinor)
	}
	// 趋势：默认窗口内每天有行
	trend, err := svc.CostsTrend("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(trend) != 30 {
		t.Fatalf("trend days = %d, want 30", len(trend))
	}
}

func TestUsageCollectorQueueFullDoesNotBlock(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)
	// 队列容量 1；不启动消费（不调 StartUsage 的消费路径——这里直接测
	// Record 的非阻塞语义：入队 1 条后继续 Record 必须立即返回丢弃）
	q := make(chan usage.Record, 1)
	svc.usageQueue = q
	svc.usageRunning = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Record(usage.Record{CallID: "a"})
		svc.Record(usage.Record{CallID: "b"}) // 满 → 丢弃
		svc.Record(usage.Record{CallID: "c"}) // 满 → 丢弃
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on full queue")
	}
	if dropped := svc.usageDropped.Load(); dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
}

func TestStopUsageDrainsQueue(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)
	svc.StartUsage(UsageSettings{QueueSize: 8})
	for i := 0; i < 5; i++ {
		svc.Record(usage.Record{
			CallID: string(rune('a' + i)), Provider: "p", Model: "m", Scenario: "chat",
			OK: true, Status: "success", UsagePresent: true, PromptTokens: 1,
			StartedAt: time.Now(), FinishedAt: time.Now(),
		})
	}
	svc.StopUsage()
	_, total, _, err := st.ListMLUsage(store.MLUsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Fatalf("after stop total = %d, want 5 (queue must drain)", total)
	}
	// 停止后 Record 静默丢弃（不 panic）
	svc.Record(usage.Record{CallID: "z"})
}

func TestParseMicroDecimal(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"8", 8_000_000, true},
		{"0", 0, true},
		{"0.5", 500_000, true},
		{"2.5", 2_500_000, true},
		{"0.000002", 2, true},
		{"0.0000015", 0, false}, // 超过 6 位小数
		{"", 0, false},
		{".5", 0, false},
		{"1.", 0, false},
		{"-1", 0, false},
		{"1e3", 0, false},
		{" 2.5 ", 2_500_000, true},
	}
	for _, c := range cases {
		got, err := parseMicroDecimal(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseMicroDecimal(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseMicroDecimal(%q) should fail, got %d", c.in, got)
		}
	}
	if MicroToDecimal(2_500_000) != "2.5" || MicroToDecimal(7500) != "0.0075" || MicroToDecimal(0) != "0" {
		t.Fatalf("MicroToDecimal roundtrip broken: %q %q %q",
			MicroToDecimal(2_500_000), MicroToDecimal(7500), MicroToDecimal(0))
	}
}

func TestPricingValidationAndAudit(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)

	if _, err := svc.SavePricing(PricingInput{Provider: "", Model: "m", PriceInPerM: "1", PriceOutPerM: "1"}, "admin"); err == nil {
		t.Fatal("empty provider should be rejected")
	}
	if _, err := svc.SavePricing(PricingInput{Provider: "p", Model: "m", PriceInPerM: "x", PriceOutPerM: "1"}, "admin"); err == nil {
		t.Fatal("bad price should be rejected")
	}
	if err := svc.DeletePricing("p", "m", "admin"); err == nil {
		t.Fatal("delete missing should 404")
	}
	if _, err := svc.SavePricing(PricingInput{Provider: "p", Model: "m", PriceInPerM: "1.5", PriceOutPerM: "2"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeletePricing("p", "m", "admin"); err != nil {
		t.Fatal(err)
	}
	audits, err := svc.Audits(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 {
		t.Fatalf("audits = %d, want 2 (save+delete)", len(audits))
	}
	for _, a := range audits {
		if a.ObjectType != "pricing" {
			t.Fatalf("audit object_type = %s", a.ObjectType)
		}
	}
}

func TestCostsTrendValidation(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)
	if _, err := svc.CostsTrend("2026-01-01", "bad-day"); err == nil {
		t.Fatal("invalid date should be rejected")
	}
	if _, err := svc.CostsTrend("2026-02-01", "2026-01-01"); err == nil {
		t.Fatal("to<from should be rejected")
	}
	if _, err := svc.CostsTrend("2025-01-01", "2026-12-31"); err == nil {
		t.Fatal(">92 days should be rejected")
	}
}
