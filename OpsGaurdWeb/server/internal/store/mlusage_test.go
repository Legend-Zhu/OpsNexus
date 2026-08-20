package store

import (
	"testing"
	"time"
)

func mkUsage(callID, provider, model, scenario, status string, prompt, completion int, started time.Time) *MLUsageRecord {
	return &MLUsageRecord{
		OperationID:      "op-" + callID,
		CallID:           callID,
		Provider:         provider,
		Model:            model,
		Scenario:         scenario,
		Status:           status,
		OK:               status == "success",
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		UsagePresent:     true,
		Priced:           true,
		CostMinor:        100,
		StartedAt:        started,
		FinishedAt:       started.Add(time.Second),
		LatencyMs:        1000,
		Day:              started.Format("2006-01-02"),
	}
}

func TestRecordMLUsageAggregatesAndIdempotent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	day := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	for _, rec := range []*MLUsageRecord{
		mkUsage("c1", "openai", "gpt-4o", "chat", "success", 100, 50, day),
		mkUsage("c2", "openai", "gpt-4o", "chat", "provider_error", 0, 0, day.Add(time.Minute)),
		mkUsage("c3", "openai", "gpt-4o", "investigate", "success", 200, 80, day.Add(2*time.Minute)),
	} {
		rec.UsagePresent = rec.CallID != "c2"
		if !rec.UsagePresent {
			rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens = 0, 0, 0
			rec.Priced = false
			rec.CostMinor = 0
		}
		if _, err := s.RecordMLUsage(rec, true); err != nil {
			t.Fatalf("record %s: %v", rec.CallID, err)
		}
	}

	// 重复 call_id：幂等跳过，不新增明细/聚合
	dup := mkUsage("c1", "openai", "gpt-4o", "chat", "success", 999, 999, day)
	saved, err := s.RecordMLUsage(dup, true)
	if err != nil {
		t.Fatal(err)
	}
	if saved {
		t.Fatal("duplicate call_id should be skipped")
	}

	// 日聚合：chat 2 次（1 成功 1 失败，1 次 usage 缺失），investigate 1 次
	days, err := s.ListMLUsageDays("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("want 2 day rows, got %d", len(days))
	}
	var chatRow, invRow *MLUsageDay
	for _, d := range days {
		switch d.Scenario {
		case "chat":
			chatRow = d
		case "investigate":
			invRow = d
		}
	}
	if chatRow == nil || invRow == nil {
		t.Fatal("missing day rows")
	}
	if chatRow.Calls != 2 || chatRow.SuccessCalls != 1 || chatRow.ErrorCalls != 1 {
		t.Fatalf("chat row calls=%d success=%d error=%d", chatRow.Calls, chatRow.SuccessCalls, chatRow.ErrorCalls)
	}
	if chatRow.UnmeteredCalls != 1 {
		t.Fatalf("chat unmetered=%d, want 1", chatRow.UnmeteredCalls)
	}
	if chatRow.PromptTokens != 100 || chatRow.CompletionTokens != 50 {
		t.Fatalf("chat tokens p=%d c=%d, want 100/50", chatRow.PromptTokens, chatRow.CompletionTokens)
	}
	if chatRow.PricedCalls != 1 || chatRow.CostMinor != 100 {
		t.Fatalf("chat priced=%d cost=%d, want 1/100", chatRow.PricedCalls, chatRow.CostMinor)
	}
	if chatRow.Operations != 2 {
		t.Fatalf("chat operations=%d, want 2", chatRow.Operations)
	}
	if invRow.Calls != 1 || invRow.TotalTokens != 280 {
		t.Fatalf("investigate row = %+v", invRow)
	}

	// 明细：过滤 + 最新在前分页
	items, total, _, err := s.ListMLUsage(MLUsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(items) != 3 || items[0].CallID != "c3" {
		t.Fatalf("list total=%d items=%d first=%s", total, len(items), items[0].CallID)
	}
	items, _, _, err = s.ListMLUsage(MLUsageFilter{Scenario: "chat", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].CallID != "c1" {
		t.Fatalf("paged chat items = %+v", items)
	}
	// 游标：before_seq = 次新 chat 记录（c2, seq=2）→ 只剩 c1
	all, _, _, err := s.ListMLUsage(MLUsageFilter{Scenario: "chat"})
	if err != nil || len(all) != 2 {
		t.Fatalf("chat all = %v err=%v", all, err)
	}
	items, _, _, err = s.ListMLUsage(MLUsageFilter{Scenario: "chat", BeforeSeq: all[0].Seq})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].CallID != "c1" {
		t.Fatalf("before_seq cursor left %+v, want c1", items)
	}

	// operation 下钻
	opItems, err := s.ListMLUsageByOperation("op-c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(opItems) != 1 || opItems[0].CallID != "c1" {
		t.Fatalf("operation items = %+v", opItems)
	}
}

// 模型名含 '/' 不得破坏日聚合 key 结构（encKeyPart 转义）。
func TestMLUsageDayKeyWithSlashInModel(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	day := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	if _, err := s.RecordMLUsage(mkUsage("c1", "p1", "openai/gpt-4", "chat", "success", 10, 5, day), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMLUsage(mkUsage("c2", "p1", "openai", "chat", "success", 7, 3, day), true); err != nil {
		t.Fatal(err)
	}
	days, err := s.ListMLUsageDays("2026-08-20", "2026-08-20")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("want 2 rows (distinct models), got %d", len(days))
	}
	models := map[string]bool{}
	for _, d := range days {
		models[d.Model] = true
	}
	if !models["openai/gpt-4"] || !models["openai"] {
		t.Fatalf("models = %v", models)
	}
}

func TestGCMLUsageBeforeKeepsDayAggregates(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	old := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	if _, err := s.RecordMLUsage(mkUsage("old1", "p", "m", "chat", "success", 1, 1, old), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMLUsage(mkUsage("new1", "p", "m", "chat", "success", 2, 2, recent), true); err != nil {
		t.Fatal(err)
	}

	n, err := s.GCMLUsageBefore(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), 500)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("gc deleted %d, want 1", n)
	}
	items, total, _, err := s.ListMLUsage(MLUsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || items[0].CallID != "new1" {
		t.Fatalf("after gc total=%d first=%s", total, items[0].CallID)
	}
	// callidx 一并清理：同 call_id 再入账应视为新记录
	saved, err := s.RecordMLUsage(mkUsage("old1", "p", "m", "chat", "success", 1, 1, recent), true)
	if err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatal("callidx should be cleaned with detail")
	}
	// 日聚合不受 GC 影响（5 月行保留）
	days, err := s.ListMLUsageDays("2026-05-01", "2026-05-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Calls != 1 {
		t.Fatalf("day aggregate should survive gc: %+v", days)
	}
}

func TestMLPricingCRUD(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if p, err := s.GetMLPricing("openai", "gpt-4o"); err != nil || p != nil {
		t.Fatalf("missing pricing should return nil,nil: %v %v", p, err)
	}
	if err := s.SaveMLPricing(&MLPricing{
		Provider: "openai", Model: "gpt-4o", Currency: "CNY",
		PriceInPerMMicro: 2_500_000, PriceOutPerMMicro: 10_000_000,
		Version: "pv-1", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 模型名含 '/'：key 转义后不冲突
	if err := s.SaveMLPricing(&MLPricing{
		Provider: "openai", Model: "gpt-4o/mini", Currency: "CNY",
		PriceInPerMMicro: 1, Version: "pv-2", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetMLPricing("openai", "gpt-4o")
	if err != nil || p == nil || p.PriceInPerMMicro != 2_500_000 {
		t.Fatalf("get pricing: %v %v", p, err)
	}
	list, err := s.ListMLPricings()
	if err != nil || len(list) != 2 {
		t.Fatalf("list pricings: %v %v", list, err)
	}
	if err := s.DeleteMLPricing("openai", "gpt-4o"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetMLPricing("openai", "gpt-4o"); p != nil {
		t.Fatal("pricing should be deleted")
	}
	if p, _ := s.GetMLPricing("openai", "gpt-4o/mini"); p == nil {
		t.Fatal("slash model pricing must be intact")
	}
}
