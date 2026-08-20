package mlops

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// fakeNotifier 预算通知捕获器。
type fakeNotifier struct {
	ids    []string
	titles []string
	bodies []string
}

func (f *fakeNotifier) EnabledChannelIDs() []string { return []string{"ch1"} }
func (f *fakeNotifier) Send(_ context.Context, ids []string, title, content string) error {
	f.ids = append(f.ids, ids...)
	f.titles = append(f.titles, title)
	f.bodies = append(f.bodies, content)
	return nil
}

// TestBudgetTierNotifyOnce 预算档位：80%/100% 各通知一次，重复入账与
// 重跑不重复通知；通知内容含月份/金额/预算/档位/未计价数。
func TestBudgetTierNotifyOnce(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)
	fn := &fakeNotifier{}
	svc.SetNotifier(fn)

	month := time.Now().Format("2006-01")
	// 预算 1 元（1_000_000 微元），warn_at 0.5
	if _, err := svc.SaveBudget(month, 1_000_000, 0.5, "admin"); err != nil {
		t.Fatal(err)
	}

	// 计价入账：单价 1 元/M token，200 万 token = 2 元 → 200%（触发 50%+100%）
	if _, err := svc.SavePricing(PricingInput{
		Provider: "p", Model: "m", PriceInPerM: "1", PriceOutPerM: "0",
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	svc.StartUsage(UsageSettings{QueueSize: 8})
	defer svc.StopUsage()

	budgetLastCheck.Store(0) // 重置限频，让入账立即触发对账
	now := time.Now()
	svc.Record(usage.Record{
		OperationID: "op-b1", CallID: "call-b1",
		Provider: "p", Model: "m", Scenario: "chat",
		PromptTokens: 2_000_000, TotalTokens: 2_000_000, UsagePresent: true,
		OK: true, Status: "success", StartedAt: now, FinishedAt: now,
	})
	if !waitFor(t, 3*time.Second, func() bool { return len(fn.titles) >= 2 }) {
		t.Fatalf("expected 2 tier notifications, got %v", fn.titles)
	}
	if len(fn.titles) != 2 {
		t.Fatalf("titles = %v", fn.titles)
	}
	if !strings.Contains(fn.titles[0], "50%") || !strings.Contains(fn.titles[1], "100%") {
		t.Fatalf("tier order wrong: %v", fn.titles)
	}
	for _, body := range fn.bodies {
		for _, want := range []string{month, "2", "预算上限", "未计价"} {
			if !strings.Contains(body, want) {
				t.Fatalf("notify body missing %q: %s", want, body)
			}
		}
	}

	// 再次入账（限频窗口后）不重复通知
	budgetLastCheck.Store(0)
	svc.Record(usage.Record{
		OperationID: "op-b2", CallID: "call-b2",
		Provider: "p", Model: "m", Scenario: "chat",
		PromptTokens: 100, UsagePresent: true,
		OK: true, Status: "success", StartedAt: now, FinishedAt: now,
	})
	waitFor(t, 2*time.Second, func() bool {
		_, total, _, _ := st.ListMLUsage(store.MLUsageFilter{})
		return total == 2
	})
	if len(fn.titles) != 2 {
		t.Fatalf("tier notifications repeated: %v", fn.titles)
	}

	// 视图：使用率 >1，notified 含两档
	views, err := svc.ListBudgets()
	if err != nil || len(views) != 1 {
		t.Fatalf("budget views: %v %v", views, err)
	}
	if views[0].SpendMinor < 2_000_000 {
		t.Fatalf("spend = %d", views[0].SpendMinor)
	}
	if len(views[0].Notified) != 2 {
		t.Fatalf("notified = %v", views[0].Notified)
	}
}

func TestBudgetValidation(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)

	cases := []struct {
		month      string
		limitMinor int64
		warnAt     float64
	}{
		{"2026-8", 100, 0.8},      // 月份格式
		{"2026-13", 100, 0.8},     // 非法月份
		{"2026-08", 0, 0.8},       // limit ≤ 0
		{"2026-08", -1, 0.8},      // 负数
		{"2026-08", 100, 0},       // warn 越界
		{"2026-08", 100, 1},       // warn 越界
		{"2026-08", 1 << 60, 0.8}, // 超上限
	}
	for i, c := range cases {
		if _, err := svc.SaveBudget(c.month, c.limitMinor, c.warnAt, "admin"); err == nil {
			t.Fatalf("case %d should be rejected: %+v", i, c)
		}
	}
	if _, err := svc.SaveBudget("2026-08", 500_000, 0.8, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteBudget("2099-01", "admin"); err == nil {
		t.Fatal("delete missing should 404")
	}
	if err := svc.DeleteBudget("2026-08", "admin"); err != nil {
		t.Fatal(err)
	}
}

func TestBindingValidationAndAudit(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)

	if _, err := svc.SaveBinding("compress", "m", "admin"); err == nil {
		t.Fatal("internal scenario should not be bindable")
	}
	if _, err := svc.SaveBinding("chat", "", "admin"); err == nil {
		t.Fatal("empty model should be rejected")
	}
	b, err := svc.SaveBinding("chat", "gpt-4o", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := svc.ScenarioModel("chat"); !ok || m != "gpt-4o" {
		t.Fatalf("ScenarioModel = %q %v", m, ok)
	}
	_ = b
	if err := svc.DeleteBinding("chat", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteBinding("chat", "admin"); err == nil {
		t.Fatal("delete missing should 404")
	}
	audits, _ := svc.Audits(10)
	if len(audits) != 2 {
		t.Fatalf("audits = %d, want 2", len(audits))
	}
}

func TestHealthCache(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st)

	svc.RecordHealth(HealthResult{Provider: "p", Model: "m1", OK: true, LatencyMs: 12})
	svc.RecordHealth(HealthResult{Provider: "p", Model: "m2", OK: false, Error: fmt.Sprint("boom")})
	items := svc.HealthResults()
	if len(items) != 2 || !items[0].OK || items[1].OK {
		t.Fatalf("health cache = %+v", items)
	}
	// 覆盖同模型旧结果
	svc.RecordHealth(HealthResult{Provider: "p", Model: "m1", OK: false, Error: "down"})
	items = svc.HealthResults()
	if len(items) != 2 {
		t.Fatalf("health cache should keep 2 entries: %+v", items)
	}
	for _, r := range items {
		if r.Model == "m1" && r.OK {
			t.Fatal("m1 should be overwritten to failed")
		}
	}
}
